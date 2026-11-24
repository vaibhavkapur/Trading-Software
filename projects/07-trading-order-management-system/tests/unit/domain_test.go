// Package unit tests the pure parts of the OMS: the state machine and its
// decision functions, fixed-point money, webhook normalization, access
// rules, risk limits, retry backoff and metrics rendering. None of these
// tests needs a database or a network.
package unit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
	"time"

	"oms/src/domain"
	"oms/src/fixed"
)

var t0 = time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)

func price(s string) *fixed.Micros {
	p := fixed.MustParse(s)
	return &p
}

// book is an order with its execution records, kept in memory. It stands in
// for the store so that the decision functions can be driven without one.
type book struct {
	t      testing.TB
	order  domain.Order
	events []domain.Event
	execs  map[string]domain.Execution // booked records by execution id
}

func newBook(t testing.TB, qty int64) *book {
	b := &book{t: t, execs: map[string]domain.Execution{}}
	b.mustApply(domain.EvOrderCreated, domain.OrderCreated{
		AccountID: "ACC-1", ClientOrderID: "c-1", Symbol: "AAPL", Side: domain.Buy, Type: domain.Limit,
		TimeInForce: domain.Day, Qty: qty, LimitPrice: price("150"), CreatedBy: "alice",
	})
	return b
}

func event(o domain.Order, typ domain.EventType, payload any) domain.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return domain.Event{OrderID: "ord-1", Version: o.Version + 1, Type: typ, Payload: raw, OccurredAt: t0, RecordedAt: t0}
}

// apply folds one event in, or returns the state machine's refusal.
func (b *book) apply(typ domain.EventType, payload any) error {
	e := event(b.order, typ, payload)
	next, err := domain.Apply(b.order, e)
	if err != nil {
		return err
	}
	b.order = next
	b.events = append(b.events, e)
	if x, ok := payload.(domain.Execution); ok {
		b.execs[x.BrokerExecID] = x
	}
	return nil
}

func (b *book) mustApply(typ domain.EventType, payload any) {
	b.t.Helper()
	if err := b.apply(typ, payload); err != nil {
		b.t.Fatalf("apply %s: %v", typ, err)
	}
}

// to walks a new order to the given state along the plain path.
func to(t testing.TB, state domain.State) *book {
	t.Helper()
	b := newBook(t, 100)
	fill := func(id string, qty int64) domain.Execution {
		n, _ := fixed.MulQty(fixed.MustParse("150"), qty)
		return domain.Execution{BrokerExecID: id, Kind: domain.ExecFill, Qty: qty, Price: fixed.MustParse("150"),
			ExecutedAt: t0, DeltaQty: qty, DeltaNotional: n}
	}
	steps := []func(){
		func() { b.mustApply(domain.EvOrderValidated, domain.OrderValidated{Authorizer: "test"}) },
		func() { b.mustApply(domain.EvSubmissionStarted, domain.SubmissionStarted{OutboxID: 1}) },
		func() { b.mustApply(domain.EvOrderAcknowledged, domain.OrderAcknowledged{BrokerOrderID: "B-1"}) },
		func() { b.mustApply(domain.EvExecutionRecorded, fill("X-1", 40)) },
		func() { b.mustApply(domain.EvExecutionRecorded, fill("X-2", 60)) },
	}
	switch state {
	case domain.StateNew:
		return b
	case domain.StateRejected:
		b.mustApply(domain.EvOrderRejected, domain.OrderRejected{By: domain.ByRisk, Reason: "no"})
		return b
	case domain.StateCancelled, domain.StateExpired:
		for _, step := range steps[:3] {
			step()
		}
		if state == domain.StateCancelled {
			b.mustApply(domain.EvOrderCancelled, domain.OrderCancelled{By: domain.ByBroker})
		} else {
			b.mustApply(domain.EvOrderExpired, domain.OrderExpired{})
		}
		return b
	}
	for i, s := range []domain.State{domain.StateValidated, domain.StateSubmitting, domain.StateAcknowledged,
		domain.StatePartiallyFilled, domain.StateFilled} {
		steps[i]()
		if s == state {
			return b
		}
	}
	t.Fatalf("no path to %s", state)
	return nil
}

// context computes what the booked executions say about an incoming report,
// the way the store does.
func (b *book) context(r domain.BrokerReport) (x domain.ExecContext, duplicate bool) {
	if _, ok := b.execs[r.ExecID]; ok {
		return x, true
	}
	for id, booked := range b.execs {
		if booked.RefBrokerExecID == r.ExecID {
			x.Superseded = true
		}
		if r.RefExecID != "" && booked.RefBrokerExecID == r.RefExecID {
			x.RefCorrectedBy = id
		}
	}
	if ref, ok := b.execs[r.RefExecID]; ok && x.RefCorrectedBy == "" && ref.Kind != domain.ExecBust {
		x.Ref = &domain.ExecValues{Qty: ref.Qty, Price: ref.Price, Fee: ref.Fee}
	}
	return x, false
}

// report runs a broker report through the decision function and applies
// whatever it decides.
func (b *book) report(r domain.BrokerReport) domain.Outcome {
	b.t.Helper()
	r.OrderID = "ord-1"
	if r.EventTime.IsZero() {
		r.EventTime = t0
	}
	var x domain.ExecContext
	if r.Kind.IsExecution() {
		var dup bool
		if x, dup = b.context(r); dup {
			return domain.OutcomeDuplicate
		}
	}
	d := domain.DecideReport(b.order, r, x, domain.SourceBroker)
	for _, draft := range d.Drafts {
		if err := b.apply(draft.Type, draft.Payload); err != nil {
			b.t.Fatalf("the decision for %s on a %s order was refused by the state machine: %v", r.Kind, b.order.State, err)
		}
	}
	return d.Outcome
}

func fillReport(id string, qty int64, px string) domain.BrokerReport {
	return domain.BrokerReport{Kind: domain.ReportFill, ExecID: id, Qty: qty, Price: fixed.MustParse(px), Fee: fixed.MustParse("0.01")}
}

func correctReport(id, ref string, qty int64, px string) domain.BrokerReport {
	return domain.BrokerReport{Kind: domain.ReportCorrect, ExecID: id, RefExecID: ref, Qty: qty, Price: fixed.MustParse(px), Fee: fixed.MustParse("0.01")}
}

func bustReport(id, ref string) domain.BrokerReport {
	return domain.BrokerReport{Kind: domain.ReportBust, ExecID: id, RefExecID: ref}
}

// --- The transition table ---

// samplePayloads gives, for each event type, payloads that could be valid in
// some state.
func samplePayloads() map[domain.EventType][]any {
	exec := func(id string, kind domain.ExecKind, ref string, qty, delta int64) domain.Execution {
		n, _ := fixed.MulQty(fixed.MustParse("150"), delta)
		return domain.Execution{BrokerExecID: id, Kind: kind, RefBrokerExecID: ref, Qty: qty,
			Price: fixed.MustParse("150"), ExecutedAt: t0, DeltaQty: delta, DeltaNotional: n}
	}
	return map[domain.EventType][]any{
		domain.EvOrderCreated:      {domain.OrderCreated{AccountID: "A", ClientOrderID: "c", Symbol: "S", Side: domain.Buy, Type: domain.Limit, TimeInForce: domain.Day, Qty: 100, LimitPrice: price("1")}},
		domain.EvOrderValidated:    {domain.OrderValidated{}},
		domain.EvOrderRejected:     {domain.OrderRejected{By: domain.ByBroker}},
		domain.EvSubmissionStarted: {domain.SubmissionStarted{}},
		domain.EvOrderAcknowledged: {domain.OrderAcknowledged{}},
		domain.EvCancelRequested:   {domain.CancelRequested{}},
		domain.EvCancelRejected:    {domain.CancelRejected{}},
		domain.EvOrderCancelled:    {domain.OrderCancelled{}},
		domain.EvReplaceRequested:  {domain.ReplaceRequested{Replace: domain.Replace{Qty: 120, LimitPrice: price("151")}}},
		domain.EvReplaceRejected:   {domain.ReplaceRejected{}},
		domain.EvOrderReplaced: {
			domain.OrderReplaced{Replace: domain.Replace{Qty: 120, LimitPrice: price("151")}},
			domain.OrderReplaced{Replace: domain.Replace{Qty: 120, LimitPrice: price("151")}, Local: true},
		},
		domain.EvOrderExpired: {domain.OrderExpired{}},
		domain.EvExecutionRecorded: {
			exec("N-1", domain.ExecFill, "", 10, 10), exec("N-2", domain.ExecFill, "", 60, 60),
			exec("N-3", domain.ExecFill, "", 100, 100), exec("N-4", domain.ExecFill, "", 10, 0),
		},
		domain.EvExecutionCorrected: {
			exec("N-5", domain.ExecCorrect, "X-1", 30, -10), exec("N-6", domain.ExecCorrect, "X-1", 50, 10),
			exec("N-7", domain.ExecCorrect, "X-1", 100, 60), exec("N-14", domain.ExecCorrect, "X-9", 100, 100),
			// A correction that was itself superseded before it arrived
			// contributes nothing of its own and only removes what it replaces.
			exec("N-15", domain.ExecCorrect, "X-1", 30, -40), exec("N-16", domain.ExecCorrect, "X-9", 30, -100),
		},
		domain.EvExecutionBusted: {
			exec("N-8", domain.ExecBust, "X-1", 0, -40), exec("N-9", domain.ExecBust, "X-2", 0, -60),
			exec("N-10", domain.ExecBust, "X-9", 0, -100), exec("N-11", domain.ExecBust, "X-9", 0, 0),
		},
		domain.EvExecutionAccepted: {exec("N-12", domain.ExecFill, "", 70, 70), exec("N-13", domain.ExecFill, "", 200, 200)},
	}
}

// Every state change Apply performs is listed in the transition table, and
// every entry of the table is actually reachable. Together: the table is
// the state machine, no more and no less.
func TestTransitions_ApplyAndTheTableAgreeExactly(t *testing.T) {
	type triple struct {
		from, to domain.State
		typ      domain.EventType
	}
	seen := map[triple]bool{}
	payloads := samplePayloads()

	starts := map[domain.State][]*book{"": {{t: t, execs: map[string]domain.Execution{}}}}
	for _, s := range domain.States {
		starts[s] = append(starts[s], to(t, s))
	}
	// Variants that make pending-dependent events applicable.
	for _, s := range []domain.State{domain.StateSubmitting, domain.StateAcknowledged, domain.StatePartiallyFilled} {
		pc := to(t, s)
		pc.mustApply(domain.EvCancelRequested, domain.CancelRequested{})
		pr := to(t, s)
		pr.mustApply(domain.EvReplaceRequested, domain.ReplaceRequested{Replace: domain.Replace{Qty: 120, LimitPrice: price("151")}})
		starts[s] = append(starts[s], pc, pr)
	}

	for from, books := range starts {
		for _, b := range books {
			for typ, samples := range payloads {
				for _, payload := range samples {
					next, err := domain.Apply(b.order, event(b.order, typ, payload))
					if err != nil {
						if !errors.Is(err, domain.ErrInvalidEvent) {
							t.Errorf("%s on %s: error is not ErrInvalidEvent: %v", typ, from, err)
						}
						continue
					}
					if next.State != from {
						seen[triple{from, next.State, typ}] = true
					}
					if !domain.TransitionAllowed(from, next.State, typ) {
						t.Errorf("Apply moved %s → %s on %s, which the table does not allow", from, next.State, typ)
					}
					if domain.Backward(from, next.State) && !typ.IsCorrection() {
						t.Errorf("%s → %s on %s is a backward move without a correction event", from, next.State, typ)
					}
					if err := domain.CheckInvariants(next); err != nil {
						t.Errorf("%s on %s broke an invariant: %v", typ, from, err)
					}
				}
			}
		}
	}

	for from, tos := range domain.Transitions {
		for to, types := range tos {
			for _, typ := range types {
				if !seen[triple{from, to, typ}] {
					t.Errorf("table entry %q → %s on %s was never produced by Apply", from, to, typ)
				}
				delete(seen, triple{from, to, typ})
			}
		}
	}
	for extra := range seen {
		t.Errorf("Apply produced %q → %s on %s, which is not in the table", extra.from, extra.to, extra.typ)
	}
}

func TestTransitions_ShapeOfTheTable(t *testing.T) {
	for _, s := range domain.States {
		if _, ok := domain.Transitions[s]; !ok {
			t.Errorf("state %s is missing from the table", s)
		}
	}
	// CANCELLED, REJECTED and EXPIRED are final. FILLED can only be left by
	// an execution correction.
	for _, s := range []domain.State{domain.StateCancelled, domain.StateRejected, domain.StateExpired} {
		if n := len(domain.Transitions[s]); n != 0 {
			t.Errorf("%s has %d outgoing transitions", s, n)
		}
	}
	// Non-goal: no backward transition without a documented correction.
	backward := 0
	for from, tos := range domain.Transitions {
		for to, types := range tos {
			if !domain.Backward(from, to) {
				continue
			}
			backward++
			for _, typ := range types {
				if typ != domain.EvExecutionCorrected && typ != domain.EvExecutionBusted {
					t.Errorf("backward transition %s → %s is allowed on %s", from, to, typ)
				}
			}
		}
	}
	if backward != 3 {
		t.Errorf("%d backward transitions in the table, want 3", backward)
	}
	// Nothing leads back to NEW or VALIDATED, and nothing skips validation.
	for from, tos := range domain.Transitions {
		for to := range tos {
			if (to == domain.StateNew && from != "") || (to == domain.StateValidated && from != domain.StateNew) {
				t.Errorf("%s → %s should not exist", from, to)
			}
			if to == domain.StateSubmitting && from != domain.StateValidated {
				t.Errorf("%s → SUBMITTING skips validation", from)
			}
		}
	}
}

func TestApply_RefusesMalformedHistory(t *testing.T) {
	b := to(t, domain.StateAcknowledged)
	cases := map[string]domain.Event{
		"version gap":     {OrderID: "ord-1", Version: b.order.Version + 2, Type: domain.EvOrderCancelled, Payload: []byte(`{}`)},
		"version replay":  {OrderID: "ord-1", Version: b.order.Version, Type: domain.EvOrderCancelled, Payload: []byte(`{}`)},
		"another order":   {OrderID: "ord-2", Version: b.order.Version + 1, Type: domain.EvOrderCancelled, Payload: []byte(`{}`)},
		"created twice":   event(b.order, domain.EvOrderCreated, samplePayloads()[domain.EvOrderCreated][0]),
		"unknown type":    event(b.order, "OrderTeleported", struct{}{}),
		"bad payload":     {OrderID: "ord-1", Version: b.order.Version + 1, Type: domain.EvOrderAcknowledged, Payload: []byte(`[1,2]`)},
		"second validate": event(b.order, domain.EvOrderValidated, domain.OrderValidated{}),
	}
	for name, e := range cases {
		if _, err := domain.Apply(b.order, e); !errors.Is(err, domain.ErrInvalidEvent) {
			t.Errorf("%s: err = %v, want ErrInvalidEvent", name, err)
		}
	}
	if _, err := domain.Apply(domain.Order{}, event(domain.Order{}, domain.EvOrderValidated, domain.OrderValidated{})); err == nil {
		t.Error("a history may start with something other than OrderCreated")
	}
	// Apply never mutates its input.
	before := b.order
	domain.Apply(b.order, event(b.order, domain.EvCancelRequested, domain.CancelRequested{}))
	if !reflect.DeepEqual(before, b.order) {
		t.Error("Apply mutated the order it was given")
	}
}

func TestApply_OverfillNeedsAnExplicitAcceptance(t *testing.T) {
	b := to(t, domain.StatePartiallyFilled) // 40 of 100
	over := domain.Execution{BrokerExecID: "X-over", Kind: domain.ExecFill, Qty: 70, Price: fixed.MustParse("150"),
		ExecutedAt: t0, DeltaQty: 70, DeltaNotional: fixed.MustParse("10500")}

	for _, typ := range []domain.EventType{domain.EvExecutionRecorded, domain.EvExecutionCorrected} {
		if err := b.apply(typ, over); err == nil {
			t.Fatalf("%s took filled quantity above order quantity", typ)
		}
	}
	b.mustApply(domain.EvExecutionAccepted, over)
	if b.order.FilledQty != 110 || !b.order.OverfillAccepted || b.order.State != domain.StateFilled {
		t.Fatalf("after acceptance: %+v", b.order)
	}
	// Acceptance is not a licence for more: another fill is still refused,
	// while a correction that lowers the total is fine.
	more := over
	more.BrokerExecID, more.DeltaQty, more.Qty = "X-more", 1, 1
	if err := b.apply(domain.EvExecutionRecorded, more); err == nil {
		t.Fatal("a further overfill was applied without acceptance")
	}
	down := domain.Execution{BrokerExecID: "X-down", Kind: domain.ExecCorrect, RefBrokerExecID: "X-over", Qty: 65,
		Price: fixed.MustParse("150"), ExecutedAt: t0, DeltaQty: -5, DeltaNotional: fixed.MustParse("-750")}
	b.mustApply(domain.EvExecutionCorrected, down)
	if b.order.FilledQty != 105 {
		t.Fatalf("filled = %d", b.order.FilledQty)
	}
}

// --- Decisions ---

func TestDecideReport_Table(t *testing.T) {
	type tc struct {
		state  domain.State
		report domain.BrokerReport
		want   domain.Outcome
		after  domain.State
	}
	ack := domain.BrokerReport{Kind: domain.ReportAck, BrokerOrderID: "B-1"}
	rej := domain.BrokerReport{Kind: domain.ReportReject, Reason: "no"}
	can := domain.BrokerReport{Kind: domain.ReportCancelled}
	exp := domain.BrokerReport{Kind: domain.ReportExpired}
	cases := []tc{
		{domain.StateSubmitting, ack, domain.OutcomeApplied, domain.StateAcknowledged},
		{domain.StateAcknowledged, ack, domain.OutcomeStale, domain.StateAcknowledged},
		{domain.StateFilled, ack, domain.OutcomeStale, domain.StateFilled},
		{domain.StateCancelled, ack, domain.OutcomeStale, domain.StateCancelled},
		{domain.StateValidated, ack, domain.OutcomeQuarantined, domain.StateValidated}, // never sent
		{domain.StateNew, fillReport("N", 1, "1"), domain.OutcomeQuarantined, domain.StateNew},

		{domain.StateSubmitting, rej, domain.OutcomeApplied, domain.StateRejected},
		{domain.StateAcknowledged, rej, domain.OutcomeApplied, domain.StateRejected},
		{domain.StateRejected, rej, domain.OutcomeQuarantined, domain.StateRejected}, // rejected by risk, never sent
		{domain.StatePartiallyFilled, rej, domain.OutcomeQuarantined, domain.StatePartiallyFilled},
		{domain.StateFilled, rej, domain.OutcomeQuarantined, domain.StateFilled},

		{domain.StateSubmitting, can, domain.OutcomeApplied, domain.StateCancelled},
		{domain.StatePartiallyFilled, can, domain.OutcomeApplied, domain.StateCancelled},
		{domain.StateCancelled, can, domain.OutcomeStale, domain.StateCancelled},
		{domain.StateFilled, can, domain.OutcomeQuarantined, domain.StateFilled},
		{domain.StateExpired, can, domain.OutcomeQuarantined, domain.StateExpired},

		{domain.StateAcknowledged, exp, domain.OutcomeApplied, domain.StateExpired},
		{domain.StateExpired, exp, domain.OutcomeStale, domain.StateExpired},
		{domain.StateCancelled, exp, domain.OutcomeQuarantined, domain.StateCancelled},

		{domain.StateAcknowledged, domain.BrokerReport{Kind: domain.ReportCancelRejected}, domain.OutcomeStale, domain.StateAcknowledged},
		{domain.StateAcknowledged, domain.BrokerReport{Kind: domain.ReportReplaceReject}, domain.OutcomeStale, domain.StateAcknowledged},
		{domain.StateAcknowledged, domain.BrokerReport{Kind: domain.ReportReplaced, OrderQty: 120, LimitPrice: price("1")}, domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateFilled, domain.BrokerReport{Kind: domain.ReportReplaced, OrderQty: 120, LimitPrice: price("1")}, domain.OutcomeStale, domain.StateFilled},
		{domain.StateAcknowledged, domain.BrokerReport{Kind: "TELEPORTED"}, domain.OutcomeQuarantined, domain.StateAcknowledged},

		{domain.StateSubmitting, fillReport("N", 30, "150"), domain.OutcomeApplied, domain.StatePartiallyFilled}, // fill beats the ack
		{domain.StateSubmitting, fillReport("N", 100, "150"), domain.OutcomeApplied, domain.StateFilled},
		{domain.StateAcknowledged, fillReport("N", 100, "150"), domain.OutcomeApplied, domain.StateFilled},
		{domain.StateAcknowledged, fillReport("N", 101, "150"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StatePartiallyFilled, fillReport("N", 60, "150"), domain.OutcomeApplied, domain.StateFilled},
		{domain.StatePartiallyFilled, fillReport("N", 61, "150"), domain.OutcomeQuarantined, domain.StatePartiallyFilled},
		{domain.StateFilled, fillReport("N", 1, "150"), domain.OutcomeQuarantined, domain.StateFilled},
		{domain.StateCancelled, fillReport("N", 30, "150"), domain.OutcomeApplied, domain.StateCancelled}, // late fill
		{domain.StateExpired, fillReport("N", 30, "150"), domain.OutcomeApplied, domain.StateExpired},
		{domain.StateRejected, fillReport("N", 30, "150"), domain.OutcomeQuarantined, domain.StateRejected},

		{domain.StateFilled, correctReport("N", "X-2", 50, "150"), domain.OutcomeApplied, domain.StatePartiallyFilled},
		{domain.StateFilled, bustReport("N", "X-2"), domain.OutcomeApplied, domain.StatePartiallyFilled},
		{domain.StatePartiallyFilled, bustReport("N", "X-1"), domain.OutcomeApplied, domain.StateAcknowledged},
		{domain.StatePartiallyFilled, correctReport("N", "X-1", 100, "150"), domain.OutcomeApplied, domain.StateFilled},
		{domain.StatePartiallyFilled, correctReport("N", "X-1", 101, "150"), domain.OutcomeQuarantined, domain.StatePartiallyFilled},

		// Malformed execution reports.
		{domain.StateAcknowledged, domain.BrokerReport{Kind: domain.ReportFill, Qty: 1, Price: 1}, domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, fillReport("N", 0, "150"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, fillReport("N", -5, "150"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, fillReport("N", 5, "0"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, correctReport("N", "", 5, "150"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, correctReport("N", "N", 5, "150"), domain.OutcomeQuarantined, domain.StateAcknowledged},
		{domain.StateAcknowledged, domain.BrokerReport{Kind: domain.ReportFill, ExecID: "N", RefExecID: "X-1", Qty: 1, Price: 1}, domain.OutcomeQuarantined, domain.StateAcknowledged},
	}
	for i, c := range cases {
		b := to(t, c.state)
		before := b.order
		got := b.report(c.report)
		if got != c.want || b.order.State != c.after {
			t.Errorf("case %d: %s on %s: outcome %s state %s, want %s %s", i, c.report.Kind, c.state, got, b.order.State, c.want, c.after)
		}
		if got != domain.OutcomeApplied && !reflect.DeepEqual(before, b.order) {
			t.Errorf("case %d: a %s report changed the order", i, got)
		}
	}
}

func TestDecideReport_PendingCancelAndReplace(t *testing.T) {
	b := to(t, domain.StateAcknowledged)
	b.mustApply(domain.EvCancelRequested, domain.CancelRequested{})
	if got := b.report(domain.BrokerReport{Kind: domain.ReportCancelRejected, Reason: "late"}); got != domain.OutcomeApplied || b.order.PendingCancel {
		t.Fatalf("cancel reject: %s, pending %v", got, b.order.PendingCancel)
	}

	b.mustApply(domain.EvReplaceRequested, domain.ReplaceRequested{Replace: domain.Replace{Qty: 120, LimitPrice: price("151")}})
	// The broker must confirm exactly what was asked.
	if got := b.report(domain.BrokerReport{Kind: domain.ReportReplaced, OrderQty: 130, LimitPrice: price("151")}); got != domain.OutcomeQuarantined {
		t.Fatalf("confirmation of different terms: %s", got)
	}
	if got := b.report(domain.BrokerReport{Kind: domain.ReportReplaced, OrderQty: 120, LimitPrice: price("151")}); got != domain.OutcomeApplied {
		t.Fatalf("replace confirmation: %s", got)
	}
	if b.order.Qty != 120 || *b.order.LimitPrice != fixed.MustParse("151") || b.order.PendingReplace != nil {
		t.Fatalf("after replace: %+v", b.order)
	}

	// A solicited cancel is not flagged as unsolicited; an unsolicited one is.
	b.mustApply(domain.EvCancelRequested, domain.CancelRequested{})
	b.report(domain.BrokerReport{Kind: domain.ReportCancelled})
	var p domain.OrderCancelled
	json.Unmarshal(b.events[len(b.events)-1].Payload, &p)
	if p.Unsolicited || b.order.PendingCancel {
		t.Errorf("solicited cancel: %+v, pending %v", p, b.order.PendingCancel)
	}
	u := to(t, domain.StateAcknowledged)
	u.report(domain.BrokerReport{Kind: domain.ReportCancelled, Reason: "halted"})
	json.Unmarshal(u.events[len(u.events)-1].Payload, &p)
	if !p.Unsolicited || p.Reason != "halted" {
		t.Errorf("unsolicited cancel: %+v", p)
	}
}

func TestDecideCancel(t *testing.T) {
	cases := []struct {
		state    domain.State
		event    domain.EventType // "" for none
		dispatch bool
		code     string
	}{
		{domain.StateValidated, domain.EvOrderCancelled, false, ""},
		{domain.StateSubmitting, domain.EvCancelRequested, true, ""},
		{domain.StateAcknowledged, domain.EvCancelRequested, true, ""},
		{domain.StatePartiallyFilled, domain.EvCancelRequested, true, ""},
		{domain.StateCancelled, "", false, ""},
		{domain.StateNew, "", false, domain.CodeNotCancellable},
		{domain.StateFilled, "", false, domain.CodeNotCancellable},
		{domain.StateRejected, "", false, domain.CodeNotCancellable},
		{domain.StateExpired, "", false, domain.CodeNotCancellable},
	}
	for _, c := range cases {
		b := to(t, c.state)
		drafts, dispatch, err := domain.DecideCancel(b.order, "alice")
		var ce *domain.CommandError
		code := ""
		if errors.As(err, &ce) {
			code = ce.Code
		}
		gotEvent := domain.EventType("")
		if len(drafts) == 1 {
			gotEvent = drafts[0].Type
			if drafts[0].Actor != "alice" || drafts[0].Source != domain.SourceAPI {
				t.Errorf("%s: draft = %+v", c.state, drafts[0])
			}
			b.mustApply(drafts[0].Type, drafts[0].Payload)
		}
		if gotEvent != c.event || dispatch != c.dispatch || code != c.code {
			t.Errorf("%s: event %q dispatch %v code %q; want %q %v %q", c.state, gotEvent, dispatch, code, c.event, c.dispatch, c.code)
		}
	}
	// A second cancel while one is pending does nothing.
	b := to(t, domain.StateAcknowledged)
	b.mustApply(domain.EvCancelRequested, domain.CancelRequested{})
	if drafts, dispatch, err := domain.DecideCancel(b.order, "alice"); len(drafts) != 0 || dispatch || err != nil {
		t.Errorf("repeated cancel: %v %v %v", drafts, dispatch, err)
	}
}

func TestDecideReplace(t *testing.T) {
	code := func(o domain.Order, r domain.Replace) string {
		_, _, err := domain.DecideReplace(o, r, "alice")
		var ce *domain.CommandError
		if errors.As(err, &ce) {
			return ce.Code
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	pf := to(t, domain.StatePartiallyFilled).order // 40 of 100 at 150
	checks := []struct {
		name string
		o    domain.Order
		r    domain.Replace
		want string
	}{
		{"up", pf, domain.Replace{Qty: 150, LimitPrice: price("150")}, ""},
		{"price only", pf, domain.Replace{Qty: 100, LimitPrice: price("151")}, ""},
		{"down to above filled", pf, domain.Replace{Qty: 41, LimitPrice: price("150")}, ""},
		{"down to filled", pf, domain.Replace{Qty: 40, LimitPrice: price("150")}, domain.CodeInvalidReplace},
		{"no change", pf, domain.Replace{Qty: 100, LimitPrice: price("150")}, domain.CodeNoChange},
		{"limit removed", pf, domain.Replace{Qty: 120}, domain.CodeInvalidReplace},
		{"zero price", pf, domain.Replace{Qty: 120, LimitPrice: price("0")}, domain.CodeInvalidReplace},
		{"too large", pf, domain.Replace{Qty: domain.MaxQty + 1, LimitPrice: price("150")}, domain.CodeInvalidReplace},
		{"notional overflow", pf, domain.Replace{Qty: domain.MaxQty, LimitPrice: price("1000000")}, domain.CodeInvalidReplace},
		{"filled", to(t, domain.StateFilled).order, domain.Replace{Qty: 150, LimitPrice: price("150")}, domain.CodeNotReplaceable},
		{"cancelled", to(t, domain.StateCancelled).order, domain.Replace{Qty: 150, LimitPrice: price("150")}, domain.CodeNotReplaceable},
		{"new", to(t, domain.StateNew).order, domain.Replace{Qty: 150, LimitPrice: price("150")}, domain.CodeNotReplaceable},
	}
	for _, c := range checks {
		if got := code(c.o, c.r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// Before dispatch the replace is applied locally; afterwards it waits
	// for the broker.
	v := to(t, domain.StateValidated)
	drafts, dispatch, _ := domain.DecideReplace(v.order, domain.Replace{Qty: 50, LimitPrice: price("149")}, "alice")
	if len(drafts) != 1 || drafts[0].Type != domain.EvOrderReplaced || dispatch {
		t.Fatalf("local replace: %+v dispatch %v", drafts, dispatch)
	}
	v.mustApply(drafts[0].Type, drafts[0].Payload)
	if v.order.Qty != 50 || v.order.PendingReplace != nil {
		t.Fatalf("after local replace: %+v", v.order)
	}
	a := to(t, domain.StateAcknowledged)
	drafts, dispatch, _ = domain.DecideReplace(a.order, domain.Replace{Qty: 50, LimitPrice: price("149")}, "alice")
	if len(drafts) != 1 || drafts[0].Type != domain.EvReplaceRequested || !dispatch {
		t.Fatalf("broker replace: %+v dispatch %v", drafts, dispatch)
	}
	a.mustApply(drafts[0].Type, drafts[0].Payload)
	if a.order.Qty != 100 || a.order.PendingReplace == nil {
		t.Fatalf("pending replace: %+v", a.order)
	}
	if got := code(a.order, domain.Replace{Qty: 60, LimitPrice: price("149")}); got != domain.CodeReplacePending {
		t.Errorf("second replace: %q", got)
	}
	a.mustApply(domain.EvReplaceRejected, domain.ReplaceRejected{})
	a.mustApply(domain.EvCancelRequested, domain.CancelRequested{})
	if got := code(a.order, domain.Replace{Qty: 60, LimitPrice: price("149")}); got != domain.CodeCancelPending {
		t.Errorf("replace during cancel: %q", got)
	}
}

func TestDecideSubmit(t *testing.T) {
	for _, c := range []struct {
		state  domain.State
		drafts int
		send   bool
	}{
		{domain.StateValidated, 1, true},
		{domain.StateSubmitting, 0, true}, // a retry: outcome of the first attempt unknown
		{domain.StateAcknowledged, 0, false},
		{domain.StateCancelled, 0, false},
		{domain.StateRejected, 0, false},
		{domain.StateFilled, 0, false},
	} {
		drafts, send := domain.DecideSubmit(to(t, c.state).order, 7)
		if len(drafts) != c.drafts || send != c.send {
			t.Errorf("%s: %d drafts, send %v", c.state, len(drafts), send)
		}
	}
}

func TestValidateNewOrder(t *testing.T) {
	good := domain.OrderCreated{AccountID: "ACC-1", ClientOrderID: "c-1", Symbol: "BRK.B", Side: domain.Sell,
		Type: domain.Limit, TimeInForce: domain.GTC, Qty: 1, LimitPrice: price("0.000001")}
	if errs := domain.ValidateNewOrder(good); len(errs) != 0 {
		t.Fatalf("valid order refused: %+v", errs)
	}
	market := good
	market.Type, market.LimitPrice = domain.Market, nil
	if errs := domain.ValidateNewOrder(market); len(errs) != 0 {
		t.Fatalf("valid market order refused: %+v", errs)
	}
	bad := map[string]func(*domain.OrderCreated){
		"account_id":      func(o *domain.OrderCreated) { o.AccountID = "has space" },
		"client_order_id": func(o *domain.OrderCreated) { o.ClientOrderID = "x'; DROP TABLE orders;--" },
		"symbol":          func(o *domain.OrderCreated) { o.Symbol = "" },
		"side":            func(o *domain.OrderCreated) { o.Side = "SHORT" },
		"time_in_force":   func(o *domain.OrderCreated) { o.TimeInForce = "" },
		"type":            func(o *domain.OrderCreated) { o.Type = "STOP" },
		"qty":             func(o *domain.OrderCreated) { o.Qty = domain.MaxQty + 1 },
		"limit_price":     func(o *domain.OrderCreated) { o.LimitPrice = nil },
	}
	for field, mutate := range bad {
		o := good
		mutate(&o)
		errs := domain.ValidateNewOrder(o)
		if len(errs) != 1 || errs[0].Field != field {
			t.Errorf("%s: errors = %+v", field, errs)
		}
	}
	for _, p := range []string{"0", "-1", "1000000.000001"} {
		o := good
		o.LimitPrice = price(p)
		if errs := domain.ValidateNewOrder(o); len(errs) != 1 || errs[0].Field != "limit_price" {
			t.Errorf("limit price %s: %+v", p, errs)
		}
	}
	// Each bound alone is not enough: the largest quantity at the largest
	// price does not fit in an int64 of micros, and is refused rather than
	// wrapped.
	huge := good
	huge.Qty, huge.LimitPrice = domain.MaxQty, price("1000000")
	if errs := domain.ValidateNewOrder(huge); len(errs) != 1 || errs[0].Field != "qty" {
		t.Errorf("overflowing notional: %+v", errs)
	}
	large := good
	large.Qty, large.LimitPrice = domain.MaxQty, price("9000")
	if errs := domain.ValidateNewOrder(large); len(errs) != 0 {
		t.Errorf("large but representable order refused: %+v", errs)
	}
	if n, ok := fixed.MulQty(*large.LimitPrice, large.Qty); !ok || n != fixed.Micros(9_000_000_000_000_000_000) {
		t.Errorf("large notional = %d, %v", n, ok)
	}
}

// --- Properties ---

type execRecord struct {
	id, ref string
	kind    domain.ReportKind
	qty     int64
	px      string
}

// effectiveTotals computes, straight from the list of records, what the
// order's totals must be: every fill counts unless a correction or bust
// replaced it, and a correction counts with its own terms.
func effectiveTotals(records []execRecord) (qty int64, notional fixed.Micros) {
	replaced := map[string]bool{}
	for _, r := range records {
		if r.ref != "" {
			replaced[r.ref] = true
		}
	}
	for _, r := range records {
		if replaced[r.id] || r.kind == domain.ReportBust {
			continue
		}
		n, _ := fixed.MulQty(fixed.MustParse(r.px), r.qty)
		qty += r.qty
		notional += n
	}
	return qty, notional
}

// Executions commute: any arrival order of the same fills, corrections and
// busts, with duplicates mixed in, gives the same totals, and those totals
// equal the independently computed effective sum. 300 seeded cases.
func TestProperty_ExecutionRecordsCommute(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0xfeed))
		const orderQty = 100_000

		// Build a random record set: fills, some of which are corrected or
		// busted (each at most once; a correction may itself be corrected).
		var records []execRecord
		open := []string{} // ids that may still be corrected
		n := 2 + rng.IntN(8)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("F%d", i)
			records = append(records, execRecord{id: id, kind: domain.ReportFill, qty: 1 + rng.Int64N(500),
				px: fmt.Sprintf("%d.%02d", 100+rng.IntN(50), rng.IntN(100))})
			open = append(open, id)
		}
		for i := 0; len(open) > 0 && i < rng.IntN(6); i++ {
			j := rng.IntN(len(open))
			ref := open[j]
			open = append(open[:j], open[j+1:]...)
			id := fmt.Sprintf("C%d", i)
			if rng.IntN(3) == 0 {
				records = append(records, execRecord{id: id, ref: ref, kind: domain.ReportBust})
			} else {
				records = append(records, execRecord{id: id, ref: ref, kind: domain.ReportCorrect, qty: 1 + rng.Int64N(500),
					px: fmt.Sprintf("%d.%02d", 100+rng.IntN(50), rng.IntN(100))})
				open = append(open, id)
			}
		}
		wantQty, wantNotional := effectiveTotals(records)

		deliver := func(order []int) (*book, []domain.Outcome) {
			b := newBook(t, orderQty)
			b.mustApply(domain.EvOrderValidated, domain.OrderValidated{})
			b.mustApply(domain.EvSubmissionStarted, domain.SubmissionStarted{})
			var outcomes []domain.Outcome
			for _, i := range order {
				r := records[i]
				outcomes = append(outcomes, b.report(domain.BrokerReport{Kind: r.kind, ExecID: r.id, RefExecID: r.ref,
					Qty: r.qty, Price: fixedOrZero(r.px), Fee: fixed.MustParse("0.01")}))
			}
			return b, outcomes
		}

		for trial := 0; trial < 4; trial++ {
			order := rng.Perm(len(records))
			// Add duplicates at random positions.
			for d := rng.IntN(4); d > 0; d-- {
				at := rng.IntN(len(order) + 1)
				order = append(order[:at], append([]int{order[rng.IntN(len(order))]}, order[at:]...)...)
			}
			b, outcomes := deliver(order)
			if b.order.FilledQty != wantQty || b.order.FilledNotional != wantNotional {
				t.Fatalf("seed %d trial %d: filled %d / %s, want %d / %s\nrecords %+v\norder %v\noutcomes %v",
					seed, trial, b.order.FilledQty, b.order.FilledNotional, wantQty, wantNotional, records, order, outcomes)
			}
			if len(b.execs) != len(records) {
				t.Fatalf("seed %d trial %d: %d records booked, want %d (outcomes %v)", seed, trial, len(b.execs), len(records), outcomes)
			}
			// The fold of the produced events is the order.
			rebuilt, err := domain.Fold(b.events)
			if err != nil || !reflect.DeepEqual(rebuilt, b.order) {
				t.Fatalf("seed %d trial %d: fold differs: %v", seed, trial, err)
			}
		}
	}
}

func fixedOrZero(s string) fixed.Micros {
	if s == "" {
		return 0
	}
	return fixed.MustParse(s)
}

// A random storm of reports and commands against one order. Whatever
// happens: filled quantity stays within the order quantity, no backward
// move happens without a correction, closed orders stay closed, versions
// are gapless, and every prefix of the history satisfies the invariants.
func TestProperty_RandomHistoriesKeepTheInvariants(t *testing.T) {
	kinds := []domain.ReportKind{domain.ReportAck, domain.ReportAck, domain.ReportReject,
		domain.ReportFill, domain.ReportFill, domain.ReportFill, domain.ReportFill, domain.ReportFill, domain.ReportFill,
		domain.ReportCorrect, domain.ReportCorrect, domain.ReportBust, domain.ReportBust,
		domain.ReportCancelled, domain.ReportCancelRejected, domain.ReportCancelRejected,
		domain.ReportReplaced, domain.ReportReplaced, domain.ReportReplaceReject, domain.ReportExpired}
	counts := map[domain.Outcome]int{}
	finals := map[domain.State]int{}

	for seed := uint64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0xbeef))
		b := newBook(t, 100)
		b.mustApply(domain.EvOrderValidated, domain.OrderValidated{})
		var execIDs []string
		if rng.IntN(5) != 0 { // most orders have been sent when the storm starts
			b.mustApply(domain.EvSubmissionStarted, domain.SubmissionStarted{OutboxID: 1})
		}

		// Histories of varying length, so that runs end in every state and
		// not only in the closed ones.
		steps := 3 + rng.IntN(30)
		for step := 0; step < steps; step++ {
			prev := b.order
			switch rng.IntN(10) {
			case 0: // dispatcher
				drafts, _ := domain.DecideSubmit(b.order, 1)
				for _, d := range drafts {
					b.mustApply(d.Type, d.Payload)
				}
			case 1: // client cancel
				if drafts, _, err := domain.DecideCancel(b.order, "alice"); err == nil {
					for _, d := range drafts {
						b.mustApply(d.Type, d.Payload)
					}
				}
			case 2: // client replace
				r := domain.Replace{Qty: 50 + rng.Int64N(150), LimitPrice: price("150")}
				if drafts, _, err := domain.DecideReplace(b.order, r, "alice"); err == nil {
					for _, d := range drafts {
						b.mustApply(d.Type, d.Payload)
					}
				}
			default: // broker report
				r := domain.BrokerReport{Kind: kinds[rng.IntN(len(kinds))], BrokerOrderID: "B-1"}
				switch r.Kind {
				case domain.ReportFill:
					r.ExecID = fmt.Sprintf("X%d", len(execIDs))
					r.Qty, r.Price = 1+rng.Int64N(60), fixed.MustParse("150")
					if rng.IntN(5) == 0 && len(execIDs) > 0 {
						r.ExecID = execIDs[rng.IntN(len(execIDs))] // a duplicate
					} else {
						execIDs = append(execIDs, r.ExecID)
					}
				case domain.ReportCorrect, domain.ReportBust:
					if len(execIDs) == 0 {
						continue
					}
					r.ExecID = fmt.Sprintf("X%d", len(execIDs))
					r.RefExecID = execIDs[rng.IntN(len(execIDs))]
					r.Qty, r.Price = 1+rng.Int64N(60), fixed.MustParse("150")
					execIDs = append(execIDs, r.ExecID)
				case domain.ReportReplaced:
					r.OrderQty, r.LimitPrice = 50+rng.Int64N(150), price("150")
					if b.order.PendingReplace != nil && rng.IntN(2) == 0 {
						r.OrderQty, r.LimitPrice = b.order.PendingReplace.Qty, b.order.PendingReplace.LimitPrice
					}
				}
				counts[b.report(r)]++
			}

			o := b.order
			if o.FilledQty > o.Qty || o.FilledQty < 0 || o.OverfillAccepted {
				t.Fatalf("seed %d step %d: filled %d of %d (accepted %v)", seed, step, o.FilledQty, o.Qty, o.OverfillAccepted)
			}
			if err := domain.CheckInvariants(o); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			if o.State != prev.State {
				last := b.events[len(b.events)-1]
				if domain.Backward(prev.State, o.State) && !last.Type.IsCorrection() {
					t.Fatalf("seed %d step %d: %s → %s on %s", seed, step, prev.State, o.State, last.Type)
				}
				switch prev.State {
				case domain.StateCancelled, domain.StateRejected, domain.StateExpired:
					t.Fatalf("seed %d step %d: left final state %s for %s", seed, step, prev.State, o.State)
				}
			}
		}
		for i, e := range b.events {
			if e.Version != int64(i+1) {
				t.Fatalf("seed %d: event %d has version %d", seed, i, e.Version)
			}
		}
		rebuilt, err := domain.Fold(b.events)
		if err != nil || !reflect.DeepEqual(rebuilt, b.order) {
			t.Fatalf("seed %d: fold differs: %v", seed, err)
		}
		finals[b.order.State]++
	}

	// The storm must actually have exercised the interesting paths.
	for _, o := range []domain.Outcome{domain.OutcomeApplied, domain.OutcomeDuplicate, domain.OutcomeStale, domain.OutcomeQuarantined} {
		if counts[o] < 50 {
			t.Errorf("only %d %s outcomes in the run", counts[o], o)
		}
	}
	var states []string
	for s, n := range finals {
		states = append(states, fmt.Sprintf("%s=%d", s, n))
	}
	sort.Strings(states)
	if len(finals) < 7 {
		t.Errorf("runs ended in only %d distinct states: %v", len(finals), states)
	}
	t.Logf("outcomes %v; final states %v", counts, states)
}
