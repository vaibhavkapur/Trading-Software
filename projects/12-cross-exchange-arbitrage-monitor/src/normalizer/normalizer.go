// Package normalizer maps exchange-specific symbols, assets, prices, and quantities
// into canonical top-N order books using conservative fixed-point rounding.
package normalizer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"arbmonitor/src/domain"
	"arbmonitor/src/fixed"
)

var (
	ErrUnknownInstrument = errors.New("unknown venue instrument")
	ErrInvalidLevel      = errors.New("invalid price level")
	ErrCrossedVenueBook  = errors.New("crossed single-venue order book")
	ErrMissingTimestamp  = errors.New("missing book timestamp")
)

// RawLevel represents an unparsed price/quantity tuple straight from an exchange payload.
type RawLevel struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
}

// RawBookUpdate represents an exchange-specific order book snapshot or update prior to normalization.
type RawBookUpdate struct {
	Venue       string     `json:"venue"`
	VenueSymbol string     `json:"venue_symbol"`
	Bids        []RawLevel `json:"bids"`
	Asks        []RawLevel `json:"asks"`
	Asof        time.Time  `json:"asof"`
	ReceivedAt  time.Time  `json:"received_at"`
	Seq         uint64     `json:"seq"`
	IsSnapshot  bool       `json:"is_snapshot"`
}

// Registry maps canonical instruments and venue-specific symbols to InstrumentSpec.
type Registry struct {
	byCanonical   map[string]domain.InstrumentSpec
	byVenueSymbol map[string]domain.InstrumentSpec // key: lower(venue) + "|" + upper(symbol)
	assetAliases  map[string]string
}

// NewRegistry creates an InstrumentRegistry from a slice of specs.
func NewRegistry(specs []domain.InstrumentSpec) (*Registry, error) {
	r := &Registry{
		byCanonical:   make(map[string]domain.InstrumentSpec, len(specs)),
		byVenueSymbol: make(map[string]domain.InstrumentSpec, len(specs)*3),
		assetAliases: map[string]string{
			"XBT":  "BTC",
			"XXBT": "BTC",
			"XETH": "ETH",
			"ZUSD": "USD",
		},
	}
	for _, spec := range specs {
		if err := validateSpec(spec); err != nil {
			return nil, err
		}
		canon := strings.ToUpper(strings.TrimSpace(spec.Canonical))
		spec.Canonical = canon
		spec.Base = r.CanonicalAsset(spec.Base)
		spec.Quote = r.CanonicalAsset(spec.Quote)
		if spec.MaxDepth <= 0 {
			spec.MaxDepth = 10
		}
		r.byCanonical[canon] = spec
		for venue, sym := range spec.VenueSymbols {
			key := venueSymbolKey(venue, sym)
			r.byVenueSymbol[key] = spec
		}
	}
	return r, nil
}

// LoadRegistryFile reads instrument specifications from a JSON file.
func LoadRegistryFile(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read instruments file: %w", err)
	}
	var specs []domain.InstrumentSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("parse instruments json: %w", err)
	}
	return NewRegistry(specs)
}

func validateSpec(spec domain.InstrumentSpec) error {
	if strings.TrimSpace(spec.Canonical) == "" {
		return errors.New("instrument canonical name is required")
	}
	if strings.TrimSpace(spec.Base) == "" || strings.TrimSpace(spec.Quote) == "" {
		return fmt.Errorf("instrument %s requires base and quote", spec.Canonical)
	}
	if !spec.TickSize.IsPositive() {
		return fmt.Errorf("instrument %s tick_size must be positive", spec.Canonical)
	}
	if !spec.LotSize.IsPositive() {
		return fmt.Errorf("instrument %s lot_size must be positive", spec.Canonical)
	}
	return nil
}

func venueSymbolKey(venue, symbol string) string {
	return strings.ToLower(strings.TrimSpace(venue)) + "|" + strings.ToUpper(strings.TrimSpace(symbol))
}

// CanonicalAsset resolves exchange-specific asset tickers (e.g. XBT -> BTC).
func (r *Registry) CanonicalAsset(asset string) string {
	upper := strings.ToUpper(strings.TrimSpace(asset))
	if mapped, ok := r.assetAliases[upper]; ok {
		return mapped
	}
	return upper
}

// LookupByVenueSymbol resolves a venue's native symbol to its canonical InstrumentSpec.
func (r *Registry) LookupByVenueSymbol(venue, venueSymbol string) (domain.InstrumentSpec, bool) {
	if spec, ok := r.byVenueSymbol[venueSymbolKey(venue, venueSymbol)]; ok {
		return spec, true
	}
	// Fallback: check if caller already provided canonical symbol.
	if spec, ok := r.byCanonical[strings.ToUpper(strings.TrimSpace(venueSymbol))]; ok {
		return spec, true
	}
	return domain.InstrumentSpec{}, false
}

// LookupCanonical returns the InstrumentSpec for a canonical instrument name (e.g. "BTC/USDT").
func (r *Registry) LookupCanonical(canonical string) (domain.InstrumentSpec, bool) {
	spec, ok := r.byCanonical[strings.ToUpper(strings.TrimSpace(canonical))]
	return spec, ok
}

// All returns all registered InstrumentSpecs sorted by canonical name.
func (r *Registry) All() []domain.InstrumentSpec {
	out := make([]domain.InstrumentSpec, 0, len(r.byCanonical))
	for _, spec := range r.byCanonical {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Canonical < out[j].Canonical })
	return out
}

// Normalizer converts RawBookUpdate records into validated, tick/lot-aligned VenueBooks.
type Normalizer struct {
	registry *Registry
	topN     int
}

// New creates a Normalizer backed by the given Registry. If topN > 0, it caps book depth globally.
func New(registry *Registry, topN int) *Normalizer {
	return &Normalizer{registry: registry, topN: topN}
}

// Registry returns the underlying instrument registry.
func (n *Normalizer) Registry() *Registry {
	return n.registry
}

// Normalize parses, validates, tick/lot rounds, aggregates, sorts, and truncates a raw order book.
func (n *Normalizer) Normalize(raw RawBookUpdate) (domain.VenueBook, error) {
	venue := strings.ToLower(strings.TrimSpace(raw.Venue))
	if venue == "" {
		return domain.VenueBook{}, errors.New("venue is required")
	}
	if raw.Asof.IsZero() || raw.ReceivedAt.IsZero() {
		return domain.VenueBook{}, ErrMissingTimestamp
	}

	spec, ok := n.registry.LookupByVenueSymbol(venue, raw.VenueSymbol)
	if !ok {
		return domain.VenueBook{}, fmt.Errorf("%w: venue=%s symbol=%s", ErrUnknownInstrument, venue, raw.VenueSymbol)
	}

	depthLimit := spec.MaxDepth
	if n.topN > 0 && (depthLimit <= 0 || n.topN < depthLimit) {
		depthLimit = n.topN
	}

	bids, err := normalizeSide(raw.Bids, spec.TickSize, spec.LotSize, true, depthLimit)
	if err != nil {
		return domain.VenueBook{}, fmt.Errorf("normalize bids for %s %s: %w", venue, spec.Canonical, err)
	}
	asks, err := normalizeSide(raw.Asks, spec.TickSize, spec.LotSize, false, depthLimit)
	if err != nil {
		return domain.VenueBook{}, fmt.Errorf("normalize asks for %s %s: %w", venue, spec.Canonical, err)
	}

	// Reject a crossed single-venue book (best bid >= best ask) as corrupt market data.
	if len(bids) > 0 && len(asks) > 0 && bids[0].Price.GreaterThanOrEqual(asks[0].Price) {
		return domain.VenueBook{}, fmt.Errorf("%w: venue=%s instrument=%s best_bid=%s best_ask=%s",
			ErrCrossedVenueBook, venue, spec.Canonical, bids[0].Price, asks[0].Price)
	}

	return domain.VenueBook{
		Venue:      venue,
		Instrument: spec.Canonical,
		QuoteAsset: spec.Quote,
		Bids:       bids,
		Asks:       asks,
		Asof:       raw.Asof.UTC(),
		ReceivedAt: raw.ReceivedAt.UTC(),
		Seq:        raw.Seq,
		Status:     domain.VenueStatusHealthy,
	}, nil
}

func normalizeSide(rawLevels []RawLevel, tickSize, lotSize fixed.Decimal, isBid bool, depthLimit int) ([]domain.PriceLevel, error) {
	if len(rawLevels) == 0 {
		return nil, nil
	}

	agg := make(map[int64]fixed.Decimal, len(rawLevels))
	for _, rl := range rawLevels {
		price, err := fixed.Parse(rl.Price)
		if err != nil {
			return nil, fmt.Errorf("%w: price %q: %v", ErrInvalidLevel, rl.Price, err)
		}
		qty, err := fixed.Parse(rl.Qty)
		if err != nil {
			return nil, fmt.Errorf("%w: qty %q: %v", ErrInvalidLevel, rl.Qty, err)
		}
		if !price.IsPositive() {
			return nil, fmt.Errorf("%w: non-positive price %s", ErrInvalidLevel, price)
		}
		if qty.IsNegative() {
			return nil, fmt.Errorf("%w: negative qty %s", ErrInvalidLevel, qty)
		}
		if qty.IsZero() {
			continue
		}

		// Conservative tick rounding: bids floor down, asks ceil up.
		var normPrice fixed.Decimal
		if isBid {
			normPrice = price.FloorToStep(tickSize)
		} else {
			normPrice = price.CeilToStep(tickSize)
		}
		if !normPrice.IsPositive() {
			continue
		}

		// Floor quantity down to valid lot size so we never overstate executable depth.
		normQty := qty.FloorToStep(lotSize)
		if !normQty.IsPositive() {
			continue
		}

		prev := agg[normPrice.Raw()]
		agg[normPrice.Raw()] = prev.Add(normQty)
	}

	levels := make([]domain.PriceLevel, 0, len(agg))
	for rawPrice, qty := range agg {
		levels = append(levels, domain.PriceLevel{
			Price: fixed.FromRaw(rawPrice),
			Qty:   qty,
		})
	}

	sort.Slice(levels, func(i, j int) bool {
		if isBid {
			return levels[i].Price.GreaterThan(levels[j].Price)
		}
		return levels[i].Price.LessThan(levels[j].Price)
	})

	if depthLimit > 0 && len(levels) > depthLimit {
		levels = levels[:depthLimit]
	}
	return levels, nil
}
