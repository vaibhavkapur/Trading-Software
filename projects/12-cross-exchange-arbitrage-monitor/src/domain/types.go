// Package domain defines the core entities, invariants, and value objects
// for the Cross-Exchange Arbitrage Monitor.
package domain

import (
	"time"

	"arbmonitor/src/fixed"
)

const (
	// PaperDisclaimer is attached to every alert and API opportunity response.
	PaperDisclaimer = "Alert: paper estimate, not an executable trade guarantee."
	// DefaultRuleVersion versions the net-edge and freshness calculation logic.
	DefaultRuleVersion = "edge-v1.0.0"
)

// PriceLevel is a single aggregated order-book level in exact fixed-point units.
type PriceLevel struct {
	Price fixed.Decimal `json:"price"`
	Qty   fixed.Decimal `json:"qty"`
}

// VenueStatus describes the connectivity and sequence health of a venue feed.
type VenueStatus string

const (
	VenueStatusHealthy      VenueStatus = "healthy"
	VenueStatusDisconnected VenueStatus = "disconnected"
	VenueStatusResyncing    VenueStatus = "resyncing"
	VenueStatusStale        VenueStatus = "stale"
)

// VenueBook is a normalized top-N order book snapshot for a single venue and instrument.
// Venue event timestamp (Asof) and local receipt timestamp (ReceivedAt) are recorded
// separately in UTC so clock skew and network delay are never conflated.
type VenueBook struct {
	Venue      string       `json:"venue"`
	Instrument string       `json:"instrument"`
	QuoteAsset string       `json:"quote_asset"`
	Bids       []PriceLevel `json:"bids"` // sorted descending by price
	Asks       []PriceLevel `json:"asks"` // sorted ascending by price
	Asof       time.Time    `json:"asof"`
	ReceivedAt time.Time    `json:"received_at"`
	Seq        uint64       `json:"seq"`
	Status     VenueStatus  `json:"status"`
}

// BestBid returns the top bid level and whether bids are non-empty.
func (b VenueBook) BestBid() (PriceLevel, bool) {
	if len(b.Bids) == 0 {
		return PriceLevel{}, false
	}
	return b.Bids[0], true
}

// BestAsk returns the top ask level and whether asks are non-empty.
func (b VenueBook) BestAsk() (PriceLevel, bool) {
	if len(b.Asks) == 0 {
		return PriceLevel{}, false
	}
	return b.Asks[0], true
}

// Clone returns a deep copy of VenueBook so stored snapshots cannot be mutated.
func (b VenueBook) Clone() VenueBook {
	out := b
	if b.Bids != nil {
		out.Bids = make([]PriceLevel, len(b.Bids))
		copy(out.Bids, b.Bids)
	}
	if b.Asks != nil {
		out.Asks = make([]PriceLevel, len(b.Asks))
		copy(out.Asks, b.Asks)
	}
	return out
}

// InstrumentSpec defines canonical instrument metadata, precision rules, and venue symbol mappings.
type InstrumentSpec struct {
	Canonical    string            `json:"canonical"`     // e.g. "BTC/USDT"
	Base         string            `json:"base"`          // e.g. "BTC"
	Quote        string            `json:"quote"`         // e.g. "USDT"
	TickSize     fixed.Decimal     `json:"tick_size"`     // price increment
	LotSize      fixed.Decimal     `json:"lot_size"`      // quantity increment
	MinNotional  fixed.Decimal     `json:"min_notional"`  // minimum order notional in quote
	MaxDepth     int               `json:"max_depth"`     // top-N levels to keep
	VenueSymbols map[string]string `json:"venue_symbols"` // venue -> venue symbol (e.g. binance -> BTCUSDT)
}

// VenueFee defines taker fee schedule for a venue/instrument pair.
type VenueFee struct {
	Venue      string        `json:"venue"`
	Instrument string        `json:"instrument"` // canonical instrument or "*" default
	TakerBps   fixed.Decimal `json:"taker_bps"`  // taker fee in basis points
	MinFee     fixed.Decimal `json:"min_fee"`    // minimum fee per leg in quote currency
	Tier       string        `json:"tier"`       // e.g. "VIP0", "Pro1"
	Version    string        `json:"version"`    // fee schedule version
}

// FXRate converts 1 unit of From currency into Rate units of To currency.
type FXRate struct {
	From       string        `json:"from"`
	To         string        `json:"to"`
	Rate       fixed.Decimal `json:"rate"`       // e.g. 1 USD = 1.0002 USDT
	SpreadBps  fixed.Decimal `json:"spread_bps"` // conversion cost in bps
	Asof       time.Time     `json:"asof"`
	ReceivedAt time.Time     `json:"received_at"`
}

// VenueBalance represents pre-funded available inventory on a single venue.
// Balances cannot be teleported between venues in instantaneous arbitrage.
type VenueBalance struct {
	Venue     string        `json:"venue"`
	Asset     string        `json:"asset"`
	Available fixed.Decimal `json:"available"`
}

// TransferCostEstimate separates off-band withdrawal/rebalancing cost from instantaneous edge.
type TransferCostEstimate struct {
	Asset          string        `json:"asset"`
	WithdrawalFee  fixed.Decimal `json:"withdrawal_fee"`   // in base asset
	EstimatedQuote fixed.Decimal `json:"estimated_quote"`  // withdrawal fee valued in quote currency
	EstConfirmSec  int64         `json:"est_confirm_sec"`  // expected on-chain settlement delay
	IncludedInEdge bool          `json:"included_in_edge"` // always false for instantaneous pre-funded arb
	Note           string        `json:"note"`
}

// DepthSlice records an exact matched slice walked across a buy ask level and sell bid level.
type DepthSlice struct {
	Qty          fixed.Decimal `json:"qty"`
	BuyPrice     fixed.Decimal `json:"buy_price"`      // normalized to common quote currency
	SellPrice    fixed.Decimal `json:"sell_price"`     // normalized to common quote currency
	RawBuyPrice  fixed.Decimal `json:"raw_buy_price"`  // in buy venue native quote
	RawSellPrice fixed.Decimal `json:"raw_sell_price"` // in sell venue native quote
	BuyLevelIdx  int           `json:"buy_level_idx"`
	SellLevelIdx int           `json:"sell_level_idx"`
}

// Opportunity represents a depth-aware cross-exchange price dislocation after all costs.
type Opportunity struct {
	ID                  string                `json:"id"`
	RuleVersion         string                `json:"rule_version"`
	Instrument          string                `json:"instrument"`
	CommonQuote         string                `json:"common_quote"`
	BuyVenue            string                `json:"buy_venue"`
	SellVenue           string                `json:"sell_venue"`
	BuyInstrument       string                `json:"buy_instrument"`
	SellInstrument      string                `json:"sell_instrument"`
	Qty                 fixed.Decimal         `json:"qty"`
	BuyVWAP             fixed.Decimal         `json:"buy_vwap"`
	SellVWAP            fixed.Decimal         `json:"sell_vwap"`
	BuyTopAsk           fixed.Decimal         `json:"buy_top_ask"`
	SellTopBid          fixed.Decimal         `json:"sell_top_bid"`
	ExecutableNotional  fixed.Decimal         `json:"executable_notional"`
	GrossEdgeBps        fixed.Decimal         `json:"gross_edge_bps"`
	NetEdgeBps          fixed.Decimal         `json:"net_edge_bps"`
	GrossProfit         fixed.Decimal         `json:"gross_profit"`
	NetProfit           fixed.Decimal         `json:"net_profit"`
	BuyFee              fixed.Decimal         `json:"buy_fee"`
	SellFee             fixed.Decimal         `json:"sell_fee"`
	FXCost              fixed.Decimal         `json:"fx_cost"`
	ModeledSlippageCost fixed.Decimal         `json:"modeled_slippage_cost"`
	DepthSlippageBps    fixed.Decimal         `json:"depth_slippage_bps"`
	BuyLevelsConsumed   int                   `json:"buy_levels_consumed"`
	SellLevelsConsumed  int                   `json:"sell_levels_consumed"`
	Slices              []DepthSlice          `json:"slices"`
	InventoryCapped     bool                  `json:"inventory_capped"`
	BuyBookAsof         time.Time             `json:"buy_book_asof"`
	BuyBookReceivedAt   time.Time             `json:"buy_book_received_at"`
	BuyBookSeq          uint64                `json:"buy_book_seq"`
	SellBookAsof        time.Time             `json:"sell_book_asof"`
	SellBookReceivedAt  time.Time             `json:"sell_book_received_at"`
	SellBookSeq         uint64                `json:"sell_book_seq"`
	LegSkewMs           int64                 `json:"leg_skew_ms"`
	BuyAgeMs            int64                 `json:"buy_age_ms"`
	SellAgeMs           int64                 `json:"sell_age_ms"`
	Asof                time.Time             `json:"asof"`
	TransferCost        *TransferCostEstimate `json:"transfer_cost,omitempty"`
	Disclaimer          string                `json:"disclaimer"`
}

// AlertStatus tracks the lifecycle of an emitted alert.
type AlertStatus string

const (
	AlertStatusActive  AlertStatus = "active"
	AlertStatusExpired AlertStatus = "expired"
)

// AlertEvidence captures immutable synchronized evidence backing an alert for audit and replay.
type AlertEvidence struct {
	BuyBookSnapshot  VenueBook `json:"buy_book_snapshot"`
	SellBookSnapshot VenueBook `json:"sell_book_snapshot"`
	BuyFee           VenueFee  `json:"buy_fee"`
	SellFee          VenueFee  `json:"sell_fee"`
	FXRateUsed       *FXRate   `json:"fx_rate_used,omitempty"`
	ConfigVersion    int       `json:"config_version"`
	RuleVersion      string    `json:"rule_version"`
}

// Alert is a persisted, auditable paper-only arbitrage alert.
type Alert struct {
	ID            string        `json:"id"`
	OpportunityID string        `json:"opportunity_id"`
	RuleVersion   string        `json:"rule_version"`
	ConfigVersion int           `json:"config_version"`
	TriggeredAt   time.Time     `json:"triggered_at"`
	ExpiryAt      time.Time     `json:"expiry_at"`
	Status        AlertStatus   `json:"status"`
	Opportunity   Opportunity   `json:"opportunity"`
	Evidence      AlertEvidence `json:"evidence"`
	Disclaimer    string        `json:"disclaimer"`
}

// AlertConfig defines runtime alert policy and freshness gates.
type AlertConfig struct {
	Version            int           `json:"version"`
	Instruments        []string      `json:"instruments"`
	MinNetBps          fixed.Decimal `json:"min_net_bps"`
	MaxQuoteAgeMs      int64         `json:"max_quote_age_ms"`
	MaxLegSkewMs       int64         `json:"max_leg_skew_ms"`
	MaxClockSkewMs     int64         `json:"max_clock_skew_ms"`
	CooldownMs         int64         `json:"cooldown_ms"`
	AlertTTLMs         int64         `json:"alert_ttl_ms"`
	MaxDepthLevels     int           `json:"max_depth_levels"`
	ModeledSlippageBps fixed.Decimal `json:"modeled_slippage_bps"`
	EnforceInventory   bool          `json:"enforce_inventory"`
}

// DefaultAlertConfig returns safe conservative defaults.
func DefaultAlertConfig() AlertConfig {
	return AlertConfig{
		Version:            1,
		Instruments:        []string{"BTC/USDT", "ETH/USDT"},
		MinNetBps:          fixed.FromInt(5),
		MaxQuoteAgeMs:      750,
		MaxLegSkewMs:       500,
		MaxClockSkewMs:     1500,
		CooldownMs:         1000,
		AlertTTLMs:         5000,
		MaxDepthLevels:     10,
		ModeledSlippageBps: fixed.MustParse("1.5"),
		EnforceInventory:   true,
	}
}

// VenueHealth summarizes live feed connectivity, staleness, and sequence integrity per venue.
type VenueHealth struct {
	Venue            string      `json:"venue"`
	Status           VenueStatus `json:"status"`
	Connected        bool        `json:"connected"`
	LastAsof         time.Time   `json:"last_asof,omitempty"`
	LastReceivedAt   time.Time   `json:"last_received_at,omitempty"`
	QuoteAgeMs       int64       `json:"quote_age_ms"`
	ClockSkewMs      int64       `json:"clock_skew_ms"`
	LastSeq          uint64      `json:"last_seq"`
	DisconnectCount  int64       `json:"disconnect_count"`
	ReconnectCount   int64       `json:"reconnect_count"`
	SequenceGaps     int64       `json:"sequence_gaps"`
	DuplicateCount   int64       `json:"duplicate_count"`
	OutOfOrderCount  int64       `json:"out_of_order_count"`
	MessagesAccepted int64       `json:"messages_accepted"`
	ActiveBooks      int         `json:"active_books"`
}

// SuppressionReason categorizes why a candidate pair did not emit an opportunity or alert.
type SuppressionReason string

const (
	SuppressNone                  SuppressionReason = ""
	SuppressDisconnected          SuppressionReason = "venue_disconnected"
	SuppressMissingSide           SuppressionReason = "missing_book_side"
	SuppressStaleQuote            SuppressionReason = "stale_quote"
	SuppressClockSkew             SuppressionReason = "clock_skew"
	SuppressLegSkew               SuppressionReason = "leg_timestamp_skew"
	SuppressNoCross               SuppressionReason = "no_positive_spread"
	SuppressInsufficientInventory SuppressionReason = "insufficient_inventory"
	SuppressBelowMinNotional      SuppressionReason = "below_min_notional"
	SuppressBelowNetThreshold     SuppressionReason = "below_net_edge_threshold"
	SuppressMissingFXRate         SuppressionReason = "missing_or_stale_fx_rate"
	SuppressCooldown              SuppressionReason = "alert_cooldown"
	SuppressInstrumentFiltered    SuppressionReason = "instrument_not_configured"
)
