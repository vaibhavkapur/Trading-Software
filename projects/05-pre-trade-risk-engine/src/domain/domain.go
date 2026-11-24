// Package domain holds the shared vocabulary: requests, decisions, reason
// codes, reservations, kill switches and the audit events that are the only
// way engine state ever changes.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"riskengine/src/fixed"
)

type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

type Result string

const (
	Allow  Result = "ALLOW"
	Reject Result = "REJECT"
	Review Result = "REVIEW"
)

// Reason codes are a stable contract: never rename or reuse one.
const (
	// Hard rejects.
	MalformedRequest     = "MALFORMED_REQUEST"
	InvalidOrderID       = "INVALID_ORDER_ID"
	InvalidAccountID     = "INVALID_ACCOUNT_ID"
	InvalidSymbol        = "INVALID_SYMBOL"
	InvalidSide          = "INVALID_SIDE"
	InvalidQty           = "INVALID_QTY"
	InvalidPrice         = "INVALID_PRICE"
	DuplicateOrderID     = "DUPLICATE_ORDER_ID"
	PolicyUnavailable    = "POLICY_UNAVAILABLE"
	UnknownAccount       = "UNKNOWN_ACCOUNT"
	UnknownSymbol        = "UNKNOWN_SYMBOL"
	KillSwitchGlobal     = "KILL_SWITCH_GLOBAL"
	KillSwitchAccount    = "KILL_SWITCH_ACCOUNT"
	KillSwitchSymbol     = "KILL_SWITCH_SYMBOL"
	InstrumentNotTrading = "INSTRUMENT_NOT_TRADABLE"
	SessionClosed        = "SESSION_CLOSED"
	LotSizeViolation     = "LOT_SIZE_VIOLATION"
	TickSizeViolation    = "TICK_SIZE_VIOLATION"
	NoMarketData         = "NO_MARKET_DATA"
	StaleQuote           = "STALE_QUOTE"
	QuoteClockSkew       = "QUOTE_CLOCK_SKEW"
	PriceCollar          = "PRICE_COLLAR"
	MaxOrderQty          = "MAX_ORDER_QTY"
	MaxOrderNotional     = "MAX_ORDER_NOTIONAL"
	RateLimit            = "RATE_LIMIT"
	MaxDailyLoss         = "MAX_DAILY_LOSS"
	MaxSymbolExposure    = "MAX_SYMBOL_EXPOSURE"
	MaxGrossExposure     = "MAX_GROSS_EXPOSURE"
	AuditUnavailable     = "AUDIT_UNAVAILABLE"
	// Manual-review triggers.
	ReviewOrderNotional = "REVIEW_ORDER_NOTIONAL"
	ExposureMarkStale   = "EXPOSURE_MARK_STALE"
)

// RiskRequest is a proposed order. LimitPrice is a pointer so that a missing
// price is distinguishable from zero.
type RiskRequest struct {
	OrderID    string         `json:"order_id"`
	AccountID  string         `json:"account_id"`
	Symbol     string         `json:"symbol"`
	Side       Side           `json:"side"`
	Qty        int64          `json:"qty"`
	LimitPrice *fixed.Decimal `json:"limit_price"`
}

// Fingerprint identifies the economic content of a request, so a redelivery
// of the same order can be told apart from order_id reuse.
func (r RiskRequest) Fingerprint() string {
	return HashJSON(r)
}

// HashJSON returns the hex SHA-256 of v's JSON encoding. encoding/json emits
// struct fields in declaration order and map keys sorted, so this is stable.
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("domain: unhashable value: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Check is one line of a decision's reason trace. Scope/Name/Threshold are
// the RiskLimit that was applied; Observed is the value it was compared with.
type Check struct {
	Name      string `json:"name"`
	Outcome   string `json:"outcome"` // PASS, REJECT, REVIEW, SKIPPED
	Code      string `json:"code,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Observed  string `json:"observed,omitempty"`
	Threshold string `json:"threshold,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type ReservationStatus string

const (
	PendingReview ReservationStatus = "PENDING_REVIEW" // capacity held, awaiting approval
	Reserved      ReservationStatus = "RESERVED"       // lease; OMS must commit before expires_at
	Committed     ReservationStatus = "COMMITTED"      // order is live; held until filled or released
	Released      ReservationStatus = "RELEASED"
)

// RiskReservation is capacity held against an account's limits for an order
// that has been allowed (or is awaiting review) but has not fully filled.
type RiskReservation struct {
	OrderID       string            `json:"order_id"`
	AccountID     string            `json:"account_id"`
	Symbol        string            `json:"symbol"`
	Side          Side              `json:"side"`
	Quantity      int64             `json:"quantity"`
	Remaining     int64             `json:"remaining"`
	Price         fixed.Decimal     `json:"price"`
	Notional      fixed.Decimal     `json:"notional"` // Remaining * Price
	Status        ReservationStatus `json:"status"`
	ExpiresAt     time.Time         `json:"expires_at"`
	ReleaseReason string            `json:"release_reason,omitempty"`
}

// Seal is the audit envelope of the record that carries a decision or event.
type Seal struct {
	Seq       uint64 `json:"audit_seq,omitempty"` // starts at 1
	Hash      string `json:"record_hash,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// RiskDecision is the response to an evaluation.
type RiskDecision struct {
	OrderID       string    `json:"order_id"`
	Decision      Result    `json:"decision"`
	ReasonCodes   []string  `json:"reason_codes"`
	PolicyVersion string    `json:"policy_version"`
	PolicyHash    string    `json:"policy_hash"`
	SnapshotHash  string    `json:"snapshot_hash"`
	DecidedAt     time.Time `json:"decided_at"`
	// Trace is present on a fresh decision. Replayed and looked-up
	// decisions omit it; the full trace is in audit record audit_seq.
	Trace       []Check          `json:"trace,omitempty"`
	Reservation *RiskReservation `json:"reservation,omitempty"`
	// Set after the audit record is written; never part of the hashed event.
	Seal
	Replayed bool `json:"replayed,omitempty"`
}

type KillScope string

const (
	ScopeGlobal  KillScope = "global"
	ScopeAccount KillScope = "account"
	ScopeSymbol  KillScope = "symbol"
)

type KillSwitch struct {
	Scope       KillScope `json:"scope"`
	Target      string    `json:"target,omitempty"` // account id or symbol; empty for global
	Enabled     bool      `json:"enabled"`
	ActivatedBy string    `json:"activated_by"`
	ActivatedAt time.Time `json:"activated_at"`
	Reason      string    `json:"reason,omitempty"`
}

func (k KillSwitch) Key() string { return string(k.Scope) + ":" + k.Target }

// QuoteView is the market data a decision relied on.
type QuoteView struct {
	Bid         fixed.Decimal `json:"bid"`
	Ask         fixed.Decimal `json:"ask"`
	EventTime   time.Time     `json:"event_time"`   // venue time
	ReceiveTime time.Time     `json:"receive_time"` // local receive time
}

// Snapshot is every input a decision depended on. Its hash is returned to the
// caller, and the snapshot itself is stored in the audit record.
type Snapshot struct {
	Request       RiskRequest     `json:"request"`
	EvaluatedAt   time.Time       `json:"evaluated_at"`
	PolicyVersion string          `json:"policy_version"`
	PolicyHash    string          `json:"policy_hash"`
	Limits        json.RawMessage `json:"limits,omitempty"`
	Quote         *QuoteView      `json:"quote,omitempty"`
	KillSwitches  []string        `json:"kill_switches,omitempty"`
	Account       *AccountView    `json:"account,omitempty"`
}

type AccountView struct {
	PositionQty      int64         `json:"position_qty"`
	SymbolExposure   fixed.Decimal `json:"symbol_exposure"`
	GrossExposure    fixed.Decimal `json:"gross_exposure"`
	RealizedPnLToday fixed.Decimal `json:"realized_pnl_today"`
	OrdersInWindow   int           `json:"orders_in_window"`
	OpenReservations int           `json:"open_reservations"`
	StaleMarks       []string      `json:"stale_marks,omitempty"`
}

// Event types. Every state mutation is one of these, appended to the audit
// log before it is applied; replaying the log rebuilds the state exactly.
const (
	EvDecision  = "DECISION"
	EvApprove   = "RESERVATION_APPROVED"
	EvCommit    = "RESERVATION_COMMITTED"
	EvRelease   = "RESERVATION_RELEASED"
	EvFill      = "FILL"
	EvKill      = "KILL_SWITCH"
	EvPolicy    = "POLICY_LOADED"
	EvReconcile = "RECONCILIATION"
	EvMalformed = "MALFORMED_REQUEST"
)

type Event struct {
	Type      string          `json:"type"`
	At        time.Time       `json:"at"`
	Decision  *DecisionEvent  `json:"decision,omitempty"`
	Approve   *ApproveEvent   `json:"approve,omitempty"`
	Commit    *CommitEvent    `json:"commit,omitempty"`
	Release   *ReleaseEvent   `json:"release,omitempty"`
	Fill      *FillEvent      `json:"fill,omitempty"`
	Kill      *KillSwitch     `json:"kill_switch,omitempty"`
	Policy    *PolicyEvent    `json:"policy,omitempty"`
	Reconcile *ReconcileEvent `json:"reconcile,omitempty"`
	Malformed *MalformedEvent `json:"malformed,omitempty"`
}

type DecisionEvent struct {
	Decision    RiskDecision `json:"decision"`
	Snapshot    Snapshot     `json:"snapshot"`
	Fingerprint string       `json:"fingerprint"`
	// Register is true when the order_id is recorded for duplicate detection
	// and counted against the account's rate limit.
	Register bool `json:"register"`
	// Mark is the reference price observed for the symbol, kept as the
	// fallback valuation if its quote later goes stale.
	Mark fixed.Decimal `json:"mark,omitempty"`
}

type ApproveEvent struct {
	OrderID    string    `json:"order_id"`
	ApprovedBy string    `json:"approved_by"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type CommitEvent struct {
	OrderID string `json:"order_id"`
}

type ReleaseEvent struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}

type FillEvent struct {
	OrderID string        `json:"order_id"`
	FillID  string        `json:"fill_id"`
	Qty     int64         `json:"qty"`
	Price   fixed.Decimal `json:"price"`
}

type PolicyEvent struct {
	Version string `json:"version"`
	Hash    string `json:"hash"`
	Source  string `json:"source,omitempty"`
}

type ReconcileEvent struct {
	AccountID string           `json:"account_id"`
	Drift     map[string]int64 `json:"drift"` // engine qty minus broker qty, non-zero only
	AbsDrift  int64            `json:"abs_drift"`
}

type MalformedEvent struct {
	BodySHA256 string `json:"body_sha256"`
	Error      string `json:"error"`
}
