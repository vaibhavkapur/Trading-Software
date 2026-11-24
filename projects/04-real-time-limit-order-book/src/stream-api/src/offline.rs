//! Offline drivers: recording a simulated session, replaying a capture with
//! checkpoint verification, and the ladder benchmark.

use crate::engine::{CheckpointStats, Engine, EngineConfig, FeedHealth, Output};
use book_core::{BTreeLadder, FeedEvent, Ladder, SequencedBook, Side, VecLadder};
use feed_adapter::{Fault, Frame, Manifest, Normalizer, SimConfig, SimSession, WIRE_SCHEMA};
use serde::Serialize;
use std::collections::BTreeMap;
use std::time::Instant;

/// Feeds frames to an engine and folds everything it publishes into a digest.
pub struct Driver {
    pub engine: Engine,
    digest: u64,
    pub status_counts: BTreeMap<&'static str, u64>,
    last_recv: i64,
}

impl Driver {
    pub fn new(engine: Engine) -> Self {
        Self {
            engine,
            digest: 0xcbf2_9ce4_8422_2325,
            status_counts: BTreeMap::new(),
            last_recv: 0,
        }
    }

    /// Returns the symbols the engine asked to resynchronize.
    pub fn feed(&mut self, frame: &Frame) -> Vec<String> {
        self.last_recv = frame.recv_ts;
        let mut outputs = self.engine.on_frame(&frame.raw, frame.recv_ts);
        outputs.extend(self.engine.tick(frame.recv_ts));
        let mut resync = Vec::new();
        for output in outputs {
            let json = serde_json::to_string(&output).expect("outputs serialize");
            for b in json.bytes() {
                self.digest ^= u64::from(b);
                self.digest = self.digest.wrapping_mul(0x0000_0100_0000_01b3);
            }
            match output {
                Output::ResyncRequest { symbol } => resync.push(symbol),
                Output::Status(ev) => *self.status_counts.entry(ev.kind.as_str()).or_default() += 1,
                _ => {}
            }
        }
        resync
    }

    pub fn digest(&self) -> String {
        format!("{:016x}", self.digest)
    }

    pub fn health(&self) -> Vec<FeedHealth> {
        self.engine
            .symbols()
            .iter()
            .filter_map(|s| self.engine.health(s, self.last_recv))
            .collect()
    }
}

/// Runs a simulated session in closed loop with an engine (so snapshot
/// requests are answered) and returns what went over the wire.
pub fn record(
    sim: SimConfig,
    plan: Vec<(u64, Fault)>,
    faults_spec: &str,
    events: u64,
    engine_cfg: EngineConfig,
) -> (Vec<Frame>, Manifest) {
    let seed = sim.seed;
    let checkpoint_depth = sim.checkpoint_depth;
    let mut session = SimSession::with_plan(sim, plan);
    let instruments = session.instruments();
    let mut driver = Driver::new(Engine::new(engine_cfg, instruments.clone()));
    let mut frames = Vec::new();
    // Run past `events` if needed so the capture ends with every book synced.
    let limit = events + 10_000;
    while session.events() < events || (!driver.engine.all_synced() && session.events() < limit) {
        let frame = session.next_frame(None);
        for symbol in driver.feed(&frame) {
            session.request_snapshot(&symbol);
        }
        frames.push(frame);
    }
    let manifest = Manifest {
        manifest_version: feed_adapter::replay::MANIFEST_VERSION,
        wire_schema: WIRE_SCHEMA.to_string(),
        data_origin: "simulated".to_string(),
        instruments,
        seed,
        events: session.events(),
        faults: faults_spec.to_string(),
        frames: frames.len() as u64,
        output_digest: driver.digest(),
        checkpoint_depth,
        checkpoints: session.take_checkpoints(),
    };
    (frames, manifest)
}

#[derive(Debug, Clone, Serialize)]
pub struct ReplayReport {
    pub frames: u64,
    pub decode_errors: u64,
    pub output_digest: String,
    pub expected_digest: String,
    pub digest_matches: bool,
    pub checkpoints: CheckpointStats,
    pub checkpoints_skipped: u64,
    pub status_counts: BTreeMap<&'static str, u64>,
    pub health: Vec<FeedHealth>,
    /// Wall-clock figures measured on this run and this machine only.
    pub measured: Measured,
}

#[derive(Debug, Clone, Serialize)]
pub struct Measured {
    pub wall_ms: f64,
    pub frames_per_sec: f64,
    pub per_frame_p50_ns: u64,
    pub per_frame_p99_ns: u64,
    pub per_frame_max_ns: u64,
}

impl ReplayReport {
    /// A replay passes when it reproduced the recorded output exactly and no
    /// checkpoint disagreed with ground truth.
    pub fn passed(&self) -> bool {
        self.digest_matches && self.checkpoints.mismatched == 0
    }
}

/// Replays a capture as fast as possible, verifying checkpoints and digest.
pub fn replay(frames: &[Frame], manifest: &Manifest, engine_cfg: EngineConfig) -> ReplayReport {
    let mut engine = Engine::new(engine_cfg, manifest.instruments.clone());
    engine.set_checkpoints(&manifest.checkpoints, manifest.checkpoint_depth);
    let mut driver = Driver::new(engine);
    let mut per_frame = Vec::with_capacity(frames.len());
    let start = Instant::now();
    for frame in frames {
        let t = Instant::now();
        driver.feed(frame);
        per_frame.push(t.elapsed().as_nanos() as u64);
    }
    let wall = start.elapsed();
    per_frame.sort_unstable();
    let pct = |p: f64| -> u64 {
        if per_frame.is_empty() {
            return 0;
        }
        per_frame[((per_frame.len() - 1) as f64 * p).round() as usize]
    };
    let checkpoints = driver.engine.checkpoint_stats();
    ReplayReport {
        frames: frames.len() as u64,
        decode_errors: driver.engine.decode_errors(),
        output_digest: driver.digest(),
        expected_digest: manifest.output_digest.clone(),
        digest_matches: driver.digest() == manifest.output_digest,
        checkpoints,
        checkpoints_skipped: checkpoints.skipped(),
        status_counts: driver.status_counts.clone(),
        health: driver.health(),
        measured: Measured {
            wall_ms: wall.as_secs_f64() * 1e3,
            frames_per_sec: frames.len() as f64 / wall.as_secs_f64().max(1e-9),
            per_frame_p50_ns: pct(0.50),
            per_frame_p99_ns: pct(0.99),
            per_frame_max_ns: per_frame.last().copied().unwrap_or(0),
        },
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct BenchResult {
    pub ladder: &'static str,
    pub events: usize,
    pub levels_per_side: i64,
    pub ns_per_event: f64,
    pub events_per_sec: f64,
    /// Proves both ladders ended in the same state.
    pub final_checksum: String,
}

/// Generates `events` clean deltas for a book `levels` deep per side.
pub fn bench_events(events: usize, levels: i64, seed: u64) -> Vec<FeedEvent> {
    let cfg = SimConfig {
        seed,
        levels,
        ..SimConfig::default()
    };
    let mut session = SimSession::new(cfg);
    let normalizer = Normalizer::new(session.instruments());
    (0..=events)
        .filter_map(|_| {
            normalizer
                .decode(&session.next_frame(None).raw)
                .ok()
                .flatten()
        })
        .collect()
}

fn bench_one<L: Ladder>(events: &[FeedEvent], levels: i64, rounds: usize) -> BenchResult {
    let mut best = f64::MAX;
    let mut checksum = 0;
    for _ in 0..rounds {
        let mut book = SequencedBook::<L>::new(events[0].symbol());
        let mut sink = 0u64;
        let start = Instant::now();
        for ev in events {
            book.on_event(ev);
            // Reading the touch after every update is part of the workload.
            sink = sink.wrapping_add(book.book().best(Side::Bid).map_or(0, |l| l.qty));
            sink = sink.wrapping_add(book.book().best(Side::Ask).map_or(0, |l| l.qty));
        }
        let ns = start.elapsed().as_nanos() as f64 / events.len() as f64;
        std::hint::black_box(sink);
        best = best.min(ns);
        checksum = book.book().checksum(usize::MAX);
    }
    BenchResult {
        ladder: L::NAME,
        events: events.len(),
        levels_per_side: levels,
        ns_per_event: best,
        events_per_sec: 1e9 / best,
        final_checksum: format!("{checksum:016x}"),
    }
}

/// Times both ladder implementations on the same stream (best of `rounds`).
pub fn bench(events: usize, levels: i64, seed: u64, rounds: usize) -> Vec<BenchResult> {
    let stream = bench_events(events, levels, seed);
    vec![
        bench_one::<BTreeLadder>(&stream, levels, rounds.max(1)),
        bench_one::<VecLadder>(&stream, levels, rounds.max(1)),
    ]
}
