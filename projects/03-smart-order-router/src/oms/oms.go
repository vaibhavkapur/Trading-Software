// Package oms owns parent and child order state. Its core invariant:
//
//	parent.FilledQty + parent.ReservedQty <= parent.TargetQty
//
// Every child reserves its full quantity when created and the reservation is
// released only by a fill or by a *confirmed* terminal report. A child whose
// report never arrived (TIMED_OUT) keeps its reservation, so a late fill can
// never push the parent past its target.
package oms

import (
	"errors"
	"fmt"

	"sor/src/domain"
)

var (
	ErrUnknownOrder = errors.New("unknown order")
	ErrNotWorking   = errors.New("parent is not working")
	ErrOverfill     = errors.New("fill would exceed order quantity")
	ErrBadQty       = errors.New("child quantity exceeds available parent quantity")
)

type Book struct {
	parents   map[string]*domain.ParentOrder
	parentIDs []string
	children  map[string]*domain.ChildOrder
	byParent  map[string][]string
	fills     map[string][]domain.Fill // by parent
	seenFill  map[string]bool
}

func NewBook() *Book {
	return &Book{parents: map[string]*domain.ParentOrder{}, children: map[string]*domain.ChildOrder{},
		byParent: map[string][]string{}, fills: map[string][]domain.Fill{}, seenFill: map[string]bool{}}
}

func (b *Book) AddParent(p domain.ParentOrder) *domain.ParentOrder {
	p.ID = fmt.Sprintf("P-%06d", len(b.parentIDs)+1)
	b.parents[p.ID] = &p
	b.parentIDs = append(b.parentIDs, p.ID)
	return &p
}

func (b *Book) Parent(id string) *domain.ParentOrder { return b.parents[id] }
func (b *Book) Child(id string) *domain.ChildOrder   { return b.children[id] }
func (b *Book) ParentIDs() []string                  { return b.parentIDs }
func (b *Book) Fills(parentID string) []domain.Fill  { return b.fills[parentID] }

func (b *Book) Children(parentID string) []*domain.ChildOrder {
	out := make([]*domain.ChildOrder, 0, len(b.byParent[parentID]))
	for _, id := range b.byParent[parentID] {
		out = append(out, b.children[id])
	}
	return out
}

// Available is the parent quantity that may still be routed.
func Available(p *domain.ParentOrder) int64 { return p.TargetQty - p.FilledQty - p.ReservedQty }

// InFlight counts children still awaiting a report within their timeout.
func (b *Book) InFlight(parentID string) (n int) {
	for _, id := range b.byParent[parentID] {
		if b.children[id].Status == domain.ChildSent {
			n++
		}
	}
	return n
}

// NewChild creates a SENT child and reserves its quantity against the parent.
func (b *Book) NewChild(parentID string, a domain.Allocation, snapshotID string, refMidMills int64, attempt int, nowMs int64) (*domain.ChildOrder, error) {
	p := b.parents[parentID]
	switch {
	case p == nil:
		return nil, ErrUnknownOrder
	case p.Status != domain.ParentWorking:
		return nil, ErrNotWorking
	case a.Qty <= 0 || a.Qty > Available(p):
		return nil, ErrBadQty
	}
	c := &domain.ChildOrder{ID: fmt.Sprintf("C-%06d", len(b.children)+1), ParentID: parentID, Venue: a.Venue,
		Side: p.Side, Qty: a.Qty, LimitPx: a.LimitPx, Status: domain.ChildSent, SnapshotID: snapshotID,
		RefMidMills: refMidMills, Attempt: attempt, SentMs: nowMs}
	b.children[c.ID] = c
	b.byParent[parentID] = append(b.byParent[parentID], c.ID)
	p.ReservedQty += a.Qty
	return c, nil
}

// Timeout marks a SENT child as TIMED_OUT. Its reservation is kept.
func (b *Book) Timeout(childID string) bool {
	c := b.children[childID]
	if c == nil || c.Status != domain.ChildSent {
		return false
	}
	c.Status, c.TimedOut = domain.ChildTimedOut, true
	return true
}

// ApplyReport applies a venue execution report. It is idempotent: a report
// for an already-terminal child, or a fill id seen before, changes nothing
// (changed=false). A report that would overfill the child or parent is
// refused in full with ErrOverfill.
func (b *Book) ApplyReport(rep domain.ExecReport, nowMs int64) (applied []domain.Fill, changed bool, err error) {
	c := b.children[rep.ChildID]
	if c == nil || c.Venue != rep.Venue {
		return nil, false, ErrUnknownOrder
	}
	if c.Status.Terminal() {
		return nil, false, nil
	}
	p := b.parents[c.ParentID]
	var fresh []domain.VenueFill
	var sum int64
	for _, f := range rep.Fills {
		if f.Qty <= 0 || b.seenFill[rep.Venue+"/"+f.ID] {
			continue
		}
		fresh = append(fresh, f)
		sum += f.Qty
	}
	if c.FilledQty+sum > c.Qty || p.FilledQty+sum > p.TargetQty {
		return nil, false, ErrOverfill
	}
	for _, f := range fresh {
		b.seenFill[rep.Venue+"/"+f.ID] = true
		fill := domain.Fill{ID: f.ID, ChildID: c.ID, ParentID: p.ID, Venue: c.Venue, Qty: f.Qty, Px: f.Px,
			FeeMills: f.FeeMills, VenueMs: rep.VenueMs, RecvMs: nowMs}
		b.fills[p.ID] = append(b.fills[p.ID], fill)
		applied = append(applied, fill)
	}
	p.ReservedQty -= c.Qty - c.FilledQty // release the whole reservation: filled part moves to FilledQty, IOC leaves are dead
	c.FilledQty += sum
	p.FilledQty += sum
	c.DoneMs = nowMs
	switch {
	case rep.Status == domain.ReportRejected && c.FilledQty == 0:
		c.Status, c.Reason = domain.ChildRejected, rep.Reason
	case c.FilledQty == c.Qty:
		c.Status = domain.ChildFilled
	case c.FilledQty > 0:
		c.Status = domain.ChildPartial
	default:
		c.Status = domain.ChildUnfilled
	}
	if p.FilledQty == p.TargetQty && p.Status == domain.ParentWorking {
		p.Status, p.DoneMs = domain.ParentFilled, nowMs
	}
	return applied, true, nil
}

// FinishParent moves a WORKING parent to a terminal status. Fills from
// children already in flight are still applied afterwards.
func (b *Book) FinishParent(id string, status domain.ParentStatus, reason string, nowMs int64) bool {
	p := b.parents[id]
	if p == nil || p.Status != domain.ParentWorking {
		return false
	}
	p.Status, p.Reason, p.DoneMs = status, reason, nowMs
	return true
}
