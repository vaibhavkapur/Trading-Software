package oms

import (
	"errors"
	"testing"

	"sor/src/domain"
)

func setup(t *testing.T, target int64) (*Book, *domain.ParentOrder) {
	t.Helper()
	b := NewBook()
	return b, b.AddParent(domain.ParentOrder{Symbol: "XYZ", Side: domain.Buy, TargetQty: target, Status: domain.ParentWorking})
}

func child(t *testing.T, b *Book, p *domain.ParentOrder, qty int64) *domain.ChildOrder {
	t.Helper()
	c, err := b.NewChild(p.ID, domain.Allocation{Venue: "A", Qty: qty, LimitPx: 100}, "S-1", 10000, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func report(c *domain.ChildOrder, fills ...domain.VenueFill) domain.ExecReport {
	return domain.ExecReport{ChildID: c.ID, Venue: c.Venue, Status: domain.ReportDone, Fills: fills}
}

func TestReservationBlocksOverRouting(t *testing.T) {
	b, p := setup(t, 1000)
	child(t, b, p, 600)
	if _, err := b.NewChild(p.ID, domain.Allocation{Venue: "B", Qty: 500}, "S", 0, 0, 1); !errors.Is(err, ErrBadQty) {
		t.Fatalf("want ErrBadQty, got %v", err)
	}
	if Available(p) != 400 {
		t.Fatalf("available = %d", Available(p))
	}
}

func TestFillLifecycleAndIdempotency(t *testing.T) {
	b, p := setup(t, 1000)
	c := child(t, b, p, 600)
	rep := report(c, domain.VenueFill{ID: "f1", Qty: 400, Px: 100, FeeMills: 12})
	fills, changed, err := b.ApplyReport(rep, 5)
	if err != nil || !changed || len(fills) != 1 {
		t.Fatalf("apply: %v %v %v", fills, changed, err)
	}
	if c.Status != domain.ChildPartial || p.FilledQty != 400 || p.ReservedQty != 0 {
		t.Fatalf("child %+v parent %+v", c, p)
	}
	// Duplicate delivery of the same report changes nothing.
	if _, changed, err := b.ApplyReport(rep, 6); changed || err != nil || p.FilledQty != 400 || len(b.Fills(p.ID)) != 1 {
		t.Fatalf("duplicate applied: %v %v", changed, err)
	}
	// A new child reporting an already-seen venue fill id is not double counted.
	c2 := child(t, b, p, 600)
	if _, _, err := b.ApplyReport(report(c2, domain.VenueFill{ID: "f1", Qty: 400, Px: 100}, domain.VenueFill{ID: "f2", Qty: 600, Px: 100}), 7); err != nil {
		t.Fatal(err)
	}
	if p.FilledQty != 1000 || p.Status != domain.ParentFilled || c2.Status != domain.ChildFilled {
		t.Fatalf("parent %+v child %+v", p, c2)
	}
	if _, err := b.NewChild(p.ID, domain.Allocation{Venue: "A", Qty: 100}, "S", 0, 0, 8); !errors.Is(err, ErrNotWorking) {
		t.Fatalf("want ErrNotWorking, got %v", err)
	}
}

func TestOverfillReportRefusedAtomically(t *testing.T) {
	b, p := setup(t, 1000)
	c := child(t, b, p, 300)
	_, changed, err := b.ApplyReport(report(c, domain.VenueFill{ID: "f1", Qty: 200, Px: 100}, domain.VenueFill{ID: "f2", Qty: 200, Px: 100}), 5)
	if !errors.Is(err, ErrOverfill) || changed {
		t.Fatalf("want ErrOverfill, got %v", err)
	}
	if p.FilledQty != 0 || p.ReservedQty != 300 || c.Status != domain.ChildSent || len(b.Fills(p.ID)) != 0 {
		t.Fatalf("state mutated: parent %+v child %+v", p, c)
	}
}

func TestTimeoutKeepsReservationUntilLateReport(t *testing.T) {
	b, p := setup(t, 1000)
	c := child(t, b, p, 1000)
	if !b.Timeout(c.ID) || b.Timeout(c.ID) {
		t.Fatal("timeout should apply exactly once")
	}
	if c.Status != domain.ChildTimedOut || Available(p) != 0 || b.InFlight(p.ID) != 0 {
		t.Fatalf("timed-out child must stay reserved: %+v avail %d", c, Available(p))
	}
	// Parent expires while the outcome is unknown; the late fill still lands and cannot overfill.
	b.FinishParent(p.ID, domain.ParentExpired, "deadline", 9)
	if _, changed, err := b.ApplyReport(report(c, domain.VenueFill{ID: "f1", Qty: 700, Px: 100}), 10); !changed || err != nil {
		t.Fatalf("late report: %v %v", changed, err)
	}
	if p.FilledQty != 700 || p.ReservedQty != 0 || c.Status != domain.ChildPartial || !c.TimedOut || p.Status != domain.ParentExpired {
		t.Fatalf("parent %+v child %+v", p, c)
	}
}

func TestRejectAndUnfilledReleaseReservation(t *testing.T) {
	b, p := setup(t, 1000)
	c := child(t, b, p, 500)
	rep := report(c)
	rep.Status, rep.Reason = domain.ReportRejected, "VENUE_REJECT"
	b.ApplyReport(rep, 2)
	c2 := child(t, b, p, 500)
	b.ApplyReport(report(c2), 3)
	if c.Status != domain.ChildRejected || c2.Status != domain.ChildUnfilled || Available(p) != 1000 {
		t.Fatalf("c %+v c2 %+v avail %d", c, c2, Available(p))
	}
	if _, _, err := b.ApplyReport(domain.ExecReport{ChildID: "nope", Venue: "A"}, 4); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("want ErrUnknownOrder, got %v", err)
	}
	wrongVenue := report(child(t, b, p, 100))
	wrongVenue.Venue = "B"
	if _, _, err := b.ApplyReport(wrongVenue, 4); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("report from wrong venue accepted: %v", err)
	}
}
