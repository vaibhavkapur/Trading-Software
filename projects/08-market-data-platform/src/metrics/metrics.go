// Package metrics defines every Prometheus series the platform exposes.
//
// Latency series name the clocks they span, because event_time (venue clock)
// and receive_time (ingestor clock) are different clocks:
//
//	mdp_feed_lag_seconds      receive_time - event_time. Crosses venue and
//	                          ingestor clocks, so it includes any clock skew
//	                          and can be negative.
//	mdp_pipeline_latency_seconds{stage}
//	                          stage completion time - receive_time. Both ends
//	                          are platform clocks; this is the end-to-end
//	                          ingestion latency.
package metrics

import (
	"math"
	"net/http"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Set is one process's metrics. Each Set owns its registry so tests can run
// several pipelines side by side.
type Set struct {
	Registry *prometheus.Registry

	// Ingest
	IngestEvents      *prometheus.CounterVec // provider, kind
	IngestBytes       *prometheus.CounterVec // provider
	IngestReconnects  *prometheus.CounterVec // provider
	IngestResyncs     *prometheus.CounterVec // provider
	IngestParseErrors *prometheus.CounterVec // provider
	IngestDuplicates  *prometheus.CounterVec // provider
	IngestConnected   *prometheus.GaugeVec   // provider
	FeedLag           *prometheus.HistogramVec

	// Normalize
	NormalizedEvents  *prometheus.CounterVec // kind
	NormalizerRejects *prometheus.CounterVec // reason
	Gaps              *prometheus.CounterVec // provider
	GapMissing        *prometheus.CounterVec // provider
	Duplicates        *prometheus.CounterVec // provider
	SessionResets     *prometheus.CounterVec // provider
	Flagged           *prometheus.CounterVec // flag

	// Stream consumers
	ConsumerLag     *prometheus.GaugeVec // consumer, topic, partition
	ConsumerCatchup *prometheus.GaugeVec // consumer

	// Sink
	SinkRows           *prometheus.CounterVec // table
	SinkBatches        *prometheus.CounterVec // table
	SinkInsertFailures *prometheus.CounterVec // kind (transient|permanent)
	SinkRejectedRows   prometheus.Counter
	SinkRetrying       prometheus.Gauge

	// Gateway
	WSClients     prometheus.Gauge
	WSSent        *prometheus.CounterVec // type
	WSDropped     prometheus.Counter
	WSDuplicates  prometheus.Counter
	HTTPRequests  *prometheus.CounterVec // route, code
	QueryDuration *prometheus.HistogramVec

	PipelineLatency *prometheus.HistogramVec // stage
}

// latencyBuckets span 50µs to ~13s.
var latencyBuckets = prometheus.ExponentialBuckets(0.00005, 2, 19)

func New() *Set {
	r := prometheus.NewRegistry()
	cv := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		r.MustRegister(c)
		return c
	}
	gv := func(name, help string, labels ...string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
		r.MustRegister(g)
		return g
	}
	hv := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
		r.MustRegister(h)
		return h
	}
	s := &Set{Registry: r}

	s.IngestEvents = cv("mdp_ingest_events_total", "Provider events published to the raw topic.", "provider", "kind")
	s.IngestBytes = cv("mdp_ingest_bytes_total", "Bytes of provider frames read.", "provider")
	s.IngestReconnects = cv("mdp_ingest_reconnects_total", "Provider connections re-established after a loss.", "provider")
	s.IngestResyncs = cv("mdp_ingest_resyncs_total", "Reconnects forced to recover a sequence gap from the provider.", "provider")
	s.IngestParseErrors = cv("mdp_ingest_parse_errors_total", "Frames that could not be parsed and were dead-lettered.", "provider")
	s.IngestDuplicates = cv("mdp_ingest_duplicates_total", "Provider events dropped at the ingestor as already published.", "provider")
	s.IngestConnected = gv("mdp_ingest_connected", "1 while the provider connection is up.", "provider")
	s.FeedLag = hv("mdp_feed_lag_seconds", "receive_time minus event_time; spans venue and ingestor clocks.",
		append([]float64{-1, -0.1, -0.01, 0}, latencyBuckets...), "provider")

	s.NormalizedEvents = cv("mdp_normalized_events_total", "Canonical events published.", "kind")
	s.NormalizerRejects = cv("mdp_normalizer_rejects_total", "Provider events rejected by normalization.", "reason")
	s.Gaps = cv("mdp_gaps_total", "Sequence gaps declared.", "provider")
	s.GapMissing = cv("mdp_gap_missing_events_total", "Provider sequence numbers covered by declared gaps.", "provider")
	s.Duplicates = cv("mdp_duplicates_total", "Redelivered provider events dropped by the normalizer.", "provider")
	s.SessionResets = cv("mdp_session_resets_total", "Provider session changes that reset sequence tracking.", "provider")
	s.Flagged = cv("mdp_flagged_events_total", "Canonical events carrying a data-quality flag.", "flag")

	s.ConsumerLag = gv("mdp_consumer_lag_records", "Records between a consumer's processed position and the log end.", "consumer", "topic", "partition")
	s.ConsumerCatchup = gv("mdp_consumer_catchup_seconds", "Time a consumer took after start to first reach the log end.", "consumer")

	s.SinkRows = cv("mdp_sink_rows_total", "Rows written to the store.", "table")
	s.SinkBatches = cv("mdp_sink_batches_total", "Insert batches written to the store.", "table")
	s.SinkInsertFailures = cv("mdp_sink_insert_failures_total", "Failed store inserts.", "kind")
	s.SinkRejectedRows = prometheus.NewCounter(prometheus.CounterOpts{Name: "mdp_sink_rejected_rows_total", Help: "Rows the store refused permanently; dead-lettered."})
	s.SinkRetrying = prometheus.NewGauge(prometheus.GaugeOpts{Name: "mdp_sink_retrying", Help: "1 while the sink is retrying a failed insert."})

	s.WSClients = prometheus.NewGauge(prometheus.GaugeOpts{Name: "mdp_ws_clients", Help: "Connected WebSocket subscribers."})
	s.WSSent = cv("mdp_ws_messages_total", "Messages queued to WebSocket subscribers.", "type")
	s.WSDropped = prometheus.NewCounter(prometheus.CounterOpts{Name: "mdp_ws_dropped_total", Help: "Events not delivered to a slow subscriber; each is reported to that subscriber as a gap."})
	s.WSDuplicates = prometheus.NewCounter(prometheus.CounterOpts{Name: "mdp_ws_duplicates_suppressed_total", Help: "Redelivered canonical events not forwarded to subscribers."})
	s.HTTPRequests = cv("mdp_http_requests_total", "HTTP requests served.", "route", "code")
	s.QueryDuration = hv("mdp_query_duration_seconds", "Historical query duration.", latencyBuckets, "route")
	r.MustRegister(s.SinkRejectedRows, s.SinkRetrying, s.WSClients, s.WSDropped, s.WSDuplicates)

	s.PipelineLatency = hv("mdp_pipeline_latency_seconds", "Stage completion minus receive_time; platform clocks only.", latencyBuckets, "stage")
	return s
}

// Handler serves the Prometheus exposition format.
func (s *Set) Handler() http.Handler {
	return promhttp.HandlerFor(s.Registry, promhttp.HandlerOpts{})
}

// matches reports whether a metric carries every label=value pair in want.
func matches(labels []*dto.LabelPair, want []string) bool {
	for i := 0; i+1 < len(want); i += 2 {
		found := false
		for _, l := range labels {
			if l.GetName() == want[i] && l.GetValue() == want[i+1] {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Sum adds up the current values of a counter or gauge family, restricted
// to series carrying the given label pairs ("provider", "alpha", ...). The
// demo and bench commands read their figures through it, so what they print
// is what /metrics exposes.
func (s *Set) Sum(name string, labelPairs ...string) float64 {
	families, _ := s.Registry.Gather()
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			if !matches(m.Label, labelPairs) {
				continue
			}
			switch {
			case m.Counter != nil:
				total += m.Counter.GetValue()
			case m.Gauge != nil:
				total += m.Gauge.GetValue()
			}
		}
	}
	return total
}

// Quantile reports an upper bound for the q-quantile of a histogram family:
// the upper edge of the bucket the quantile falls in. A histogram cannot say
// more than that, and callers should present the figure as "at most". It
// returns the number of observations too; with none, bound is NaN. A
// quantile beyond the last finite bucket is +Inf.
func (s *Set) Quantile(name string, q float64, labelPairs ...string) (bound float64, observations uint64) {
	families, _ := s.Registry.Gather()
	buckets := map[float64]uint64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			if m.Histogram == nil || !matches(m.Label, labelPairs) {
				continue
			}
			observations += m.Histogram.GetSampleCount()
			for _, b := range m.Histogram.Bucket {
				buckets[b.GetUpperBound()] += b.GetCumulativeCount()
			}
		}
	}
	if observations == 0 {
		return math.NaN(), 0
	}
	bounds := make([]float64, 0, len(buckets))
	for b := range buckets {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)
	rank := uint64(math.Ceil(q * float64(observations)))
	if rank < 1 {
		rank = 1
	}
	for _, b := range bounds {
		if buckets[b] >= rank {
			return b, observations
		}
	}
	return math.Inf(1), observations
}
