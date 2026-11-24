// Package policy is the policy-loader: versioned, read-only limits and
// reference data. A policy is immutable once loaded; reloads swap the whole
// document atomically.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"riskengine/src/fixed"
)

// MaxThreshold caps every notional limit far below fixed.Max so saturated
// arithmetic always lands above the limit.
const MaxThreshold = fixed.Decimal(1e17)

type RateLimit struct {
	MaxOrders int   `json:"max_orders"`
	WindowMs  int64 `json:"window_ms"`
}

// Limits are the resolved per-account thresholds.
type Limits struct {
	MaxOrderQty         int64         `json:"max_order_qty"`
	MaxOrderNotional    fixed.Decimal `json:"max_order_notional"`
	ReviewOrderNotional fixed.Decimal `json:"review_order_notional"` // 0 disables
	MaxSymbolExposure   fixed.Decimal `json:"max_symbol_exposure"`
	MaxGrossExposure    fixed.Decimal `json:"max_gross_exposure"`
	MaxDailyLoss        fixed.Decimal `json:"max_daily_loss"`
	RateLimit           RateLimit     `json:"rate_limit"`
	QuoteMaxAgeMs       int64         `json:"quote_max_age_ms"`
	MaxClockSkewMs      int64         `json:"max_clock_skew_ms"`
	PriceCollarBps      int64         `json:"price_collar_bps"`
	ReservationTTLMs    int64         `json:"reservation_ttl_ms"`
	ReviewTTLMs         int64         `json:"review_ttl_ms"`
}

type Symbol struct {
	Tradable       bool          `json:"tradable"`
	LotSize        int64         `json:"lot_size"`
	TickSize       fixed.Decimal `json:"tick_size"`
	PriceCollarBps int64         `json:"price_collar_bps,omitempty"` // overrides the account collar when set
}

// Session is a daily UTC trading window. A nil session means always open.
type Session struct {
	OpenUTC  string `json:"open_utc"`  // "13:30"
	CloseUTC string `json:"close_utc"` // "20:00", exclusive
	Weekdays []int  `json:"weekdays"`  // 0=Sunday; empty means every day

	open, close int
}

func (s *Session) IsOpen(t time.Time) bool {
	if s == nil {
		return true
	}
	t = t.UTC()
	if len(s.Weekdays) > 0 {
		ok := false
		for _, d := range s.Weekdays {
			ok = ok || d == int(t.Weekday())
		}
		if !ok {
			return false
		}
	}
	m := t.Hour()*60 + t.Minute()
	return m >= s.open && m < s.close
}

type document struct {
	Version  string                     `json:"version"`
	Defaults Limits                     `json:"defaults"`
	Accounts map[string]json.RawMessage `json:"accounts"`
	Symbols  map[string]Symbol          `json:"symbols"`
	Session  *Session                   `json:"session"`
}

type Policy struct {
	Version string
	Hash    string // SHA-256 of the source bytes
	Symbols map[string]Symbol
	Session *Session

	accounts map[string]Limits
}

// Limits returns the resolved limits for an account. Accounts that are not
// listed have no limits and must be rejected by the caller.
func (p *Policy) Limits(account string) (Limits, bool) {
	l, ok := p.accounts[account]
	return l, ok
}

func (p *Policy) Accounts() []string {
	out := make([]string, 0, len(p.accounts))
	for a := range p.accounts {
		out = append(out, a)
	}
	return out
}

func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Parse validates a policy document. Any unknown field, missing threshold or
// nonsensical value fails the whole load, so a typo can never silently
// disable a limit.
func Parse(b []byte) (*Policy, error) {
	var doc document
	if err := strict(b, &doc); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if doc.Version == "" {
		return nil, errors.New("policy: version is required")
	}
	if len(doc.Accounts) == 0 {
		return nil, errors.New("policy: at least one account is required")
	}
	if len(doc.Symbols) == 0 {
		return nil, errors.New("policy: at least one symbol is required")
	}
	sum := sha256.Sum256(b)
	p := &Policy{
		Version:  doc.Version,
		Hash:     hex.EncodeToString(sum[:]),
		Symbols:  doc.Symbols,
		Session:  doc.Session,
		accounts: make(map[string]Limits, len(doc.Accounts)),
	}
	for id, raw := range doc.Accounts {
		l := doc.Defaults // account JSON overlays only the fields it names
		if err := strict(raw, &l); err != nil {
			return nil, fmt.Errorf("policy: account %q: %w", id, err)
		}
		if err := l.validate(); err != nil {
			return nil, fmt.Errorf("policy: account %q: %w", id, err)
		}
		p.accounts[id] = l
	}
	for sym, s := range doc.Symbols {
		if s.LotSize <= 0 || s.TickSize <= 0 || s.PriceCollarBps < 0 || s.PriceCollarBps > 10000 {
			return nil, fmt.Errorf("policy: symbol %q: lot_size and tick_size must be positive, price_collar_bps in [0,10000]", sym)
		}
	}
	if s := doc.Session; s != nil {
		var err1, err2 error
		s.open, err1 = parseHHMM(s.OpenUTC)
		s.close, err2 = parseHHMM(s.CloseUTC)
		if err1 != nil || err2 != nil || s.open >= s.close {
			return nil, errors.New("policy: session needs open_utc < close_utc in HH:MM")
		}
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				return nil, errors.New("policy: session weekdays must be 0-6")
			}
		}
	}
	return p, nil
}

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (l Limits) validate() error {
	pos := func(name string, v int64) error {
		if v <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
		return nil
	}
	money := func(name string, v fixed.Decimal) error {
		if v <= 0 || v > MaxThreshold {
			return fmt.Errorf("%s must be in (0, %s]", name, MaxThreshold)
		}
		return nil
	}
	if l.ReviewOrderNotional < 0 || l.ReviewOrderNotional > MaxThreshold {
		return errors.New("review_order_notional out of range")
	}
	if l.PriceCollarBps > 10000 {
		return errors.New("price_collar_bps must be at most 10000")
	}
	return errors.Join(
		pos("max_order_qty", l.MaxOrderQty),
		money("max_order_notional", l.MaxOrderNotional),
		money("max_symbol_exposure", l.MaxSymbolExposure),
		money("max_gross_exposure", l.MaxGrossExposure),
		money("max_daily_loss", l.MaxDailyLoss),
		pos("rate_limit.max_orders", int64(l.RateLimit.MaxOrders)),
		pos("rate_limit.window_ms", l.RateLimit.WindowMs),
		pos("quote_max_age_ms", l.QuoteMaxAgeMs),
		pos("max_clock_skew_ms", l.MaxClockSkewMs),
		pos("price_collar_bps", l.PriceCollarBps),
		pos("reservation_ttl_ms", l.ReservationTTLMs),
		pos("review_ttl_ms", l.ReviewTTLMs),
	)
}
