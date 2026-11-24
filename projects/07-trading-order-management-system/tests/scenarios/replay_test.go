package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// replayScript drives a fixed sequence of commands, broker activity and
// faults. With sequential ids and a scripted clock, everything it produces
// is a pure function of the code.
func replayScript(t *testing.T, e *harness.Env) {
	t.Helper()
	ctx := context.Background()
	step := func() { e.Advance(250 * time.Millisecond) }

	// A: the plain path. Buy 100 AAPL, filled in two parts.
	a := e.Created()
	step()
	e.Drain()
	e.Must(e.Sim.Ack(a.ID))
	step()
	e.Flush()
	e.Fill(a.ID, 40, "149.50")
	step()
	e.Fill(a.ID, 60, "150")

	// B: cancelled before it was ever sent.
	b := e.Created(func(r *service.CreateOrderRequest) {
		r.Side, r.Symbol, r.Qty, r.LimitPrice = domain.Sell, "MSFT", 50, harness.Price("400")
	})
	e.Cancel(b.ID).Want(http.StatusOK)
	e.Drain()

	// C: refused by pre-trade risk.
	e.Create(harness.OrderReq(func(r *service.CreateOrderRequest) { r.Qty = 50_000 }), e.Key()).Want(http.StatusCreated)

	// D: a retried submission, a partial fill that the broker later
	// corrects, a replace and a cancel.
	step()
	e.Sim.SetFaults(brokersim.Faults{LoseResponses: 1})
	d := e.Created(func(r *service.CreateOrderRequest) {
		r.Symbol, r.Qty, r.LimitPrice, r.TimeInForce = "TSLA", 200, harness.Price("250"), domain.GTC
	})
	e.Drain()
	e.Advance(time.Second)
	e.Drain()
	e.Must(e.Sim.Ack(d.ID))
	x := e.FillHeld(d.ID, 80, "250")
	e.Flush()
	step()
	if _, _, err := e.Sim.Correct(x, 70, fixed.MustParse("249.50")); err != nil {
		t.Fatal(err)
	}
	e.Flush()
	e.Replace(d.ID, 150, "249").Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmReplace(d.ID))
	e.Flush()
	step()
	e.Cancel(d.ID).Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(d.ID))
	e.Flush()

	// E: both fills lost on the way; reconciliation books them.
	f := e.Working(func(r *service.CreateOrderRequest) { r.Symbol, r.LimitPrice = "NVDA", harness.Price("900") })
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(f.ID, 30, "899")
	e.FillHeld(f.ID, 70, "900")
	e.Sim.SetFaults(brokersim.Faults{})
	e.Advance(6 * time.Second)
	e.Reconcile()

	// F: the broker overfills; an operator accepts it.
	g := e.Working()
	e.Fill(g.ID, 90, "150")
	e.Fill(g.ID, 20, "150.10")
	issue := e.OpenIssue(domain.ReasonOverfill)
	step()
	e.Resolve(issue.ID, service.ActionAcceptExecution, "confirmed with the broker").Want(http.StatusOK)

	// Finally the broker redelivers everything it ever sent.
	e.Accepted(e.Sim.RedeliverAll(ctx))
}

type replayLine struct {
	Seq         int64           `json:"seq"`
	OrderID     string          `json:"order_id"`
	Version     int64           `json:"version"`
	Type        string          `json:"type"`
	StateAfter  string          `json:"state_after"`
	Source      string          `json:"source"`
	Actor       string          `json:"actor,omitempty"`
	CausationID string          `json:"causation_id,omitempty"`
	OccurredAt  string          `json:"occurred_at"`
	RecordedAt  string          `json:"recorded_at"`
	Payload     json.RawMessage `json:"payload"`
}

// dump renders the full event log followed by the final orders and
// positions, one JSON document per line.
func dump(t *testing.T, e *harness.Env) []byte {
	t.Helper()
	ctx := context.Background()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	events, err := e.App.Store.AllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const layout = "2006-01-02T15:04:05.000000Z"
	for _, ev := range events {
		var compact bytes.Buffer
		if err := json.Compact(&compact, ev.Payload); err != nil {
			t.Fatal(err)
		}
		enc.Encode(replayLine{ev.Seq, ev.OrderID, ev.Version, string(ev.Type), string(ev.StateAfter), string(ev.Source),
			ev.Actor, ev.CausationID, ev.OccurredAt.UTC().Format(layout), ev.RecordedAt.UTC().Format(layout), compact.Bytes()})
	}
	orders, err := e.App.Store.ListOrders(ctx, store.OrderFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	for _, o := range orders {
		enc.Encode(map[string]any{"order": service.View(o)})
	}
	positions, err := e.App.Store.ListPositions(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		enc.Encode(map[string]any{"position": p})
	}
	return out.Bytes()
}

// Replay: the same script, run twice against two fresh systems, produces
// byte-identical histories, and that history matches the golden file
// committed with the code. Any change to what the OMS records for a given
// input shows up here as a diff.
//
// Regenerate with: go test ./tests/scenarios -run TestReplay -update
func TestReplay_DeterministicGoldenHistory(t *testing.T) {
	run := func() []byte {
		e := harness.New(t, func(o *harness.Options) { o.SequentialIDs = true })
		replayScript(t, e)
		return dump(t, e)
	}
	first, second := run(), run()
	if !bytes.Equal(first, second) {
		t.Fatalf("two runs of the same script differ:\n%s", firstDiff(first, second))
	}

	golden := filepath.Join("testdata", "replay.golden.jsonl")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, first, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d lines)", golden, bytes.Count(first, []byte("\n")))
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(first, want) {
		t.Fatalf("history differs from %s (run with -update if the change is intended):\n%s", golden, firstDiff(want, first))
	}
}

func firstDiff(want, got []byte) string {
	w, g := bytes.Split(want, []byte("\n")), bytes.Split(got, []byte("\n"))
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b []byte
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if !bytes.Equal(a, b) {
			return "line " + itoa(i+1) + "\n  want: " + string(a) + "\n  got:  " + string(b)
		}
	}
	return "(no difference found)"
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Replay: folding the stored events of each order through the pure state
// machine, with no database involved, reproduces exactly the order the
// database holds.
func TestReplay_FoldingEventsRebuildsEveryOrder(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.SequentialIDs = true })
	replayScript(t, e)
	ctx := context.Background()

	events, err := e.App.Store.AllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byOrder := map[string][]domain.Event{}
	for _, ev := range events {
		byOrder[ev.OrderID] = append(byOrder[ev.OrderID], ev)
	}
	orders, err := e.App.Store.ListOrders(ctx, store.OrderFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 6 || len(byOrder) != 6 {
		t.Fatalf("%d orders, %d event streams; want 6 of each", len(orders), len(byOrder))
	}
	for _, stored := range orders {
		rebuilt, err := domain.Fold(byOrder[stored.ID])
		if err != nil {
			t.Fatalf("fold %s: %v", stored.ID, err)
		}
		if !reflect.DeepEqual(rebuilt, stored) {
			t.Errorf("order %s: rebuilt from events\n  %+v\nstored\n  %+v", stored.ID, rebuilt, stored)
		}
		// Every prefix of the history is a valid order too, and the state
		// recorded on each event is the state the fold reaches there.
		var o domain.Order
		for _, ev := range byOrder[stored.ID] {
			if o, err = domain.Apply(o, ev); err != nil {
				t.Fatalf("apply %s v%d: %v", ev.OrderID, ev.Version, err)
			}
			if o.State != ev.StateAfter {
				t.Errorf("%s v%d: state_after %s, fold gives %s", ev.OrderID, ev.Version, ev.StateAfter, o.State)
			}
			if err := domain.CheckInvariants(o); err != nil {
				t.Errorf("%s v%d: %v", ev.OrderID, ev.Version, err)
			}
		}
	}
	if rep := e.Verify(); rep.Orders != 6 || rep.Events != len(events) {
		t.Errorf("verify report = %+v", rep)
	}
}
