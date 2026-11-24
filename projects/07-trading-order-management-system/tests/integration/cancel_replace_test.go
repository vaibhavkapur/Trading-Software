package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

func TestCancel_ConfirmedByTheBroker(t *testing.T) {
	e := harness.New(t)
	o := e.Working()

	pending := e.Cancel(o.ID).Want(http.StatusAccepted).Order()
	// Requesting a cancel does not cancel: the order is still working and
	// can still trade until the broker confirms.
	if pending.State != domain.StateAcknowledged || !pending.PendingCancel {
		t.Fatalf("after cancel request: %+v", pending)
	}
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()

	done := e.WantState(o.ID, domain.StateCancelled)
	if done.PendingCancel || done.ClosedAt == nil || done.LeavesQty != 0 {
		t.Errorf("cancelled order = %+v", done)
	}
	evs := e.Events(o.ID)
	var p domain.OrderCancelled
	json.Unmarshal(evs[len(evs)-1].Payload, &p)
	if p.By != domain.ByBroker || p.Unsolicited {
		t.Errorf("cancel payload = %+v", p)
	}
}

func TestCancel_PartialFillThenCancelKeepsTheFill(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateCancelled)
	e.WantFilled(o.ID, 40, "6000")
	e.WantPosition(harness.Account1, "AAPL", 40, "-6000")
}

// The fill wins the race: the cancel was requested, but the order filled
// before the broker processed it.
func TestCancel_FillWinsTheRace(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()

	e.Fill(o.ID, 100, "150")
	filled := e.WantState(o.ID, domain.StateFilled)
	if filled.PendingCancel {
		t.Error("a closed order still has a pending cancel")
	}
	// The broker's refusal of the cancel arrives afterwards and changes
	// nothing.
	e.Must(e.Sim.RejectCancel(o.ID, "TOO_LATE"))
	if got := harness.Outcomes(e.Flush()); got[0] != "STALE" {
		t.Fatalf("late cancel reject outcome = %v", got)
	}
	e.WantState(o.ID, domain.StateFilled)
	e.Cancel(o.ID).WantError(http.StatusConflict, "ORDER_NOT_CANCELLABLE")
}

func TestCancel_RejectedByTheBrokerLeavesTheOrderWorking(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.RejectCancel(o.ID, "IN_AUCTION"))
	e.Flush()
	after := e.WantState(o.ID, domain.StateAcknowledged)
	if after.PendingCancel {
		t.Error("pending_cancel is still set")
	}
	// A new cancel can be requested, and is sent as a new request.
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	if st := e.Sim.Stats(); st.CancelRequests != 2 {
		t.Errorf("broker saw %d cancel requests, want 2", st.CancelRequests)
	}
}

func TestCancel_UnsolicitedBrokerCancel(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateCancelled)
	evs := e.Events(o.ID)
	var p domain.OrderCancelled
	json.Unmarshal(evs[len(evs)-1].Payload, &p)
	if !p.Unsolicited {
		t.Error("the cancel is not marked unsolicited")
	}
}

func TestCancel_WhileStillSubmitting(t *testing.T) {
	e := harness.New(t)
	o := e.Submitted() // sent, not yet acknowledged
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	if st := e.Sim.Stats(); st.CancelRequests != 1 {
		t.Fatalf("broker saw %d cancels", st.CancelRequests)
	}
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateCancelled)
}

func TestExpire_DayOrderAtSessionClose(t *testing.T) {
	e := harness.New(t)
	day := e.Working()
	gtc := e.Working(func(r *service.CreateOrderRequest) { r.TimeInForce = domain.GTC })
	e.Fill(day.ID, 10, "150")

	if n := e.Sim.CloseSession(); n != 1 {
		t.Fatalf("the broker expired %d orders, want 1", n)
	}
	e.Flush()
	expired := e.WantState(day.ID, domain.StateExpired)
	e.WantFilled(day.ID, 10, "1500")
	if expired.ClosedAt == nil {
		t.Error("closed_at is not set")
	}
	e.WantState(gtc.ID, domain.StateAcknowledged)
	e.Cancel(day.ID).WantError(http.StatusConflict, "ORDER_NOT_CANCELLABLE")
}

func TestReplace_BeforeDispatchChangesTheOrderLocally(t *testing.T) {
	e := harness.New(t)
	o := e.Created()
	got := e.Replace(o.ID, 80, "149.25").Want(http.StatusOK).Order()
	if got.Qty != 80 || *got.LimitPrice != fixed.MustParse("149.25") || got.PendingReplace != nil || got.State != domain.StateValidated {
		t.Fatalf("replaced order = %+v", got)
	}
	e.Drain()
	// Only the submission went out, carrying the new terms.
	bo, _ := e.Sim.Order(o.ID)
	if bo.Qty != 80 || *bo.LimitPrice != fixed.MustParse("149.25") {
		t.Fatalf("broker order = %+v", bo)
	}
	if st := e.Sim.Stats(); st.ReplaceRequests != 0 {
		t.Errorf("broker saw %d replace requests, want 0", st.ReplaceRequests)
	}
}

func TestReplace_ConfirmedByTheBroker(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")

	pending := e.Replace(o.ID, 60, "151").Want(http.StatusAccepted).Order()
	// The order keeps its current terms until the broker confirms.
	if pending.Qty != 100 || pending.PendingReplace == nil || pending.PendingReplace.Qty != 60 {
		t.Fatalf("after replace request: %+v", pending)
	}
	e.Drain()
	e.Must(e.Sim.ConfirmReplace(o.ID))
	e.Flush()

	after := e.WantState(o.ID, domain.StatePartiallyFilled)
	if after.Qty != 60 || *after.LimitPrice != fixed.MustParse("151") || after.PendingReplace != nil || after.LeavesQty != 20 {
		t.Fatalf("replaced order = %+v", after)
	}
	e.Fill(o.ID, 20, "151")
	e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 60, "9020")
}

func TestReplace_RejectedByTheBroker(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Replace(o.ID, 200, "").Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.RejectReplace(o.ID, "PRICE_BAND"))
	e.Flush()
	after := e.WantState(o.ID, domain.StateAcknowledged)
	if after.Qty != 100 || after.PendingReplace != nil {
		t.Fatalf("after rejected replace: %+v", after)
	}
}

// The replace was requested for 60 while 40 were filled; before the broker
// processes it, 30 more fill. The broker then refuses the replace. Whatever
// the interleaving, filled quantity never ends up above order quantity.
func TestReplace_FillRacesTheReplace(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")
	e.Replace(o.ID, 60, "").Want(http.StatusAccepted)
	e.Drain()
	e.Fill(o.ID, 30, "150") // 70 filled, replace to 60 still pending

	// A confirmation of the now impossible replace is quarantined.
	e.Must(e.Sim.ConfirmReplace(o.ID))
	if got := harness.Outcomes(e.Flush()); got[0] != "QUARANTINED" {
		t.Fatalf("impossible replace outcome = %v", got)
	}
	after := e.WantState(o.ID, domain.StatePartiallyFilled)
	if after.Qty != 100 || after.FilledQty != 70 {
		t.Fatalf("after impossible replace: qty %d filled %d", after.Qty, after.FilledQty)
	}
	issue := e.OpenIssue(domain.ReasonConflictingReport)
	if issue.Severity != store.SeverityHigh {
		t.Errorf("severity = %s", issue.Severity)
	}
}

func TestReplace_Refusals(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")

	e.Replace(o.ID, 40, "").WantError(http.StatusUnprocessableEntity, "INVALID_REPLACE") // not above filled
	e.Replace(o.ID, 100, "150").WantError(http.StatusUnprocessableEntity, "NO_CHANGE")
	e.Replace(o.ID, 50_000, "").WantError(http.StatusUnprocessableEntity, "RISK_REJECTED")
	e.Call(e.Alice, http.MethodPost, "/v1/orders/"+o.ID+"/replace", `{}`, "Idempotency-Key", e.Key()).
		WantError(http.StatusUnprocessableEntity, "VALIDATION_FAILED")

	e.Replace(o.ID, 120, "").Want(http.StatusAccepted)
	e.Replace(o.ID, 130, "").WantError(http.StatusConflict, "REPLACE_PENDING")
	e.Drain()
	e.Must(e.Sim.RejectReplace(o.ID, "no"))
	e.Flush()

	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Replace(o.ID, 130, "").WantError(http.StatusConflict, "CANCEL_PENDING")
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.Replace(o.ID, 130, "").WantError(http.StatusConflict, "ORDER_NOT_REPLACEABLE")

	// None of the refusals left a trace on the order.
	after := e.Order(o.ID)
	if after.Qty != 100 || *after.LimitPrice != fixed.MustParse("150") {
		t.Fatalf("order terms changed: %+v", after)
	}

	m := e.Working(func(r *service.CreateOrderRequest) { r.Type = domain.Market; r.LimitPrice = nil })
	e.Replace(m.ID, 0, "151").WantError(http.StatusUnprocessableEntity, "VALIDATION_FAILED")
}
