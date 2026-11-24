// Package domain holds the canonical order, quote and fill schemas.
//
// Units (integers only on the execution path):
//   - prices:   ticks        (1 tick = 0.01 currency units)
//   - money:    mills        (1 tick = 100 mills), used for fees and costs
//   - quantity: shares, always a multiple of the configured lot size
//   - time:     Unix milliseconds, UTC. Venue event time (`asof_ms`,
//     `venue_ms`) is kept separate from local receive time (`recv_ms`).
package domain

const MillsPerTick = 100

type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

func (s Side) Valid() bool { return s == Buy || s == Sell }

// Sign is +1 for buys and -1 for sells.
func (s Side) Sign() int64 {
	if s == Sell {
		return -1
	}
	return 1
}

type Level struct {
	Px  int64 `json:"px"`
	Qty int64 `json:"qty"`
}

// VenueQuote is the canonical depth snapshot published by a venue.
type VenueQuote struct {
	Venue  string  `json:"venue"`
	Symbol string  `json:"symbol"`
	Seq    uint64  `json:"seq"`
	Bids   []Level `json:"bids"` // best first
	Asks   []Level `json:"asks"` // best first
	AsOfMs int64   `json:"asof_ms"`
	RecvMs int64   `json:"recv_ms"`
}

type ParentStatus string

const (
	ParentWorking   ParentStatus = "WORKING"
	ParentFilled    ParentStatus = "FILLED"
	ParentExpired   ParentStatus = "EXPIRED"
	ParentCancelled ParentStatus = "CANCELLED"
	ParentRejected  ParentStatus = "REJECTED"
)

const (
	StrategyTWAP      = "TWAP"
	StrategyLiquidity = "LIQUIDITY"
)

type ParentOrder struct {
	ID              string       `json:"id"`
	Symbol          string       `json:"symbol"`
	Side            Side         `json:"side"`
	TargetQty       int64        `json:"target_qty"`
	Strategy        string       `json:"strategy"`
	StartMs         int64        `json:"start_ms"`
	DeadlineMs      int64        `json:"deadline_ms"`
	IntervalMs      int64        `json:"interval_ms"`
	Status          ParentStatus `json:"status"`
	FilledQty       int64        `json:"filled_qty"`
	ReservedQty     int64        `json:"reserved_qty"` // leaves of unresolved children
	ArrivalMidMills int64        `json:"arrival_mid_mills"`
	Reason          string       `json:"reason,omitempty"`
	DoneMs          int64        `json:"done_ms,omitempty"`
}

type ChildStatus string

const (
	ChildSent     ChildStatus = "SENT"
	ChildTimedOut ChildStatus = "TIMED_OUT" // outcome unknown; quantity stays reserved
	ChildFilled   ChildStatus = "FILLED"
	ChildPartial  ChildStatus = "PARTIALLY_FILLED" // IOC remainder cancelled
	ChildUnfilled ChildStatus = "UNFILLED"         // IOC found no liquidity at limit
	ChildRejected ChildStatus = "REJECTED"
)

func (s ChildStatus) Terminal() bool { return s != ChildSent && s != ChildTimedOut }

// ChildOrder is an immediate-or-cancel marketable limit order routed to one venue.
type ChildOrder struct {
	ID          string      `json:"id"`
	ParentID    string      `json:"parent_id"`
	Venue       string      `json:"venue"`
	Side        Side        `json:"side"`
	Qty         int64       `json:"qty"`
	LimitPx     int64       `json:"limit_px"`
	Status      ChildStatus `json:"status"`
	FilledQty   int64       `json:"filled_qty"`
	SnapshotID  string      `json:"snapshot_id"`
	RefMidMills int64       `json:"ref_mid_mills"`
	Attempt     int         `json:"attempt"`
	SentMs      int64       `json:"sent_ms"`
	DoneMs      int64       `json:"done_ms,omitempty"`
	TimedOut    bool        `json:"timed_out,omitempty"`
	Reason      string      `json:"reason,omitempty"`
}

type Fill struct {
	ID       string `json:"id"`
	ChildID  string `json:"child_id"`
	ParentID string `json:"parent_id"`
	Venue    string `json:"venue"`
	Qty      int64  `json:"qty"`
	Px       int64  `json:"px"`
	FeeMills int64  `json:"fee_mills"`
	VenueMs  int64  `json:"venue_ms"`
	RecvMs   int64  `json:"recv_ms"`
}

// VenueFill is a fill as reported by a venue, before OMS enrichment.
type VenueFill struct {
	ID       string `json:"id"`
	Qty      int64  `json:"qty"`
	Px       int64  `json:"px"`
	FeeMills int64  `json:"fee_mills"`
}

const (
	ReportDone     = "DONE"
	ReportRejected = "REJECTED"
)

// ExecReport is the single terminal report a venue returns for an IOC child.
type ExecReport struct {
	ChildID string      `json:"child_id"`
	Venue   string      `json:"venue"`
	Status  string      `json:"status"`
	Reason  string      `json:"reason,omitempty"`
	Fills   []VenueFill `json:"fills"`
	VenueMs int64       `json:"venue_ms"`
}

type Allocation struct {
	Venue   string `json:"venue"`
	Qty     int64  `json:"qty"`
	LimitPx int64  `json:"limit_px"`
}

// TraceEntry explains why a candidate was or was not used by the planner.
type TraceEntry struct {
	Venue     string `json:"venue"`
	Px        int64  `json:"px,omitempty"`
	AvailQty  int64  `json:"avail_qty,omitempty"`
	CostMills int64  `json:"cost_mills,omitempty"`
	TakenQty  int64  `json:"taken_qty"`
	Reason    string `json:"reason"`
}

type RouteDecision struct {
	ParentID      string       `json:"parent_id"`
	SnapshotID    string       `json:"snapshot_id"`
	PolicyVersion string       `json:"policy_version"`
	DecidedMs     int64        `json:"decided_ms"`
	WantQty       int64        `json:"want_qty"`
	Allocations   []Allocation `json:"allocations"`
	Trace         []TraceEntry `json:"trace"`
}
