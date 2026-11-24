//! The single-writer pipeline: decode → sequence → book → metrics.
//!
//! `Engine` does no I/O and never reads a clock; callers pass the local
//! receive time in. That keeps it deterministic, so a capture replays to
//! byte-identical output and staleness logic is testable without sleeping.

use analytics::{compute_metric, BestQuote, BookMetric, OfiTracker};
use book_core::{
    checksum_levels, BookDelta, FeedEvent, Integrity, Level, Outcome, Seq, SeqStats, SequencedBook,
    Side, SyncState, TsMicros,
};
use feed_adapter::{checksum_hex, Checkpoint, Instrument, Normalizer};
use serde::Serialize;
use std::collections::{BTreeMap, HashMap};

#[derive(Debug, Clone)]
pub struct EngineConfig {
    /// Maximum depth per side included in published book views.
    pub depth: usize,
    /// Levels per side that count towards depth and depth-imbalance metrics.
    pub metric_depth: usize,
    /// A symbol is stale when nothing was received for longer than this.
    pub stale_after_ms: i64,
    /// How often an unanswered snapshot request is repeated.
    pub resync_retry_ms: i64,
    /// Venue timestamps this far ahead of the local clock are flagged.
    pub skew_tolerance_ms: i64,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            depth: 50,
            metric_depth: 5,
            stale_after_ms: 2_000,
            resync_retry_ms: 1_000,
            skew_tolerance_ms: 1_000,
        }
    }
}

/// What subscribers see. `state`, `integrity` and `stale` must be honoured by
/// any consumer before treating the levels as current.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct BookView {
    pub symbol: String,
    pub seq: Seq,
    pub state: SyncState,
    pub integrity: Integrity,
    pub stale: bool,
    pub bids: Vec<Level>,
    pub asks: Vec<Level>,
    /// Venue time of the last event received for this symbol.
    pub event_ts: Option<TsMicros>,
    /// Local time that event was received.
    pub recv_ts: Option<TsMicros>,
    /// Local time this view was produced.
    pub asof: TsMicros,
    /// Checksum of exactly the levels in this view.
    pub checksum: String,
}

impl BookView {
    pub fn truncated(&self, depth: usize) -> BookView {
        if depth >= self.bids.len() && depth >= self.asks.len() {
            return self.clone();
        }
        let bids: Vec<Level> = self.bids.iter().take(depth).copied().collect();
        let asks: Vec<Level> = self.asks.iter().take(depth).copied().collect();
        BookView {
            checksum: checksum_hex(checksum_levels(&bids, &asks)),
            bids,
            asks,
            ..self.clone()
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct FeedHealth {
    pub symbol: String,
    pub state: SyncState,
    pub last_seq: Seq,
    pub gap_count: u64,
    pub duplicate_count: u64,
    pub rejected_count: u64,
    pub recovery_count: u64,
    pub drift_count: u64,
    pub crossed_events: u64,
    pub integrity: Integrity,
    /// Milliseconds since anything was received for this symbol (local clock
    /// only, so it is meaningful without clock synchronization).
    pub stale_ms: i64,
    pub stale: bool,
    pub last_event_ts: Option<TsMicros>,
    pub last_recv_ts: Option<TsMicros>,
    /// Local receive time minus venue event time. This mixes two clocks: it is
    /// an indication, not a latency measurement.
    pub venue_lag_ms: Option<i64>,
    pub clock_skew_suspected: bool,
    pub last_recovery_ms: Option<i64>,
    pub unsynced_for_ms: Option<i64>,
    pub buffered: usize,
    pub asof: TsMicros,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum StatusKind {
    SnapshotApplied,
    GapDetected,
    ResyncRequested,
    Resynced,
    Duplicate,
    StaleSnapshot,
    Rejected,
    DecodeError,
    Crossed,
    Locked,
    Uncrossed,
    Stale,
    Fresh,
    DriftDetected,
    ClockSkew,
    ClockSkewCleared,
    CheckpointMismatch,
    SourceRestarted,
    ClientLagged,
}

impl StatusKind {
    pub fn as_str(self) -> &'static str {
        match self {
            StatusKind::SnapshotApplied => "snapshot_applied",
            StatusKind::GapDetected => "gap_detected",
            StatusKind::ResyncRequested => "resync_requested",
            StatusKind::Resynced => "resynced",
            StatusKind::Duplicate => "duplicate",
            StatusKind::StaleSnapshot => "stale_snapshot",
            StatusKind::Rejected => "rejected",
            StatusKind::DecodeError => "decode_error",
            StatusKind::Crossed => "crossed",
            StatusKind::Locked => "locked",
            StatusKind::Uncrossed => "uncrossed",
            StatusKind::Stale => "stale",
            StatusKind::Fresh => "fresh",
            StatusKind::DriftDetected => "drift_detected",
            StatusKind::ClockSkew => "clock_skew",
            StatusKind::ClockSkewCleared => "clock_skew_cleared",
            StatusKind::CheckpointMismatch => "checkpoint_mismatch",
            StatusKind::SourceRestarted => "source_restarted",
            StatusKind::ClientLagged => "client_lagged",
        }
    }
}

/// Symbol used for events that cannot be attributed to one book.
pub const ALL_SYMBOLS: &str = "*";

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct StatusEvent {
    pub symbol: String,
    /// Local time.
    pub ts: TsMicros,
    pub kind: StatusKind,
    pub seq: Option<Seq>,
    pub value_ms: Option<i64>,
    pub detail: String,
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Output {
    /// A delta applied to a synced book.
    Delta(BookDelta),
    /// A new full-depth base image: first sync, recovery, or drift correction.
    Snapshot(BookView),
    Status(StatusEvent),
    /// Instruction to the feed source, not for subscribers.
    ResyncRequest {
        symbol: String,
    },
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct CheckpointStats {
    pub total: u64,
    pub matched: u64,
    pub mismatched: u64,
}

impl CheckpointStats {
    /// Checkpoints whose sequence number was never observed in a synced
    /// state (it fell inside a gap), so nothing could be compared.
    pub fn skipped(&self) -> u64 {
        self.total - self.matched - self.mismatched
    }
}

struct SymbolState {
    book: SequencedBook,
    last_recv: Option<TsMicros>,
    last_event_ts: Option<TsMicros>,
    unsynced_since: Option<TsMicros>,
    last_request: Option<TsMicros>,
    integrity: Integrity,
    crossed_events: u64,
    stale: bool,
    skewed: bool,
    last_recovery_ms: Option<i64>,
    ofi: OfiTracker,
}

pub struct Engine {
    cfg: EngineConfig,
    normalizer: Normalizer,
    symbols: BTreeMap<String, SymbolState>,
    started: Option<TsMicros>,
    frames: u64,
    decode_errors: u64,
    checkpoints: HashMap<(String, Seq), String>,
    checkpoint_depth: usize,
    checkpoint_stats: CheckpointStats,
}

fn status(
    symbol: &str,
    ts: TsMicros,
    kind: StatusKind,
    seq: Option<Seq>,
    value_ms: Option<i64>,
    detail: impl Into<String>,
) -> Output {
    Output::Status(StatusEvent {
        symbol: symbol.to_string(),
        ts,
        kind,
        seq,
        value_ms,
        detail: detail.into(),
    })
}

impl Engine {
    pub fn new(cfg: EngineConfig, instruments: Vec<Instrument>) -> Self {
        let symbols = instruments
            .iter()
            .map(|i| {
                (
                    i.symbol.clone(),
                    SymbolState {
                        book: SequencedBook::new(i.symbol.clone()),
                        last_recv: None,
                        last_event_ts: None,
                        unsynced_since: None,
                        last_request: None,
                        integrity: Integrity::Ok,
                        crossed_events: 0,
                        stale: false,
                        skewed: false,
                        last_recovery_ms: None,
                        ofi: OfiTracker::default(),
                    },
                )
            })
            .collect();
        Self {
            cfg,
            normalizer: Normalizer::new(instruments),
            symbols,
            started: None,
            frames: 0,
            decode_errors: 0,
            checkpoints: HashMap::new(),
            checkpoint_depth: 0,
            checkpoint_stats: CheckpointStats::default(),
        }
    }

    /// Installs ground-truth checkpoints to verify against while processing.
    pub fn set_checkpoints(&mut self, checkpoints: &[Checkpoint], depth: usize) {
        self.checkpoints = checkpoints
            .iter()
            .map(|c| ((c.symbol.clone(), c.seq), c.checksum.clone()))
            .collect();
        self.checkpoint_depth = depth;
        self.checkpoint_stats = CheckpointStats {
            total: self.checkpoints.len() as u64,
            ..Default::default()
        };
    }

    pub fn config(&self) -> &EngineConfig {
        &self.cfg
    }
    pub fn symbols(&self) -> Vec<String> {
        self.symbols.keys().cloned().collect()
    }
    pub fn instrument(&self, symbol: &str) -> Option<&Instrument> {
        self.normalizer.instrument(symbol)
    }
    pub fn frames(&self) -> u64 {
        self.frames
    }
    pub fn decode_errors(&self) -> u64 {
        self.decode_errors
    }
    pub fn checkpoint_stats(&self) -> CheckpointStats {
        self.checkpoint_stats
    }
    pub fn seq_stats(&self, symbol: &str) -> Option<SeqStats> {
        self.symbols.get(symbol).map(|s| s.book.stats())
    }
    pub fn all_synced(&self) -> bool {
        self.symbols
            .values()
            .all(|s| s.book.state() == SyncState::Synced)
    }

    /// Processes one raw frame received at local time `recv`.
    pub fn on_frame(&mut self, raw: &str, recv: TsMicros) -> Vec<Output> {
        self.frames += 1;
        self.started.get_or_insert(recv);
        let mut out = Vec::new();
        let ev = match self.normalizer.decode(raw) {
            Err(e) => {
                self.decode_errors += 1;
                out.push(status(
                    ALL_SYMBOLS,
                    recv,
                    StatusKind::DecodeError,
                    None,
                    None,
                    e.to_string(),
                ));
                return out;
            }
            Ok(None) => {
                for (symbol, st) in &mut self.symbols {
                    st.last_recv = Some(recv);
                    if std::mem::take(&mut st.stale) {
                        out.push(status(
                            symbol,
                            recv,
                            StatusKind::Fresh,
                            None,
                            None,
                            "heartbeat",
                        ));
                    }
                }
                return out;
            }
            Ok(Some(ev)) => ev,
        };
        let symbol = ev.symbol().to_string();
        let retry_us = self.cfg.resync_retry_ms * 1_000;
        let skew_us = self.cfg.skew_tolerance_ms * 1_000;
        let st = self
            .symbols
            .get_mut(&symbol)
            .expect("normalizer only accepts configured symbols");

        st.last_recv = Some(recv);
        st.last_event_ts = Some(ev.venue_ts());
        if std::mem::take(&mut st.stale) {
            out.push(status(
                &symbol,
                recv,
                StatusKind::Fresh,
                None,
                None,
                "feed resumed",
            ));
        }
        let ahead_us = ev.venue_ts() - recv;
        let skewed = ahead_us > skew_us;
        if skewed != st.skewed {
            st.skewed = skewed;
            out.push(if skewed {
                status(
                    &symbol,
                    recv,
                    StatusKind::ClockSkew,
                    None,
                    Some(ahead_us / 1_000),
                    "venue timestamp is ahead of the local clock",
                )
            } else {
                status(&symbol, recv, StatusKind::ClockSkewCleared, None, None, "")
            });
        }

        let outcome = st.book.on_event(&ev);
        let state = st.book.state();
        let last_seq = st.book.last_seq();
        let mut rebased = false;
        let mut applied = false;
        match outcome {
            Outcome::DeltaApplied { recovered, .. } => {
                applied = true;
                if recovered {
                    rebased = true;
                    let ms = st.unsynced_since.map(|t| (recv - t) / 1_000);
                    st.last_recovery_ms = ms;
                    out.push(status(
                        &symbol,
                        recv,
                        StatusKind::Resynced,
                        Some(last_seq),
                        ms,
                        "late delta closed the gap (reorder buffer)",
                    ));
                } else if state == SyncState::Synced {
                    st.ofi.update(
                        st.book.book().best(Side::Bid),
                        st.book.book().best(Side::Ask),
                    );
                    if let FeedEvent::Delta(d) = &ev {
                        out.push(Output::Delta(d.clone()));
                    }
                }
            }
            Outcome::SnapshotApplied {
                seq,
                drained,
                synced,
                was,
                drift,
            } => {
                applied = true;
                if drift {
                    rebased = true;
                    out.push(status(
                        &symbol,
                        recv,
                        StatusKind::DriftDetected,
                        Some(seq),
                        None,
                        "book differed from venue snapshot at the same sequence; replaced",
                    ));
                }
                match (was, synced) {
                    (SyncState::Resyncing, true) => {
                        rebased = true;
                        let ms = st.unsynced_since.map(|t| (recv - t) / 1_000);
                        st.last_recovery_ms = ms;
                        out.push(status(
                            &symbol,
                            recv,
                            StatusKind::Resynced,
                            Some(last_seq),
                            ms,
                            format!("snapshot at seq {seq} + {drained} buffered deltas"),
                        ));
                    }
                    (SyncState::AwaitingSnapshot, true) => {
                        rebased = true;
                        out.push(status(
                            &symbol,
                            recv,
                            StatusKind::SnapshotApplied,
                            Some(seq),
                            None,
                            format!("initial sync, {drained} buffered deltas applied"),
                        ));
                    }
                    (_, false) => out.push(status(
                        &symbol,
                        recv,
                        StatusKind::SnapshotApplied,
                        Some(seq),
                        None,
                        "snapshot applied but buffered deltas still have a hole",
                    )),
                    (SyncState::Synced, true) => {}
                }
            }
            Outcome::GapDetected { expected, got } => out.push(status(
                &symbol,
                recv,
                StatusKind::GapDetected,
                Some(got),
                None,
                format!("expected seq {expected}, got {got}"),
            )),
            Outcome::Buffered { .. } => {}
            Outcome::Duplicate { seq, last_seq } => out.push(status(
                &symbol,
                recv,
                StatusKind::Duplicate,
                Some(seq),
                None,
                format!("seq {seq} already applied (last {last_seq}); ignored"),
            )),
            Outcome::StaleSnapshot { seq, last_seq } => out.push(status(
                &symbol,
                recv,
                StatusKind::StaleSnapshot,
                Some(seq),
                None,
                format!("snapshot seq {seq} is older than book seq {last_seq}; ignored"),
            )),
            Outcome::Rejected { error } => out.push(status(
                &symbol,
                recv,
                StatusKind::Rejected,
                None,
                None,
                error.to_string(),
            )),
        }

        if state == SyncState::Synced {
            st.unsynced_since = None;
            st.last_request = None;
        } else {
            st.unsynced_since.get_or_insert(recv);
            if st.last_request.is_none_or(|t| recv - t >= retry_us) {
                st.last_request = Some(recv);
                out.push(status(
                    &symbol,
                    recv,
                    StatusKind::ResyncRequested,
                    None,
                    None,
                    "snapshot requested",
                ));
                out.push(Output::ResyncRequest {
                    symbol: symbol.clone(),
                });
            }
        }

        let integrity = st.book.book().integrity();
        if integrity != st.integrity {
            st.integrity = integrity;
            let kind = match integrity {
                Integrity::Crossed => {
                    st.crossed_events += 1;
                    StatusKind::Crossed
                }
                Integrity::Locked => StatusKind::Locked,
                Integrity::Ok => StatusKind::Uncrossed,
            };
            let px = |side| {
                st.book
                    .book()
                    .best(side)
                    .map_or("-".to_string(), |l| l.price_ticks.to_string())
            };
            let quote = format!("bid {} / ask {} (ticks)", px(Side::Bid), px(Side::Ask));
            out.push(status(&symbol, recv, kind, Some(last_seq), None, quote));
        }

        if applied && state == SyncState::Synced && !self.checkpoints.is_empty() {
            if let Some(expected) = self.checkpoints.remove(&(symbol.clone(), last_seq)) {
                let got = checksum_hex(st.book.book().checksum(self.checkpoint_depth));
                if got == expected {
                    self.checkpoint_stats.matched += 1;
                } else {
                    self.checkpoint_stats.mismatched += 1;
                    out.push(status(
                        &symbol,
                        recv,
                        StatusKind::CheckpointMismatch,
                        Some(last_seq),
                        None,
                        format!("expected {expected}, got {got}"),
                    ));
                }
            }
        }

        if rebased {
            st.ofi.reset();
            // Full depth: subscribers that apply deltas need every level.
            let view = self
                .view(&symbol, usize::MAX, recv)
                .expect("symbol exists: it was just updated");
            out.push(Output::Snapshot(view));
        }
        out
    }

    /// Time-driven transitions: staleness and repeated snapshot requests.
    pub fn tick(&mut self, now: TsMicros) -> Vec<Output> {
        let started = *self.started.get_or_insert(now);
        let stale_us = self.cfg.stale_after_ms * 1_000;
        let retry_us = self.cfg.resync_retry_ms * 1_000;
        let mut out = Vec::new();
        for (symbol, st) in &mut self.symbols {
            let idle = now - st.last_recv.unwrap_or(started);
            if idle > stale_us && !st.stale {
                st.stale = true;
                out.push(status(
                    symbol,
                    now,
                    StatusKind::Stale,
                    None,
                    Some(idle / 1_000),
                    "no data received; displayed prices are not current",
                ));
            }
            if st.book.state() != SyncState::Synced {
                let since = *st.unsynced_since.get_or_insert(now);
                let due = match st.last_request {
                    Some(t) => now - t >= retry_us,
                    None => now - since >= retry_us,
                };
                if due {
                    st.last_request = Some(now);
                    out.push(status(
                        symbol,
                        now,
                        StatusKind::ResyncRequested,
                        None,
                        None,
                        "snapshot requested (retry)",
                    ));
                    out.push(Output::ResyncRequest {
                        symbol: symbol.clone(),
                    });
                }
            }
        }
        out
    }

    /// Forgets all book state after the upstream session restarted.
    pub fn reset(&mut self, now: TsMicros) -> Vec<Output> {
        let mut out = Vec::new();
        for (symbol, st) in &mut self.symbols {
            st.book.reset();
            st.unsynced_since = Some(now);
            st.last_request = None;
            st.integrity = Integrity::Ok;
            st.ofi.reset();
            out.push(status(
                symbol,
                now,
                StatusKind::SourceRestarted,
                None,
                None,
                "upstream session restarted; awaiting snapshot",
            ));
        }
        out
    }

    fn idle_us(&self, st: &SymbolState, now: TsMicros) -> i64 {
        (now - st.last_recv.or(self.started).unwrap_or(now)).max(0)
    }

    pub fn view(&self, symbol: &str, depth: usize, now: TsMicros) -> Option<BookView> {
        let st = self.symbols.get(symbol)?;
        let book = st.book.book();
        let bids = book.top(Side::Bid, depth);
        let asks = book.top(Side::Ask, depth);
        Some(BookView {
            symbol: symbol.to_string(),
            seq: st.book.last_seq(),
            state: st.book.state(),
            integrity: book.integrity(),
            stale: self.idle_us(st, now) > self.cfg.stale_after_ms * 1_000,
            checksum: checksum_hex(checksum_levels(&bids, &asks)),
            bids,
            asks,
            event_ts: st.last_event_ts,
            recv_ts: st.last_recv,
            asof: now,
        })
    }

    pub fn health(&self, symbol: &str, now: TsMicros) -> Option<FeedHealth> {
        let st = self.symbols.get(symbol)?;
        let stats = st.book.stats();
        let idle = self.idle_us(st, now);
        Some(FeedHealth {
            symbol: symbol.to_string(),
            state: st.book.state(),
            last_seq: st.book.last_seq(),
            gap_count: stats.gaps,
            duplicate_count: stats.duplicates,
            rejected_count: stats.rejected,
            recovery_count: stats.recoveries,
            drift_count: stats.drift,
            crossed_events: st.crossed_events,
            integrity: st.book.book().integrity(),
            stale_ms: idle / 1_000,
            stale: idle > self.cfg.stale_after_ms * 1_000,
            last_event_ts: st.last_event_ts,
            last_recv_ts: st.last_recv,
            venue_lag_ms: st
                .last_recv
                .zip(st.last_event_ts)
                .map(|(r, e)| (r - e) / 1_000),
            clock_skew_suspected: st.skewed,
            last_recovery_ms: st.last_recovery_ms,
            unsynced_for_ms: st.unsynced_since.map(|t| (now - t).max(0) / 1_000),
            buffered: st.book.buffered(),
            asof: now,
        })
    }

    pub fn quote(&self, symbol: &str) -> Option<BestQuote> {
        let st = self.symbols.get(symbol)?;
        let book = st.book.book();
        Some(BestQuote::from_levels(
            symbol,
            &book.top(Side::Bid, 1),
            &book.top(Side::Ask, 1),
            st.last_event_ts.unwrap_or(0),
        ))
    }

    /// Metrics for a synced book, consuming the OFI window. `None` while the
    /// book is not trustworthy: no metric is better than a wrong one.
    pub fn take_metric(&mut self, symbol: &str) -> Option<BookMetric> {
        let depth = self.cfg.metric_depth;
        let st = self.symbols.get_mut(symbol)?;
        if st.book.state() != SyncState::Synced {
            return None;
        }
        let book = st.book.book();
        Some(compute_metric(
            symbol,
            st.last_event_ts.unwrap_or(0),
            st.book.last_seq(),
            &book.top(Side::Bid, depth),
            &book.top(Side::Ask, depth),
            st.ofi.take(),
        ))
    }
}
