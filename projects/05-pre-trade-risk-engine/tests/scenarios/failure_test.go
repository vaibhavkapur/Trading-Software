// Package scenarios exercises the engine under the conditions that go wrong
// in production: dependencies down, data missing or stale, clocks
// disagreeing, messages delivered twice.
package scenarios

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"riskengine/src/audit"
	"riskengine/src/core"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
	"riskengine/src/metrics"
	"riskengine/src/policy"
	"riskengine/src/state"
	"riskengine/tests/harness"
)

var (
	allow  = domain.Allow
	reject = domain.Reject
	review = domain.Review
)

func r(codes ...string) []string { return codes }

// flakyLog is an audit log whose storage can be made to fail.
type flakyLog struct {
	*audit.Log
	down atomic.Bool
}

func (f *flakyLog) Append(ev domain.Event) (domain.Seal, error) {
	if f.down.Load() {
		return domain.Seal{}, errors.New("disk unavailable")
	}
	return f.Log.Append(ev)
}

type rig struct {
	eng   *core.Engine
	log   *flakyLog
	mkt   *market.Store
	clock *harness.Clock
	met   *metrics.Registry
}

// newRig wires an engine by hand so the audit log can be broken on demand.
func newRig(t *testing.T, loadPolicy bool) *rig {
	t.Helper()
	l, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"), harness.Key(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	g := &rig{log: &flakyLog{Log: l}, mkt: market.NewStore(5 * time.Second), clock: harness.NewClock(), met: metrics.New()}
	g.eng = core.New(state.New(), g.mkt, g.log, g.clock, g.met)
	if loadPolicy {
		p, err := policy.Parse([]byte(harness.DefaultPolicy))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.eng.SetPolicy(p, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *rig) quote(t *testing.T, sym, px string) {
	t.Helper()
	p := fixed.MustParse(px)
	if err := g.mkt.Update(market.Quote{Symbol: sym, Bid: p, Ask: p, EventTime: g.clock.Now()}, g.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestNoPolicyMeansNoTrading(t *testing.T) {
	g := newRig(t, false)
	g.quote(t, "XYZ", "100")
	d := g.eng.Evaluate(harness.Req("o1", "a-1", "XYZ", "BUY", 1, "100.00"))
	harness.AssertDecision(t, d, reject, domain.PolicyUnavailable)
	if d.Seq == 0 {
		t.Error("the reject itself must still be recorded")
	}
}

// Definition of done: no decision without a recorded policy version and
// reason trace. If the record cannot be written, the answer is REJECT.
func TestAuditOutageFailsClosed(t *testing.T) {
	g := newRig(t, true)
	g.quote(t, "XYZ", "100")
	harness.AssertDecision(t, g.eng.Evaluate(harness.Req("o1", "a-big", "XYZ", "BUY", 10, "100.00")), allow)

	g.log.down.Store(true)
	d := g.eng.Evaluate(harness.Req("o2", "a-big", "XYZ", "BUY", 10, "100.00"))
	harness.AssertDecision(t, d, reject, domain.AuditUnavailable)
	if d.Reservation != nil || d.Seq != 0 {
		t.Fatalf("an unrecorded decision reserved capacity or claims a record: %+v", d)
	}
	if n := len(g.eng.AccountState("a-big").Reservations); n != 1 {
		t.Fatalf("%d reservations, want only o1's", n)
	}
	// Lifecycle calls change nothing while the log is down.
	if _, err := g.eng.Commit("o1"); !errors.Is(err, core.ErrAudit) {
		t.Fatalf("commit during outage: %v", err)
	}
	if _, err := g.eng.Fill("o1", "f1", 10, fixed.MustParse("100")); !errors.Is(err, core.ErrAudit) {
		t.Fatalf("fill during outage: %v", err)
	}
	st := g.eng.AccountState("a-big")
	if st.Reservations[0].Status != domain.Reserved || len(st.Positions) != 0 {
		t.Fatalf("state moved without a record: %+v", st)
	}
	if p, _ := policy.Parse([]byte(harness.DefaultPolicy)); g.eng.SetPolicy(p, "x") == nil {
		t.Fatal("policy swapped without a record")
	}

	// A kill switch must still engage: refusing to stop trading because
	// the evidence store is broken would be failing open.
	ks, _, durable, err := g.eng.SetKillSwitch(domain.ScopeGlobal, "", true, "ops", "audit outage")
	if !errors.Is(err, core.ErrAudit) || durable || !ks.Enabled || len(g.eng.KillSwitches()) != 1 {
		t.Fatalf("kill during outage: durable=%v err=%v active=%d", durable, err, len(g.eng.KillSwitches()))
	}
	// But it cannot be lifted without a record.
	if _, _, _, err := g.eng.SetKillSwitch(domain.ScopeGlobal, "", false, "ops", ""); err == nil || len(g.eng.KillSwitches()) != 1 {
		t.Fatal("kill switch lifted with no audit record")
	}

	g.log.down.Store(false)
	if _, _, _, err := g.eng.SetKillSwitch(domain.ScopeGlobal, "", false, "ops", "recovered"); err != nil {
		t.Fatal(err)
	}
	// The order_id rejected for AUDIT_UNAVAILABLE was not consumed.
	harness.AssertDecision(t, g.eng.Evaluate(harness.Req("o2", "a-big", "XYZ", "BUY", 10, "100.00")), allow)
	if g.met.Counter("risk_audit_append_errors_total") < 4 {
		t.Error("audit failures not counted")
	}
}

func TestMarketDataLossAndRecovery(t *testing.T) {
	e := harness.New(t)
	e.Expect(reject, r(domain.NoMarketData), "m1", "a-big", "XYZ", "BUY", 1, "100.00") // feed never connected
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "m2", "a-big", "XYZ", "BUY", 1, "100.00")
	e.Clock.Advance(3 * time.Second) // feed disconnects; last quote ages out
	e.Expect(reject, r(domain.StaleQuote), "m3", "a-big", "XYZ", "BUY", 1, "100.00")
	// Even a risk-reducing order is refused: without a price there is no
	// way to tell that it is one.
	e.Expect(reject, r(domain.StaleQuote), "m4", "a-big", "XYZ", "SELL", 1, "100.00")
	e.Quote("XYZ", "100", "100") // feed reconnects
	e.Expect(allow, nil, "m5", "a-big", "XYZ", "BUY", 1, "100.00")
}

// A position in a symbol whose feed has died is valued at its last mark,
// and orders elsewhere go to a human instead of being auto-approved.
func TestStaleMarkOnAnotherSymbolForcesReview(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Quote("ABC", "50", "50")
	e.Expect(allow, nil, "s1", "a-big", "ABC", "BUY", 100, "50.00")
	e.Fill("s1", "f", 100, "50.00")

	e.Clock.Advance(3 * time.Second)
	e.Quote("XYZ", "100", "100") // XYZ is live again, ABC is not
	d := e.Expect(review, r(domain.ExposureMarkStale), "s2", "a-big", "XYZ", "BUY", 10, "100.00")
	if d.Reservation.Status != domain.PendingReview {
		t.Fatalf("reservation: %+v", d.Reservation)
	}

	// The OMS cannot commit an order that is awaiting review.
	if _, err := e.App.Engine.Commit("s2"); !errors.Is(err, core.ErrPendingReview) {
		t.Fatalf("commit before approval: %v", err)
	}
	res, err := e.App.Engine.Approve("s2", "risk-officer")
	if err != nil || res.Status != domain.Reserved || !res.ExpiresAt.Equal(e.Clock.Now().Add(30*time.Second)) {
		t.Fatalf("approve: %+v %v", res, err)
	}
	if _, err := e.App.Engine.Approve("s2", "risk-officer"); !errors.Is(err, core.ErrNotPending) {
		t.Fatalf("second approval: %v", err)
	}
	if res, err := e.App.Engine.Commit("s2"); err != nil || res.Status != domain.Committed {
		t.Fatalf("commit after approval: %+v %v", res, err)
	}

	e.Quote("ABC", "50", "50") // feed back: no review needed
	e.Expect(allow, nil, "s3", "a-big", "XYZ", "BUY", 10, "100.00")
}

func TestClockSkew(t *testing.T) {
	e := harness.New(t)
	now := e.Clock.Now()
	px := fixed.MustParse("100")
	put := func(ahead time.Duration) error {
		return e.App.Market.Update(market.Quote{Symbol: "XYZ", Bid: px, Ask: px, EventTime: now.Add(ahead)}, now)
	}
	// Venue clock 400ms ahead of ours: inside the 500ms policy tolerance.
	if err := put(400 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	e.Expect(allow, nil, "k1", "a-big", "XYZ", "BUY", 1, "100.00")
	// 3s ahead: ingest tolerates it (5s), but no decision may rest on it.
	if err := put(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	e.Expect(reject, r(domain.QuoteClockSkew), "k2", "a-big", "XYZ", "BUY", 1, "100.00")
	// A minute ahead is refused outright so it cannot shadow later quotes.
	if err := put(time.Minute); !errors.Is(err, market.ErrFutureQuote) {
		t.Fatalf("far-future quote: %v", err)
	}

	// Our own clock steps backwards (bad NTP sync): the quote we hold now
	// looks like it is from the future, and is not trusted.
	e2 := harness.New(t)
	e2.Quote("XYZ", "100", "100")
	e2.Clock.Advance(-2 * time.Second)
	e2.Expect(reject, r(domain.QuoteClockSkew), "k3", "a-big", "XYZ", "BUY", 1, "100.00")
}

func TestDuplicateDelivery(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	first := e.Expect(allow, nil, "d1", "a-big", "XYZ", "BUY", 100, "100.00")

	// The same request arriving again (client retry, at-least-once queue)
	// gets the original decision back and reserves nothing more.
	e.Clock.Advance(100 * time.Millisecond)
	again := e.Eval("d1", "a-big", "XYZ", "BUY", 100, "100.00")
	if !again.Replayed || again.Hash != first.Hash || !again.DecidedAt.Equal(first.DecidedAt) {
		t.Fatalf("redelivery was re-decided: %s", harness.JSON(again))
	}
	if n := len(e.App.Engine.AccountState("a-big").Reservations); n != 1 {
		t.Fatalf("%d reservations after redelivery", n)
	}

	// The same order_id with different economics is not a retry.
	for i, alt := range []domain.RiskRequest{
		harness.Req("d1", "a-big", "XYZ", "BUY", 101, "100.00"),
		harness.Req("d1", "a-big", "XYZ", "SELL", 100, "100.00"),
		harness.Req("d1", "a-big", "XYZ", "BUY", 100, "100.01"),
		harness.Req("d1", "a-1", "XYZ", "BUY", 100, "100.00"),
	} {
		d := e.App.Engine.Evaluate(alt)
		harness.AssertDecision(t, d, reject, domain.DuplicateOrderID)
		if d.Reservation != nil {
			t.Fatalf("variant %d reserved capacity", i)
		}
	}
	// None of that disturbed the original.
	if got, _ := e.App.Engine.Decision("d1"); got.Hash != first.Hash || got.Decision != allow {
		t.Fatal("original decision was overwritten")
	}
	// A rejected order_id is final too: resubmitting it cannot flip it.
	rej := e.Expect(reject, r(domain.PriceCollar), "d2", "a-big", "XYZ", "BUY", 1, "150.00")
	if d := e.Eval("d2", "a-big", "XYZ", "BUY", 1, "150.00"); !d.Replayed || d.Hash != rej.Hash {
		t.Fatal("rejected order was re-evaluated on retry")
	}
	if e.App.Metrics.Counter("risk_duplicate_replays_total") != 2 {
		t.Error("replays not counted")
	}

	// Commit and release are safe to retry as well.
	for i := 0; i < 2; i++ {
		if res, err := e.App.Engine.Commit("d1"); err != nil || res.Status != domain.Committed {
			t.Fatalf("commit #%d: %+v %v", i, res, err)
		}
	}
	for i := 0; i < 2; i++ {
		if res, err := e.App.Engine.Release("d1", "cancel"); err != nil || res.Status != domain.Released {
			t.Fatalf("release #%d: %+v %v", i, res, err)
		}
	}
}

// Definition of done: the kill switch blocks new orders across all
// execution entry points — evaluate, approve and commit.
func TestKillSwitchBlocksEveryEntryPoint(t *testing.T) {
	e := harness.New(t)
	eng := e.App.Engine
	e.Quote("XYZ", "100", "100")
	e.Quote("ABC", "50", "50")
	e.Expect(allow, nil, "k-lease", "a-1", "XYZ", "BUY", 10, "100.00")                              // allowed, not yet sent
	e.Expect(allow, nil, "k-live", "a-1", "XYZ", "BUY", 10, "100.00")                               // live at the venue
	e.Expect(review, r(domain.ReviewOrderNotional), "k-review", "a-1", "XYZ", "BUY", 600, "100.00") // awaiting approval
	eng.Commit("k-live")

	if _, _, _, err := eng.SetKillSwitch(domain.ScopeAccount, "a-1", true, "risk-officer", "limit breach"); err != nil {
		t.Fatal(err)
	}

	// 1. New orders.
	e.Expect(reject, r(domain.KillSwitchAccount), "k-new", "a-1", "XYZ", "BUY", 1, "100.00")
	e.Expect(reject, r(domain.KillSwitchAccount), "k-new-sell", "a-1", "XYZ", "SELL", 1, "100.00")
	// 2. An order allowed before the switch but not yet sent.
	res, err := eng.Commit("k-lease")
	if !errors.Is(err, core.ErrKillSwitch) || res.Status != domain.Released || res.ReleaseReason != "KILL_SWITCH" {
		t.Fatalf("commit under kill switch: %+v %v", res, err)
	}
	// 3. An order waiting for approval.
	if res, err := eng.Approve("k-review", "risk-officer"); !errors.Is(err, core.ErrKillSwitch) || res.Status != domain.Released {
		t.Fatalf("approve under kill switch: %+v %v", res, err)
	}
	// Fills on the order that was already live are facts and still booked.
	if _, err := eng.Fill("k-live", "f1", 10, fixed.MustParse("100")); err != nil {
		t.Fatalf("fill under kill switch: %v", err)
	}
	// Scope: other accounts trade on.
	e.Expect(allow, nil, "k-other", "a-big", "XYZ", "BUY", 1, "100.00")

	// Symbol and global scopes.
	eng.SetKillSwitch(domain.ScopeSymbol, "ABC", true, "ops", "")
	e.Expect(reject, r(domain.KillSwitchSymbol), "k-sym", "a-big", "ABC", "BUY", 10, "50.00")
	eng.SetKillSwitch(domain.ScopeGlobal, "", true, "ops", "")
	e.Expect(reject, r(domain.KillSwitchGlobal, domain.KillSwitchSymbol), "k-all", "a-big", "ABC", "BUY", 10, "50.00")
	e.Expect(reject, r(domain.KillSwitchGlobal, domain.KillSwitchAccount), "k-all2", "a-1", "XYZ", "BUY", 1, "100.00")

	// Lifting one switch does not lift the others.
	eng.SetKillSwitch(domain.ScopeGlobal, "", false, "ops", "")
	e.Expect(reject, r(domain.KillSwitchAccount), "k-still", "a-1", "XYZ", "BUY", 1, "100.00")
	eng.SetKillSwitch(domain.ScopeAccount, "a-1", false, "risk-officer", "resolved")
	e.Expect(allow, nil, "k-resumed", "a-1", "XYZ", "BUY", 1, "100.00")
}

// Once SetKillSwitch returns, no order may be allowed, however many
// evaluations were in flight.
func TestKillSwitchIsImmediateUnderLoad(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	px := fixed.MustParse("100")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowedSeqs []uint64
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// Spread across accounts so rate limits do not mask the test.
				acct := []string{"a-1", "a-big"}[i%2]
				if d := e.Eval(fmt.Sprintf("w%d-%d", w, i), acct, "XYZ", "BUY", 1, "100.00"); d.Decision == allow {
					mu.Lock()
					allowedSeqs = append(allowedSeqs, d.Seq)
					mu.Unlock()
				}
				e.Clock.Advance(time.Millisecond)
				if i%50 == 0 { // keep the quote fresh as the fake clock runs
					now := e.Clock.Now()
					e.App.Market.Update(market.Quote{Symbol: "XYZ", Bid: px, Ask: px, EventTime: now}, now)
				}
			}
		}(w)
	}
	time.Sleep(30 * time.Millisecond)
	_, seal, _, err := e.App.Engine.SetKillSwitch(domain.ScopeGlobal, "", true, "ops", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()

	if len(allowedSeqs) == 0 {
		t.Fatal("fixture: nothing was allowed before the switch")
	}
	for _, s := range allowedSeqs {
		if s > seal.Seq {
			t.Fatalf("order allowed at audit seq %d, after the kill switch at seq %d", s, seal.Seq)
		}
	}
}

func TestReservationLeaseExpiry(t *testing.T) {
	e := harness.New(t)
	eng := e.App.Engine
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "x1", "a-big", "XYZ", "BUY", 1000, "100.00") // never committed
	e.Expect(allow, nil, "x2", "a-big", "XYZ", "BUY", 500, "100.00")  // committed: lives on
	eng.Commit("x2")
	e.Expect(reject, r(domain.MaxSymbolExposure), "x3", "a-big", "XYZ", "BUY", 100, "100.00")

	// Exactly at expiry the lease is still good; a restart must not
	// shorten or extend it.
	e.Clock.Advance(30 * time.Second)
	e.Restart()
	eng = e.App.Engine
	if n := eng.Sweep(); n != 0 {
		t.Fatalf("swept %d at exact expiry", n)
	}
	e.Clock.Advance(time.Millisecond)

	// The OMS tries to commit late: refused, and the capacity is released.
	if res, err := eng.Commit("x1"); !errors.Is(err, core.ErrExpired) || res.ReleaseReason != "EXPIRED" {
		t.Fatalf("late commit: %+v %v", res, err)
	}
	if _, err := eng.Commit("x1"); !errors.Is(err, core.ErrReleased) {
		t.Fatalf("commit after expiry: %v", err)
	}
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "x4", "a-big", "XYZ", "BUY", 100, "100.00") // freed capacity is usable

	// The sweeper does the same for leases nobody comes back for.
	e.Clock.Advance(31 * time.Second)
	if n := eng.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1 (x4)", n)
	}
	open := eng.AccountState("a-big").Reservations
	if len(open) != 1 || open[0].OrderID != "x2" || open[0].Status != domain.Committed {
		t.Fatalf("only the committed order should remain: %+v", open)
	}

	// A fill that arrives for an expired order is booked and flagged.
	res, err := eng.Fill("x1", "late", 5, fixed.MustParse("100"))
	if err != nil || !res.Unreserved || res.Position.Qty != 5 {
		t.Fatalf("fill on expired order: %+v %v", res, err)
	}
}

// The OMS is unreachable for the whole test. Push delivery keeps failing,
// yet nothing gets through, because enforcement is in the engine.
func TestOMSUnreachableDoesNotWeakenKillSwitch(t *testing.T) {
	e := harness.NewWith(t, harness.DefaultPolicy, "http://127.0.0.1:1/unreachable")
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "u1", "a-1", "XYZ", "BUY", 1, "100.00")
	if _, _, _, err := e.App.Engine.SetKillSwitch(domain.ScopeGlobal, "", true, "ops", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.App.Engine.Commit("u1"); !errors.Is(err, core.ErrKillSwitch) {
		t.Fatalf("commit: %v", err)
	}
	e.Expect(reject, r(domain.KillSwitchGlobal), "u2", "a-1", "XYZ", "BUY", 1, "100.00")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.App.Notifier.Status(); len(s) == 1 && s[0].Attempts >= 2 {
			if s[0].AckedAt != nil || s[0].LastError == "" {
				t.Fatalf("delivery status: %+v", s[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("notifier did not retry the unreachable OMS")
}

// Finished orders are forgotten after the retention period so memory stays
// bounded; orders that still hold capacity never are. Forgetting is driven
// by event time, so a restart forgets exactly the same orders.
func TestFinishedOrdersAreForgottenAfterRetention(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(reject, r(domain.PriceCollar), "old-rejected", "a-big", "XYZ", "BUY", 1, "150.00")
	e.Expect(allow, nil, "old-released", "a-big", "XYZ", "BUY", 1, "100.00")
	e.App.Engine.Release("old-released", "cancel")
	e.Expect(allow, nil, "old-working", "a-big", "XYZ", "BUY", 10, "100.00")
	e.App.Engine.Commit("old-working")

	check := func(when string) {
		t.Helper()
		for id, want := range map[string]bool{"old-rejected": false, "old-released": false, "old-working": true, "new": true} {
			if _, ok := e.App.Engine.Decision(id); ok != want {
				t.Errorf("%s: order %s remembered=%v, want %v", when, id, ok, want)
			}
		}
	}
	e.Clock.Advance(24*time.Hour + time.Second)
	e.Quote("XYZ", "100", "100")
	if _, ok := e.App.Engine.Decision("old-rejected"); !ok {
		t.Fatal("forgetting must wait for the next decision event, not wall time")
	}
	e.Expect(allow, nil, "new", "a-big", "XYZ", "BUY", 1, "100.00")
	check("live")
	e.Restart()
	check("after restart")

	// The long-lived order still fills normally.
	if res, err := e.App.Engine.Fill("old-working", "f", 10, fixed.MustParse("100")); err != nil || res.Unreserved {
		t.Fatalf("fill on retained order: %+v %v", res, err)
	}
	if _, err := e.App.Engine.Fill("old-released", "f", 1, fixed.MustParse("100")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fill on forgotten order: %v", err)
	}
}
