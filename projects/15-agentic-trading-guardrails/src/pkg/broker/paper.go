package broker

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

const EnvironmentPaperSimulated = "PAPER_SIMULATED"

var (
	ErrBrokerDisconnected = errors.New("paper broker adapter disconnected (fail-closed)")
	ErrSymbolNotFound     = errors.New("symbol not available in paper market feed")
	ErrAccountNotFound    = errors.New("paper account not found")
	ErrInsufficientCash   = errors.New("insufficient paper buying power")
	ErrInsufficientShares = errors.New("insufficient paper position quantity to sell")
	ErrUnverifiedWrite    = errors.New("broker write blocked: missing Policy Enforcement Point (PEP) verification permit")
	ErrBrokerRejected     = errors.New("paper broker rejected order")
	ErrLiveModeDisabled   = errors.New("real-money live trading is explicitly disabled; paper-only mode enforced")
)

// ExecutionPermit is an opaque token that proves an order passed the single Policy Enforcement Point (PEP).
type ExecutionPermit struct {
	decisionID     string
	policyVersion  string
	approvalID     string
	correlationID  string
	idempotencyKey string
	sealed         bool
}

// IssueExecutionPermit constructs a sealed PEP permit. Only the gateway PEP calls this after all checks succeed.
func IssueExecutionPermit(decisionID, policyVersion, approvalID, correlationID, idempotencyKey string) ExecutionPermit {
	return ExecutionPermit{
		decisionID:     decisionID,
		policyVersion:  policyVersion,
		approvalID:     approvalID,
		correlationID:  correlationID,
		idempotencyKey: idempotencyKey,
		sealed:         decisionID != "" && policyVersion != "",
	}
}

func (p ExecutionPermit) IsValid() bool {
	return p.sealed && p.decisionID != "" && p.policyVersion != ""
}

// Position holds an exact integer share count and average cost in PriceTicks.
type Position struct {
	Symbol        string           `json:"symbol"`
	Quantity      int64            `json:"quantity"`
	AvgPriceTicks types.PriceTicks `json:"avg_price_ticks"`
}

// PortfolioSnapshot represents a read-only view of a simulated paper account.
type PortfolioSnapshot struct {
	AccountID   string              `json:"account_id"`
	CashCents   types.MoneyCents    `json:"cash_cents"`
	Positions   map[string]Position `json:"positions"`
	Environment string              `json:"environment"`
	AsOfUTC     time.Time           `json:"as_of_utc"`
}

// MarketDataSnapshot extends MarketQuote with optional simulated commentary (useful for prompt-injection tests).
type MarketDataSnapshot struct {
	Quote       types.MarketQuote `json:"quote"`
	VenueNotes  string            `json:"venue_notes,omitempty"`
	Environment string            `json:"environment"`
}

// PaperAdapter is an isolated paper-only broker and deterministic market simulator.
// Credentials stay strictly inside unexported fields and are never returned or logged.
type PaperAdapter struct {
	mu           sync.RWMutex
	clock        types.Clock
	apiKey       string // unexported; loaded from env/secret manager
	apiSecret    string // unexported; never serialized or exposed
	disconnected bool
	rejectNext   string
	orderSeq     uint64

	quotes         map[string]types.PriceTicks
	quoteStaleness map[string]time.Duration
	venueNotes     map[string]string
	accounts       map[string]*PortfolioSnapshot
	idempotency    map[string]types.BrokerOrderResponse
}

func NewPaperAdapter(clock types.Clock) (*PaperAdapter, error) {
	if clock == nil {
		clock = types.SystemClock{}
	}
	if strings.EqualFold(os.Getenv("BROKER_MODE"), "LIVE") {
		return nil, ErrLiveModeDisabled
	}

	// Credentials injected strictly from environment (isolated inside execution-proxy)
	apiKey := os.Getenv("PAPER_BROKER_API_KEY")
	if apiKey == "" {
		apiKey = "paper-sim-local-key"
	}
	apiSecret := os.Getenv("PAPER_BROKER_API_SECRET")
	if apiSecret == "" {
		apiSecret = "paper-sim-local-secret"
	}

	now := clock.Now()
	p := &PaperAdapter{
		clock:          clock,
		apiKey:         apiKey,
		apiSecret:      apiSecret,
		quotes:         make(map[string]types.PriceTicks),
		quoteStaleness: make(map[string]time.Duration),
		venueNotes:     make(map[string]string),
		accounts:       make(map[string]*PortfolioSnapshot),
		idempotency:    make(map[string]types.BrokerOrderResponse),
	}

	// Deterministic paper market fixtures (PriceTicks = USD * 10,000)
	p.quotes["SPY"] = 510_0000  // $510.0000
	p.quotes["QQQ"] = 440_0000  // $440.0000
	p.quotes["AAPL"] = 225_0000 // $225.0000
	p.quotes["MSFT"] = 420_0000 // $420.0000
	p.quotes["NVDA"] = 130_0000 // $130.0000
	p.quotes["TLT"] = 95_0000   // $95.0000
	p.quotes["MEME"] = 12_5000  // $12.5000 (explicitly denied by default policy)

	// Default paper accounts
	p.accounts["a-1"] = &PortfolioSnapshot{
		AccountID:   "a-1",
		CashCents:   types.MoneyCents(10_000_000), // $100,000.00 paper cash
		Positions:   map[string]Position{"SPY": {Symbol: "SPY", Quantity: 10, AvgPriceTicks: 500_0000}},
		Environment: EnvironmentPaperSimulated,
		AsOfUTC:     now,
	}
	p.accounts["a-2"] = &PortfolioSnapshot{
		AccountID:   "a-2",
		CashCents:   types.MoneyCents(5_000_000), // $50,000.00 paper cash
		Positions:   map[string]Position{"QQQ": {Symbol: "QQQ", Quantity: 5, AvgPriceTicks: 430_0000}},
		Environment: EnvironmentPaperSimulated,
		AsOfUTC:     now,
	}

	return p, nil
}

// --- Fault injection & simulation controls (for tests and failure demos) ---

func (p *PaperAdapter) SetDisconnected(disconnected bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disconnected = disconnected
}

func (p *PaperAdapter) SetQuoteStaleness(symbol string, age time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if age == 0 {
		delete(p.quoteStaleness, sym)
	} else {
		p.quoteStaleness[sym] = age
	}
}

func (p *PaperAdapter) SetQuotePrice(symbol string, priceTicks types.PriceTicks) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quotes[strings.ToUpper(strings.TrimSpace(symbol))] = priceTicks
}

func (p *PaperAdapter) SetVenueNote(symbol, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.venueNotes[strings.ToUpper(strings.TrimSpace(symbol))] = note
}

func (p *PaperAdapter) SetRejectNext(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rejectNext = reason
}

// GetQuote returns a simulated paper quote with distinct VenueTimeUTC and ReceivedAtUTC.
func (p *PaperAdapter) GetQuote(symbol string) (MarketDataSnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.disconnected {
		return MarketDataSnapshot{}, ErrBrokerDisconnected
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	last, ok := p.quotes[sym]
	if !ok {
		return MarketDataSnapshot{}, fmt.Errorf("%w: %s", ErrSymbolNotFound, sym)
	}
	received := p.clock.Now()
	stale := p.quoteStaleness[sym]
	venueTime := received.Add(-stale)

	spread := types.PriceTicks(500) // $0.0500 simulated spread
	bid := last - spread
	if bid <= 0 {
		bid = 1
	}
	ask := last + spread

	return MarketDataSnapshot{
		Quote: types.MarketQuote{
			Symbol:        sym,
			BidTicks:      bid,
			AskTicks:      ask,
			LastTicks:     last,
			VenueTimeUTC:  venueTime.UTC(),
			ReceivedAtUTC: received.UTC(),
			Source:        "paper-feed-sim-v1",
			Simulated:     true,
		},
		VenueNotes:  p.venueNotes[sym],
		Environment: EnvironmentPaperSimulated,
	}, nil
}

// GetPortfolio returns a copy of a paper account's portfolio state.
func (p *PaperAdapter) GetPortfolio(accountID string) (PortfolioSnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.disconnected {
		return PortfolioSnapshot{}, ErrBrokerDisconnected
	}
	acc, ok := p.accounts[accountID]
	if !ok {
		return PortfolioSnapshot{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	posCopy := make(map[string]Position, len(acc.Positions))
	for k, v := range acc.Positions {
		posCopy[k] = v
	}
	return PortfolioSnapshot{
		AccountID:   acc.AccountID,
		CashCents:   acc.CashCents,
		Positions:   posCopy,
		Environment: EnvironmentPaperSimulated,
		AsOfUTC:     p.clock.Now(),
	}, nil
}

// SubmitOrder executes a simulated paper fill.
// Requires a valid PEP ExecutionPermit; direct unverified writes fail closed.
func (p *PaperAdapter) SubmitOrder(permit ExecutionPermit, proposal types.OrderProposal) (types.BrokerOrderResponse, error) {
	if !permit.IsValid() {
		return types.BrokerOrderResponse{}, ErrUnverifiedWrite
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.disconnected {
		return types.BrokerOrderResponse{}, ErrBrokerDisconnected
	}
	if p.rejectNext != "" {
		reason := p.rejectNext
		p.rejectNext = ""
		return types.BrokerOrderResponse{}, fmt.Errorf("%w: %s", ErrBrokerRejected, reason)
	}

	// Idempotent duplicate delivery handling
	if permit.idempotencyKey != "" {
		if prev, exists := p.idempotency[permit.idempotencyKey]; exists {
			prev.DuplicateReplay = true
			return prev, nil
		}
	}

	acc, ok := p.accounts[proposal.AccountID]
	if !ok {
		return types.BrokerOrderResponse{}, fmt.Errorf("%w: %s", ErrAccountNotFound, proposal.AccountID)
	}

	sym := strings.ToUpper(strings.TrimSpace(proposal.Symbol))
	if _, ok := p.quotes[sym]; !ok {
		return types.BrokerOrderResponse{}, fmt.Errorf("%w: %s", ErrSymbolNotFound, sym)
	}

	fillTicks := proposal.LimitPriceTicks
	notionalCents, err := types.ComputeNotionalCents(proposal.Quantity, fillTicks)
	if err != nil {
		return types.BrokerOrderResponse{}, err
	}

	switch proposal.Side {
	case types.SideBuy:
		if acc.CashCents < notionalCents {
			return types.BrokerOrderResponse{}, fmt.Errorf("%w: need %s, have %s",
				ErrInsufficientCash, notionalCents.FormatUSD(), acc.CashCents.FormatUSD())
		}
		acc.CashCents -= notionalCents
		pos := acc.Positions[sym]
		pos.Symbol = sym
		pos.Quantity += proposal.Quantity
		pos.AvgPriceTicks = fillTicks
		acc.Positions[sym] = pos
	case types.SideSell:
		pos := acc.Positions[sym]
		if pos.Quantity < proposal.Quantity {
			return types.BrokerOrderResponse{}, fmt.Errorf("%w: sell %d > held %d",
				ErrInsufficientShares, proposal.Quantity, pos.Quantity)
		}
		pos.Quantity -= proposal.Quantity
		if pos.Quantity == 0 {
			delete(acc.Positions, sym)
		} else {
			acc.Positions[sym] = pos
		}
		newCash, err := types.SafeAddCents(acc.CashCents, notionalCents)
		if err != nil {
			return types.BrokerOrderResponse{}, err
		}
		acc.CashCents = newCash
	default:
		return types.BrokerOrderResponse{}, fmt.Errorf("%w: unsupported side %q", ErrBrokerRejected, proposal.Side)
	}

	now := p.clock.Now()
	acc.AsOfUTC = now
	p.orderSeq++
	resp := types.BrokerOrderResponse{
		BrokerOrderID:   fmt.Sprintf("paper-ord-%06d", p.orderSeq),
		ProposalID:      proposal.ID,
		AccountID:       proposal.AccountID,
		Symbol:          sym,
		Side:            proposal.Side,
		OrderType:       proposal.OrderType,
		Quantity:        proposal.Quantity,
		FillPriceTicks:  fillTicks,
		NotionalCents:   notionalCents,
		Status:          "FILLED_PAPER",
		Environment:     EnvironmentPaperSimulated,
		VenueTimeUTC:    now,
		ReceivedAtUTC:   now,
		IdempotencyKey:  permit.idempotencyKey,
		DuplicateReplay: false,
	}

	if permit.idempotencyKey != "" {
		p.idempotency[permit.idempotencyKey] = resp
	}
	return resp, nil
}
