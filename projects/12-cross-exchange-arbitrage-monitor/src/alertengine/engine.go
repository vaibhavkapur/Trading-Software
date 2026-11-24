package alertengine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/fixed"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
)

// Engine scans cross-venue books, enforces alert policy & cooldowns, persists evidence,
// and serves live active opportunities and alert history.
type Engine struct {
	mu          sync.RWMutex
	feeds       *connectors.FeedManager
	registry    *normalizer.Registry
	calc        *edgecalculator.Calculator
	store       AlertStore
	metrics     *metrics.Collector
	cfg         domain.AlertConfig
	lastAlertAt map[string]time.Time          // pairKey -> last trigger time
	liveOpps    map[string]domain.Opportunity // pairKey -> latest valid opportunity
	alerts      []domain.Alert
	alertIndex  map[string]int // alertID -> index in alerts
}

// New creates an AlertEngine wired to feeds, registry, calculator, store, and metrics.
func New(
	feeds *connectors.FeedManager,
	registry *normalizer.Registry,
	calc *edgecalculator.Calculator,
	store AlertStore,
	collector *metrics.Collector,
	cfg domain.AlertConfig,
) *Engine {
	if store == nil {
		store = NewMemoryAlertStore()
	}
	if collector == nil {
		collector = metrics.New()
	}
	if cfg.Version <= 0 {
		cfg.Version = 1
	}
	return &Engine{
		feeds:       feeds,
		registry:    registry,
		calc:        calc,
		store:       store,
		metrics:     collector,
		cfg:         cfg,
		lastAlertAt: make(map[string]time.Time),
		liveOpps:    make(map[string]domain.Opportunity),
		alertIndex:  make(map[string]int),
	}
}

// Config returns a copy of the active AlertConfig.
func (e *Engine) Config() domain.AlertConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cp := e.cfg
	if e.cfg.Instruments != nil {
		cp.Instruments = make([]string, len(e.cfg.Instruments))
		copy(cp.Instruments, e.cfg.Instruments)
	}
	return cp
}

// UpdateConfig validates and applies a new AlertConfig, incrementing its version.
func (e *Engine) UpdateConfig(next domain.AlertConfig) (domain.AlertConfig, error) {
	if next.MinNetBps.IsNegative() {
		return domain.AlertConfig{}, errors.New("min_net_bps must be non-negative")
	}
	if next.MaxQuoteAgeMs <= 0 {
		return domain.AlertConfig{}, errors.New("max_quote_age_ms must be positive")
	}
	if next.MaxLegSkewMs < 0 {
		return domain.AlertConfig{}, errors.New("max_leg_skew_ms must be non-negative")
	}
	if next.CooldownMs < 0 {
		return domain.AlertConfig{}, errors.New("cooldown_ms must be non-negative")
	}
	if next.AlertTTLMs <= 0 {
		next.AlertTTLMs = 5000
	}
	if next.MaxDepthLevels <= 0 {
		next.MaxDepthLevels = 10
	}
	if next.MaxClockSkewMs <= 0 {
		next.MaxClockSkewMs = 1500
	}
	if next.ModeledSlippageBps.IsNegative() {
		return domain.AlertConfig{}, errors.New("modeled_slippage_bps must be non-negative")
	}

	normalizedInsts := make([]string, 0, len(next.Instruments))
	for _, inst := range next.Instruments {
		trimmed := strings.ToUpper(strings.TrimSpace(inst))
		if trimmed != "" {
			normalizedInsts = append(normalizedInsts, trimmed)
		}
	}
	next.Instruments = normalizedInsts

	e.mu.Lock()
	defer e.mu.Unlock()
	next.Version = e.cfg.Version + 1
	e.cfg = next
	return e.cfg, nil
}

// EvaluateAll scans every distinct ordered pair of venue books sharing the same base asset
// at evaluation time `now`, updates live opportunities, emits alerts, and returns any new alerts.
func (e *Engine) EvaluateAll(now time.Time) ([]domain.Alert, error) {
	now = now.UTC()
	e.ExpireAlerts(now)

	cfg := e.Config()
	books := e.feeds.AllBooks()
	if len(books) < 2 {
		return nil, nil
	}

	// Group books by canonical base asset in deterministic order.
	byBase := make(map[string][]domain.VenueBook)
	specByInst := make(map[string]domain.InstrumentSpec)
	for _, b := range books {
		spec, ok := e.registry.LookupCanonical(b.Instrument)
		if !ok {
			continue
		}
		specByInst[b.Instrument] = spec
		byBase[spec.Base] = append(byBase[spec.Base], b)
	}

	bases := make([]string, 0, len(byBase))
	for base := range byBase {
		bases = append(bases, base)
	}
	sort.Strings(bases)

	var emitted []domain.Alert
	var firstErr error

	for _, base := range bases {
		group := byBase[base]
		sort.Slice(group, func(i, j int) bool {
			if group[i].Venue != group[j].Venue {
				return group[i].Venue < group[j].Venue
			}
			return group[i].Instrument < group[j].Instrument
		})

		for i := 0; i < len(group); i++ {
			for j := 0; j < len(group); j++ {
				if i == j {
					continue
				}
				buyBook := group[i]
				sellBook := group[j]
				if buyBook.Venue == sellBook.Venue {
					continue
				}

				buySpec := specByInst[buyBook.Instrument]
				sellSpec := specByInst[sellBook.Instrument]
				pKey := pairKey(buyBook.Instrument, buyBook.Venue, sellBook.Venue, sellBook.Instrument)

				if !instrumentAllowed(cfg.Instruments, buyBook.Instrument, sellBook.Instrument) {
					e.metrics.RecordSuppression(domain.SuppressInstrumentFiltered)
					continue
				}

				e.metrics.RecordEvaluation()
				res := e.calc.Evaluate(edgecalculator.EvalInput{
					BuyBook:  buyBook,
					SellBook: sellBook,
					BuySpec:  buySpec,
					SellSpec: sellSpec,
					Config:   cfg,
					Now:      now,
				})

				if res.Opportunity != nil {
					e.metrics.RecordOpportunity(*res.Opportunity)
				}

				if res.Suppressed != domain.SuppressNone {
					e.metrics.RecordSuppression(res.Suppressed)
					// Remove stale/disqualified pair from live opportunities, or keep sub-threshold positive opp for API query visibility.
					e.mu.Lock()
					if res.Suppressed == domain.SuppressBelowNetThreshold && res.Opportunity != nil && res.Opportunity.NetProfit.IsPositive() {
						e.liveOpps[pKey] = *res.Opportunity
					} else {
						delete(e.liveOpps, pKey)
					}
					e.mu.Unlock()
					continue
				}

				opp := *res.Opportunity
				e.mu.Lock()
				e.liveOpps[pKey] = opp
				lastAt, hadLast := e.lastAlertAt[pKey]
				inCooldown := hadLast && cfg.CooldownMs > 0 && now.Sub(lastAt).Milliseconds() < cfg.CooldownMs
				e.mu.Unlock()

				if inCooldown {
					e.metrics.RecordSuppression(domain.SuppressCooldown)
					continue
				}

				ttl := time.Duration(cfg.AlertTTLMs) * time.Millisecond
				if ttl <= 0 {
					ttl = 5 * time.Second
				}
				alertID := deterministicAlertID(opp.ID, cfg.Version, now)
				alert := domain.Alert{
					ID:            alertID,
					OpportunityID: opp.ID,
					RuleVersion:   opp.RuleVersion,
					ConfigVersion: cfg.Version,
					TriggeredAt:   now,
					ExpiryAt:      now.Add(ttl),
					Status:        domain.AlertStatusActive,
					Opportunity:   opp,
					Evidence: domain.AlertEvidence{
						BuyBookSnapshot:  buyBook.Clone(),
						SellBookSnapshot: sellBook.Clone(),
						BuyFee:           res.BuyFee,
						SellFee:          res.SellFee,
						FXRateUsed:       res.FXRateUsed,
						ConfigVersion:    cfg.Version,
						RuleVersion:      opp.RuleVersion,
					},
					Disclaimer: domain.PaperDisclaimer,
				}

				if err := e.store.Append(alert); err != nil {
					e.metrics.RecordStoreWriteError()
					if firstErr == nil {
						firstErr = fmt.Errorf("persist alert %s: %w", alert.ID, err)
					}
					continue
				}

				e.mu.Lock()
				e.lastAlertAt[pKey] = now
				e.alertIndex[alert.ID] = len(e.alerts)
				e.alerts = append(e.alerts, alert)
				e.mu.Unlock()

				e.metrics.RecordAlertEmitted(alert)
				emitted = append(emitted, alert)
			}
		}
	}

	return emitted, firstErr
}

// ExpireAlerts transitions active alerts past their ExpiryAt into expired status.
func (e *Engine) ExpireAlerts(now time.Time) int {
	now = now.UTC()
	e.mu.Lock()
	defer e.mu.Unlock()

	expired := 0
	for i := range e.alerts {
		if e.alerts[i].Status == domain.AlertStatusActive && !now.Before(e.alerts[i].ExpiryAt) {
			e.alerts[i].Status = domain.AlertStatusExpired
			lifeMs := e.alerts[i].ExpiryAt.Sub(e.alerts[i].TriggeredAt).Milliseconds()
			e.metrics.RecordAlertExpired(lifeMs)
			expired++
		}
	}
	return expired
}

// ActiveOpportunities returns currently valid, non-stale opportunities meeting optional filters.
func (e *Engine) ActiveOpportunities(now time.Time, minNetBps fixed.Decimal, instrument string) []domain.Opportunity {
	now = now.UTC()
	e.mu.RLock()
	cfg := e.cfg
	opps := make([]domain.Opportunity, 0, len(e.liveOpps))
	for _, opp := range e.liveOpps {
		opps = append(opps, opp)
	}
	e.mu.RUnlock()

	instFilter := strings.ToUpper(strings.TrimSpace(instrument))
	var out []domain.Opportunity
	for _, opp := range opps {
		if instFilter != "" && opp.Instrument != instFilter && opp.SellInstrument != instFilter {
			continue
		}
		if opp.NetEdgeBps.LessThan(minNetBps) {
			continue
		}
		// Re-verify freshness at query time so stale opportunities are never returned.
		if cfg.MaxQuoteAgeMs > 0 {
			if now.Sub(opp.BuyBookReceivedAt).Milliseconds() > cfg.MaxQuoteAgeMs ||
				now.Sub(opp.SellBookReceivedAt).Milliseconds() > cfg.MaxQuoteAgeMs {
				continue
			}
		}
		out = append(out, opp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NetEdgeBps.Equal(out[j].NetEdgeBps) {
			return out[i].NetEdgeBps.GreaterThan(out[j].NetEdgeBps)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Alerts returns recorded alerts, optionally filtered to active-only and capped by limit.
func (e *Engine) Alerts(activeOnly bool, limit int) []domain.Alert {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var out []domain.Alert
	for _, a := range e.alerts {
		if activeOnly && a.Status != domain.AlertStatusActive {
			continue
		}
		out = append(out, a)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func instrumentAllowed(allowed []string, buyInst, sellInst string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, inst := range allowed {
		if inst == "*" || inst == buyInst || inst == sellInst {
			return true
		}
	}
	return false
}

func pairKey(buyInst, buyVenue, sellVenue, sellInst string) string {
	return buyInst + "|" + buyVenue + "->" + sellVenue + "|" + sellInst
}

func deterministicAlertID(oppID string, configVersion int, triggeredAt time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d", oppID, configVersion, triggeredAt.UTC().UnixNano())
	return "alt-" + hex.EncodeToString(h.Sum(nil))[:16]
}
