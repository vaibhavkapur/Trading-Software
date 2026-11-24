// Package replay deterministically reproduces arbitrage opportunities, alerts,
// metrics, and paper adverse-selection diagnostics from recorded feed fixtures.
package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/fixed"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
)

// Options configures a deterministic replay run.
type Options struct {
	Events             []connectors.FeedEvent
	Registry           *normalizer.Registry
	Fees               *edgecalculator.FeeSchedule
	Balances           *edgecalculator.BalanceBook
	Config             domain.AlertConfig
	ArchivePath        string // optional JSONL file path to persist alerts
	AdverseLookaheadMs int64  // optional paper execution adverse-selection check window
}

// PaperExecutionCheck evaluates whether an emitted alert would still have positive edge
// after a simulated execution latency window (adverse-selection analysis).
type PaperExecutionCheck struct {
	AlertID          string        `json:"alert_id"`
	TriggerNetBps    fixed.Decimal `json:"trigger_net_bps"`
	LookaheadNetBps  fixed.Decimal `json:"lookahead_net_bps"`
	LookaheadMs      int64         `json:"lookahead_ms"`
	StillProfitable  bool          `json:"still_profitable"`
	AdverseSelection bool          `json:"adverse_selection"`
}

// Report contains the complete deterministic output of a replay run.
type Report struct {
	EventsProcessed   int                   `json:"events_processed"`
	BooksAccepted     int                   `json:"books_accepted"`
	DuplicatesIgnored int                   `json:"duplicates_ignored"`
	OutOfOrderIgnored int                   `json:"out_of_order_ignored"`
	SequenceGaps      int                   `json:"sequence_gaps"`
	Disconnects       int                   `json:"disconnects"`
	Reconnects        int                   `json:"reconnects"`
	Alerts            []domain.Alert        `json:"alerts"`
	VenueHealth       []domain.VenueHealth  `json:"venue_health"`
	Metrics           metrics.Snapshot      `json:"metrics"`
	PaperChecks       []PaperExecutionCheck `json:"paper_checks,omitempty"`
	DigestSHA256      string                `json:"digest_sha256"`
	Disclaimer        string                `json:"disclaimer"`
}

// Run executes a deterministic replay using event timestamps as the sole clock source.
func Run(opts Options) (*Report, error) {
	norm := normalizer.New(opts.Registry, opts.Config.MaxDepthLevels)
	feeds := connectors.NewFeedManager(connectors.NewDefaultAdapters(), norm)
	calc := edgecalculator.New(opts.Fees, opts.Balances, feeds, edgecalculator.DefaultTransferSchedule())
	collector := metrics.New()

	var store alertengine.AlertStore
	if opts.ArchivePath != "" {
		var err error
		store, err = alertengine.NewJSONLAlertStore(opts.ArchivePath)
		if err != nil {
			return nil, err
		}
	} else {
		store = alertengine.NewMemoryAlertStore()
	}

	engine := alertengine.New(feeds, opts.Registry, calc, store, collector, opts.Config)

	rep := &Report{
		Disclaimer: domain.PaperDisclaimer,
	}

	type pendingCheck struct {
		alert     domain.Alert
		checkAt   time.Time
		completed bool
	}
	var pending []pendingCheck
	var lastTime time.Time

	for _, ev := range opts.Events {
		rep.EventsProcessed++
		evTime := ev.ReceivedAt.UTC()
		if evTime.After(lastTime) {
			lastTime = evTime
		}

		outcome := feeds.Ingest(ev)
		if outcome.Duplicate {
			rep.DuplicatesIgnored++
		}
		if outcome.OutOfOrder {
			rep.OutOfOrderIgnored++
		}
		if outcome.SequenceGap {
			rep.SequenceGaps++
		}
		if outcome.Disconnected {
			rep.Disconnects++
		}
		if outcome.Reconnected {
			rep.Reconnects++
		}
		if outcome.Accepted && outcome.Book != nil {
			rep.BooksAccepted++
		}

		if outcome.Accepted || outcome.SequenceGap {
			emitted, err := engine.EvaluateAll(evTime)
			if err != nil {
				return nil, fmt.Errorf("replay evaluate at %s: %w", evTime.Format(time.RFC3339Nano), err)
			}
			for _, a := range emitted {
				if opts.AdverseLookaheadMs > 0 {
					pending = append(pending, pendingCheck{
						alert:   a,
						checkAt: a.TriggeredAt.Add(time.Duration(opts.AdverseLookaheadMs) * time.Millisecond),
					})
				}
			}
		}

		// Evaluate any paper adverse-selection checks whose lookahead window has elapsed.
		if opts.AdverseLookaheadMs > 0 {
			for i := range pending {
				if pending[i].completed || evTime.Before(pending[i].checkAt) {
					continue
				}
				pending[i].completed = true
				chk := evaluatePaperLookahead(pending[i].alert, feeds, opts.Registry, calc, opts.Config, evTime, opts.AdverseLookaheadMs)
				rep.PaperChecks = append(rep.PaperChecks, chk)
			}
		}
	}

	// Complete any remaining paper checks at end of stream.
	if opts.AdverseLookaheadMs > 0 && !lastTime.IsZero() {
		for i := range pending {
			if pending[i].completed {
				continue
			}
			pending[i].completed = true
			chk := evaluatePaperLookahead(pending[i].alert, feeds, opts.Registry, calc, opts.Config, lastTime, opts.AdverseLookaheadMs)
			rep.PaperChecks = append(rep.PaperChecks, chk)
		}
	}

	// Expire alerts cleanly at end-of-stream + TTL if needed only relative to lastTime.
	if !lastTime.IsZero() {
		engine.ExpireAlerts(lastTime)
	}

	rep.Alerts = engine.Alerts(false, 0)
	rep.VenueHealth = feeds.VenueHealth(lastTime, opts.Config.MaxQuoteAgeMs)
	rep.Metrics = collector.Snapshot()
	rep.DigestSHA256 = computeAlertsDigest(rep.Alerts)
	return rep, nil
}

func evaluatePaperLookahead(
	a domain.Alert,
	feeds *connectors.FeedManager,
	reg *normalizer.Registry,
	calc *edgecalculator.Calculator,
	cfg domain.AlertConfig,
	now time.Time,
	lookaheadMs int64,
) PaperExecutionCheck {
	buyBook, ok1 := feeds.GetBook(a.Opportunity.BuyVenue, a.Opportunity.BuyInstrument)
	sellBook, ok2 := feeds.GetBook(a.Opportunity.SellVenue, a.Opportunity.SellInstrument)
	buySpec, ok3 := reg.LookupCanonical(a.Opportunity.BuyInstrument)
	sellSpec, ok4 := reg.LookupCanonical(a.Opportunity.SellInstrument)

	chk := PaperExecutionCheck{
		AlertID:       a.ID,
		TriggerNetBps: a.Opportunity.NetEdgeBps,
		LookaheadMs:   lookaheadMs,
	}
	if !(ok1 && ok2 && ok3 && ok4) {
		chk.AdverseSelection = true
		return chk
	}
	res := calc.Evaluate(edgecalculator.EvalInput{
		BuyBook:  buyBook,
		SellBook: sellBook,
		BuySpec:  buySpec,
		SellSpec: sellSpec,
		Config:   cfg,
		Now:      now,
	})
	if res.Opportunity != nil {
		chk.LookaheadNetBps = res.Opportunity.NetEdgeBps
		chk.StillProfitable = res.Suppressed == domain.SuppressNone && res.Opportunity.NetProfit.IsPositive()
		chk.AdverseSelection = res.Opportunity.NetEdgeBps.LessThan(a.Opportunity.NetEdgeBps)
	} else {
		chk.AdverseSelection = true
	}
	return chk
}

// computeAlertsDigest hashes the canonical JSON serialization of all emitted alerts
// to prove deterministic replay reproducibility.
func computeAlertsDigest(alerts []domain.Alert) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, a := range alerts {
		_ = enc.Encode(a)
	}
	return hex.EncodeToString(h.Sum(nil))
}
