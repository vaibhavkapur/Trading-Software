//! Service-level metrics in Prometheus text exposition format.
//!
//! Every duration here is measured with one monotonic clock inside this
//! process. Nothing claims venue-to-here latency: that would need clocks
//! synchronized with the venue.

use crate::engine::Engine;
use book_core::{Integrity, SyncState, TsMicros};
use std::fmt::Write;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering::Relaxed};

pub struct Histogram {
    bounds: &'static [f64],
    buckets: Vec<AtomicU64>,
    sum_nanos: AtomicU64,
    count: AtomicU64,
}

impl Histogram {
    pub fn new(bounds: &'static [f64]) -> Self {
        Self {
            bounds,
            buckets: bounds.iter().map(|_| AtomicU64::new(0)).collect(),
            sum_nanos: AtomicU64::new(0),
            count: AtomicU64::new(0),
        }
    }

    pub fn observe(&self, seconds: f64) {
        if let Some(i) = self.bounds.iter().position(|b| seconds <= *b) {
            self.buckets[i].fetch_add(1, Relaxed);
        }
        self.sum_nanos.fetch_add((seconds * 1e9) as u64, Relaxed);
        self.count.fetch_add(1, Relaxed);
    }

    pub fn count(&self) -> u64 {
        self.count.load(Relaxed)
    }

    fn render(&self, out: &mut String, name: &str, help: &str) {
        let _ = writeln!(out, "# HELP {name} {help}\n# TYPE {name} histogram");
        let mut cumulative = 0;
        for (bound, bucket) in self.bounds.iter().zip(&self.buckets) {
            cumulative += bucket.load(Relaxed);
            let _ = writeln!(out, "{name}_bucket{{le=\"{bound}\"}} {cumulative}");
        }
        let count = self.count.load(Relaxed);
        let _ = writeln!(out, "{name}_bucket{{le=\"+Inf\"}} {count}");
        let _ = writeln!(
            out,
            "{name}_sum {}\n{name}_count {count}",
            self.sum_nanos.load(Relaxed) as f64 / 1e9
        );
    }
}

const LATENCY_BOUNDS: &[f64] = &[
    0.000_005, 0.000_01, 0.000_025, 0.000_05, 0.000_1, 0.000_25, 0.000_5, 0.001, 0.002_5, 0.005,
    0.01, 0.025, 0.05, 0.1, 0.25, 1.0,
];
const RECOVERY_BOUNDS: &[f64] = &[
    0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0,
];

pub struct Metrics {
    /// Frame handed over by the source → book updated.
    pub feed_to_book: Histogram,
    /// Gap detected → book synced again.
    pub gap_recovery: Histogram,
    pub ws_clients: AtomicI64,
    pub ws_lagged: AtomicU64,
    pub sink_rows_written: AtomicU64,
    pub sink_rows_dropped: AtomicU64,
    pub sink_write_errors: AtomicU64,
}

impl Default for Metrics {
    fn default() -> Self {
        Self {
            feed_to_book: Histogram::new(LATENCY_BOUNDS),
            gap_recovery: Histogram::new(RECOVERY_BOUNDS),
            ws_clients: AtomicI64::new(0),
            ws_lagged: AtomicU64::new(0),
            sink_rows_written: AtomicU64::new(0),
            sink_rows_dropped: AtomicU64::new(0),
            sink_write_errors: AtomicU64::new(0),
        }
    }
}

fn family(out: &mut String, name: &str, kind: &str, help: &str) {
    let _ = writeln!(out, "# HELP {name} {help}\n# TYPE {name} {kind}");
}

impl Metrics {
    pub fn render(&self, engine: &Engine, now: TsMicros) -> String {
        let mut out = String::with_capacity(4096);
        let o = &mut out;

        family(
            o,
            "lob_frames_total",
            "counter",
            "Raw frames received (message throughput: use rate()).",
        );
        let _ = writeln!(o, "lob_frames_total {}", engine.frames());
        family(
            o,
            "lob_decode_errors_total",
            "counter",
            "Frames that could not be decoded.",
        );
        let _ = writeln!(o, "lob_decode_errors_total {}", engine.decode_errors());

        let symbols = engine.symbols();
        type Getter = fn(&book_core::SeqStats) -> u64;
        let counters: [(&str, &str, Getter); 7] = [
            (
                "lob_deltas_applied_total",
                "Deltas applied to the book.",
                |s| s.deltas_applied,
            ),
            (
                "lob_snapshots_applied_total",
                "Snapshots applied to the book.",
                |s| s.snapshots_applied,
            ),
            ("lob_sequence_gaps_total", "Sequence gaps detected.", |s| {
                s.gaps
            }),
            (
                "lob_duplicates_total",
                "Duplicate or already-applied deltas ignored.",
                |s| s.duplicates,
            ),
            (
                "lob_rejected_total",
                "Events rejected by book validation.",
                |s| s.rejected,
            ),
            (
                "lob_recoveries_total",
                "Completed resynchronizations.",
                |s| s.recoveries,
            ),
            (
                "lob_drift_total",
                "Times a same-sequence snapshot disagreed with the book.",
                |s| s.drift,
            ),
        ];
        for (name, help, get) in counters {
            family(o, name, "counter", help);
            for s in &symbols {
                if let Some(stats) = engine.seq_stats(s) {
                    let _ = writeln!(o, "{name}{{symbol=\"{s}\"}} {}", get(&stats));
                }
            }
        }

        type Gauge = fn(&crate::engine::FeedHealth) -> i64;
        let gauges: [(&str, &str, Gauge); 6] = [
            ("lob_last_seq", "Last sequence number applied.", |h| {
                h.last_seq as i64
            }),
            (
                "lob_synced",
                "1 when the book is synced, 0 while awaiting or resyncing.",
                |h| (h.state == SyncState::Synced) as i64,
            ),
            (
                "lob_feed_stale",
                "1 when no data arrived within the staleness threshold.",
                |h| h.stale as i64,
            ),
            (
                "lob_feed_idle_ms",
                "Milliseconds since the last frame for the symbol.",
                |h| h.stale_ms,
            ),
            (
                "lob_book_crossed",
                "1 while best bid is above best ask.",
                |h| (h.integrity == Integrity::Crossed) as i64,
            ),
            (
                "lob_crossed_events_total",
                "Times the book entered a crossed state.",
                |h| h.crossed_events as i64,
            ),
        ];
        for (name, help, get) in gauges {
            let kind = if name.ends_with("_total") {
                "counter"
            } else {
                "gauge"
            };
            family(o, name, kind, help);
            for s in &symbols {
                if let Some(h) = engine.health(s, now) {
                    let _ = writeln!(o, "{name}{{symbol=\"{s}\"}} {}", get(&h));
                }
            }
        }

        let cp = engine.checkpoint_stats();
        family(
            o,
            "lob_replay_checkpoints_total",
            "counter",
            "Replay checkpoints by verification result (book accuracy).",
        );
        let _ = writeln!(
            o,
            "lob_replay_checkpoints_total{{result=\"matched\"}} {}",
            cp.matched
        );
        let _ = writeln!(
            o,
            "lob_replay_checkpoints_total{{result=\"mismatched\"}} {}",
            cp.mismatched
        );

        self.feed_to_book.render(
            o,
            "lob_feed_to_book_seconds",
            "Time from frame hand-off by the feed source to book update (in-process, monotonic clock).",
        );
        self.gap_recovery.render(
            o,
            "lob_gap_recovery_seconds",
            "Time from gap detection to the book being synced again.",
        );

        family(
            o,
            "lob_ws_clients",
            "gauge",
            "Connected WebSocket subscribers.",
        );
        let _ = writeln!(o, "lob_ws_clients {}", self.ws_clients.load(Relaxed));
        family(
            o,
            "lob_ws_lagged_total",
            "counter",
            "Times a slow subscriber was re-based with a snapshot.",
        );
        let _ = writeln!(o, "lob_ws_lagged_total {}", self.ws_lagged.load(Relaxed));
        family(
            o,
            "lob_sink_rows_written_total",
            "counter",
            "Rows accepted by ClickHouse.",
        );
        let _ = writeln!(
            o,
            "lob_sink_rows_written_total {}",
            self.sink_rows_written.load(Relaxed)
        );
        family(
            o,
            "lob_sink_rows_dropped_total",
            "counter",
            "Rows dropped (queue full or writes rejected after retries).",
        );
        let _ = writeln!(
            o,
            "lob_sink_rows_dropped_total {}",
            self.sink_rows_dropped.load(Relaxed)
        );
        family(
            o,
            "lob_sink_write_errors_total",
            "counter",
            "Failed ClickHouse insert attempts.",
        );
        let _ = writeln!(
            o,
            "lob_sink_write_errors_total {}",
            self.sink_write_errors.load(Relaxed)
        );
        out
    }
}
