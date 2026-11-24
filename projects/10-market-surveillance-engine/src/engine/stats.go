package engine

import (
	"fmt"
	"io"
	"time"

	"surveillance/src/cases"
	"surveillance/src/metrics"
)

// Stats is a point-in-time summary. Event and alert totals are rebuilt
// from the journal and so cover its whole history; duplicates, rejects and
// journal errors are counted since this process started.
type Stats struct {
	Events struct {
		Total                uint64            `json:"total"`
		Orders               uint64            `json:"orders"`
		Trades               uint64            `json:"trades"`
		Late                 uint64            `json:"late"`
		DuplicatesSinceStart uint64            `json:"duplicates_since_start"`
		RejectedSinceStart   map[string]uint64 `json:"rejected_since_start"`
	} `json:"events"`
	Alerts struct {
		Total      int               `json:"total"`
		ByRule     map[string]uint64 `json:"by_rule"`
		BySeverity map[string]uint64 `json:"by_severity"`
		ByStatus   map[string]uint64 `json:"by_status"`
		// Detections is every rule firing; Suppressed is how many of
		// those were folded into an existing alert.
		Detections       uint64            `json:"detections"`
		Suppressed       uint64            `json:"suppressed"`
		SuppressedByRule map[string]uint64 `json:"suppressed_by_rule"`
		DuplicateRate    float64           `json:"duplicate_rate"`
		PerMillionEvents float64           `json:"per_million_events"`
	} `json:"alerts"`
	Stream struct {
		Watermark           *time.Time `json:"watermark"`
		MaxEventTime        *time.Time `json:"max_event_time"`
		Buffered            int        `json:"buffered_events"`
		WatermarkLagSeconds float64    `json:"watermark_lag_seconds"`
		IdleAdvances        uint64     `json:"idle_advances_since_start"`
	} `json:"stream"`
	Ruleset struct {
		Version string `json:"version"`
		Hash    string `json:"hash"`
	} `json:"ruleset"`
	Journal struct {
		Bytes            int64  `json:"bytes"`
		ErrorsSinceStart uint64 `json:"errors_since_start"`
		PendingOutput    bool   `json:"pending_output"`
	} `json:"journal"`
	Cases cases.Summary `json:"cases"`
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	var s Stats
	s.Events.Total, s.Events.Orders, s.Events.Trades, s.Events.Late = e.st.events, e.st.orders, e.st.trades, e.st.late
	s.Events.DuplicatesSinceStart = e.st.duplicates
	s.Events.RejectedSinceStart = map[string]uint64{}
	for k, v := range e.st.rejected {
		s.Events.RejectedSinceStart[k] = v
	}

	s.Alerts.ByRule, s.Alerts.BySeverity = map[string]uint64{}, map[string]uint64{}
	s.Alerts.ByStatus, s.Alerts.SuppressedByRule = map[string]uint64{}, map[string]uint64{}
	for _, a := range e.book.All() {
		s.Alerts.Total++
		s.Alerts.ByRule[a.RuleID]++
		s.Alerts.BySeverity[a.Severity]++
		s.Alerts.ByStatus[a.Status]++
		s.Alerts.Detections += uint64(a.Occurrences)
		s.Alerts.Suppressed += uint64(a.Occurrences - 1)
		s.Alerts.SuppressedByRule[a.RuleID] += uint64(a.Occurrences - 1)
	}
	if s.Alerts.Detections > 0 {
		s.Alerts.DuplicateRate = float64(s.Alerts.Suppressed) / float64(s.Alerts.Detections)
	}
	if s.Events.Total > 0 {
		s.Alerts.PerMillionEvents = float64(s.Alerts.Total) * 1e6 / float64(s.Events.Total)
	}

	s.Stream.Buffered = e.reorder.Buffered()
	s.Stream.IdleAdvances = e.st.idleAdvances
	wm, haveWM := e.reorder.Watermark()
	maxSeen, haveMax := e.reorder.MaxSeen()
	if haveWM {
		t := time.Unix(0, wm).UTC()
		s.Stream.Watermark = &t
	}
	if haveMax {
		t := time.Unix(0, maxSeen).UTC()
		s.Stream.MaxEventTime = &t
		if lag := maxSeen - wm; haveWM && lag > 0 {
			s.Stream.WatermarkLagSeconds = time.Duration(lag).Seconds()
		}
	}

	s.Ruleset.Version, s.Ruleset.Hash = e.rs.Version, e.rsHash
	s.Journal.Bytes = e.jr.Size()
	s.Journal.ErrorsSinceStart = e.st.journalErrors
	s.Journal.PendingOutput = e.pendingOut != nil
	s.Cases = e.cases.Summary()
	return s
}

// WriteMetrics renders the Prometheus exposition.
func (e *Engine) WriteMetrics(w io.Writer) {
	s := e.Stats()
	metrics.Counter(w, "surv_events_total", "Events accepted into the journal, including late ones.", s.Events.Total)
	metrics.Counter(w, "surv_events_late_total", "Events that arrived behind the watermark and were kept out of detection.", s.Events.Late)
	metrics.Counter(w, "surv_events_duplicate_total", "Redelivered events recognised and dropped since start.", s.Events.DuplicatesSinceStart)
	metrics.Labelled(w, "surv_events_rejected_total", "counter", "Events refused since start, by reason code.", "code", s.Events.RejectedSinceStart)
	metrics.Labelled(w, "surv_alerts_total", "counter", "Alerts raised, by rule.", "rule", s.Alerts.ByRule)
	metrics.Labelled(w, "surv_alerts_by_severity", "gauge", "Alerts by severity.", "severity", s.Alerts.BySeverity)
	metrics.Labelled(w, "surv_alerts_by_status", "gauge", "Alerts by workflow status.", "status", s.Alerts.ByStatus)
	metrics.Labelled(w, "surv_detections_suppressed_total", "counter", "Detections folded into an existing alert, by rule.", "rule", s.Alerts.SuppressedByRule)
	metrics.Gauge(w, "surv_alerts_per_million_events", "Alerts raised per million events accepted.", s.Alerts.PerMillionEvents)
	metrics.Gauge(w, "surv_duplicate_alert_ratio", "Share of detections that suppression folded into an existing alert.", s.Alerts.DuplicateRate)
	e.AlertLatency.Write(w, "surv_event_to_alert_seconds", "Local receive time of the triggering event to the alert being raised.")
	metrics.Gauge(w, "surv_watermark_lag_seconds", "Newest event time minus the watermark.", s.Stream.WatermarkLagSeconds)
	metrics.Gauge(w, "surv_buffered_events", "Events waiting for the watermark.", float64(s.Stream.Buffered))
	metrics.Counter(w, "surv_idle_watermark_advances_total", "Watermark advances driven by the idle timer since start.", s.Stream.IdleAdvances)
	metrics.Counter(w, "surv_journal_errors_total", "Failed journal writes since start.", s.Journal.ErrorsSinceStart)
	metrics.Gauge(w, "surv_journal_bytes", "Size of the journal.", float64(s.Journal.Bytes))
	byStatus := map[string]uint64{}
	for k, v := range s.Cases.ByStatus {
		byStatus[k] = uint64(v)
	}
	byDisp := map[string]uint64{}
	for k, v := range s.Cases.ByDisposition {
		byDisp[k] = uint64(v)
	}
	metrics.Labelled(w, "surv_cases", "gauge", "Cases by status.", "status", byStatus)
	metrics.Labelled(w, "surv_case_resolutions", "gauge", "Resolved cases by disposition.", "disposition", byDisp)
	metrics.Gauge(w, "surv_case_resolution_ratio", "Resolved cases over all cases.", s.Cases.ResolutionRate)
	e.ResolveTime.Write(w, "surv_case_resolution_seconds", "Case opened to case resolved, for cases resolved since start.")
	fmt.Fprintf(w, "# HELP surv_ruleset_info Active ruleset.\n# TYPE surv_ruleset_info gauge\nsurv_ruleset_info{version=%q,hash=%q} 1\n", s.Ruleset.Version, s.Ruleset.Hash)
}
