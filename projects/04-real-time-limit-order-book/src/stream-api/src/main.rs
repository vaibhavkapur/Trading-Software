//! `lob`: serve a live (simulated or replayed) book, record captures, verify
//! replays, and benchmark the ladder implementations.

use anyhow::{bail, Context};
use clap::{Parser, Subcommand, ValueEnum};
use feed_adapter::{
    manifest_path, parse_fault_plan, read_capture, read_manifest, write_frame, write_manifest,
    SimConfig, SimSymbol,
};
use std::io::Write;
use std::net::SocketAddr;
use std::path::PathBuf;
use stream_api::clickhouse::ClickHouseConfig;
use stream_api::engine::EngineConfig;
use stream_api::offline;
use stream_api::server::{self, ServeConfig, SourceKind};

#[derive(Parser)]
#[command(
    name = "lob",
    version,
    about = "Real-time limit order book (simulated data only)"
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Clone, Copy, ValueEnum)]
enum Source {
    Sim,
    Replay,
}

#[derive(clap::Args, Clone)]
struct EngineArgs {
    /// Milliseconds without data before a symbol is reported stale.
    #[arg(long, default_value_t = 2_000)]
    stale_after_ms: i64,
    /// Interval between repeated snapshot requests while unsynced.
    #[arg(long, default_value_t = 1_000)]
    resync_retry_ms: i64,
    /// Levels per side counted by the depth metrics.
    #[arg(long, default_value_t = 5)]
    metric_depth: usize,
    /// Maximum levels per side in published views.
    #[arg(long, default_value_t = 50)]
    max_depth: usize,
}

impl EngineArgs {
    fn config(&self) -> EngineConfig {
        EngineConfig {
            depth: self.max_depth.max(1),
            metric_depth: self.metric_depth.max(1),
            stale_after_ms: self.stale_after_ms,
            resync_retry_ms: self.resync_retry_ms,
            ..EngineConfig::default()
        }
    }
}

#[derive(Subcommand)]
enum Cmd {
    /// Run the HTTP/WebSocket server on a simulated or replayed feed.
    Serve {
        #[arg(long, default_value = "127.0.0.1:8080")]
        addr: SocketAddr,
        #[arg(long, value_enum, default_value = "sim")]
        source: Source,
        /// Comma-separated symbols for the simulated venue.
        #[arg(long, default_value = "XYZ,ABC")]
        symbols: String,
        #[arg(long, default_value_t = 42)]
        seed: u64,
        /// Simulated events per second.
        #[arg(long, default_value_t = 400)]
        rate: u32,
        /// Simulated venue sends an unsolicited snapshot every N events (0 = never).
        #[arg(long, default_value_t = 0)]
        snapshot_every: u64,
        /// Capture to replay (requires the matching .manifest.json next to it).
        #[arg(long)]
        file: Option<PathBuf>,
        /// Replay speed multiplier; 0 replays without pacing.
        #[arg(long, default_value_t = 1.0)]
        speed: f64,
        /// Restart the replay when it ends.
        #[arg(long = "loop")]
        looped: bool,
        /// Directory containing the built UI.
        #[arg(long, default_value = "src/ui/dist")]
        ui_dir: PathBuf,
        /// Enable unauthenticated fault-injection and replay-control routes.
        #[arg(long)]
        admin: bool,
        /// ClickHouse HTTP endpoint, e.g. http://localhost:8123 (off when unset).
        #[arg(long, env = "CLICKHOUSE_URL")]
        clickhouse_url: Option<String>,
        #[arg(long, env = "CLICKHOUSE_USER")]
        clickhouse_user: Option<String>,
        #[arg(long, env = "CLICKHOUSE_PASSWORD", hide_env_values = true)]
        clickhouse_password: Option<String>,
        /// Also store every applied delta in ClickHouse.
        #[arg(long)]
        store_deltas: bool,
        #[command(flatten)]
        engine: EngineArgs,
    },
    /// Record a deterministic simulated session (with faults) to a capture file.
    Record {
        #[arg(long)]
        out: PathBuf,
        #[arg(long, default_value_t = 20_000)]
        events: u64,
        #[arg(long, default_value_t = 42)]
        seed: u64,
        #[arg(long, default_value = "XYZ")]
        symbols: String,
        /// Fault schedule, e.g. `drop@2000:3,dup@5000,reorder@8000,cross@11000`.
        #[arg(long, default_value = "")]
        faults: String,
        /// Ground-truth checkpoint every N deltas per symbol.
        #[arg(long, default_value_t = 500)]
        checkpoint_every: u64,
        #[command(flatten)]
        engine: EngineArgs,
    },
    /// Replay a capture offline and verify checkpoints and output digest.
    Replay {
        #[arg(long)]
        file: PathBuf,
        /// Print the full report as JSON.
        #[arg(long)]
        json: bool,
        #[command(flatten)]
        engine: EngineArgs,
    },
    /// Compare the ladder implementations on an identical event stream.
    Bench {
        #[arg(long, default_value_t = 1_000_000)]
        events: usize,
        /// Comma-separated book depths (levels per side) to test.
        #[arg(long, default_value = "20,200,2000")]
        levels: String,
        #[arg(long, default_value_t = 7)]
        seed: u64,
        #[arg(long, default_value_t = 5)]
        rounds: usize,
    },
}

fn sim_symbols(spec: &str) -> anyhow::Result<Vec<SimSymbol>> {
    let symbols: Vec<SimSymbol> = spec
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .enumerate()
        .map(|(i, s)| SimSymbol::new(s, 10_000 + 2_500 * i as i64))
        .collect();
    if symbols.is_empty() {
        bail!("at least one symbol is required");
    }
    Ok(symbols)
}

fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .with_writer(std::io::stderr)
        .init();

    match Cli::parse().cmd {
        Cmd::Serve {
            addr,
            source,
            symbols,
            seed,
            rate,
            snapshot_every,
            file,
            speed,
            looped,
            ui_dir,
            admin,
            clickhouse_url,
            clickhouse_user,
            clickhouse_password,
            store_deltas,
            engine,
        } => {
            let source = match source {
                Source::Sim => SourceKind::Sim {
                    cfg: SimConfig {
                        symbols: sim_symbols(&symbols)?,
                        seed,
                        snapshot_every,
                        ..SimConfig::default()
                    },
                    rate,
                },
                Source::Replay => {
                    let file = file.context("--file is required with --source replay")?;
                    let manifest = read_manifest(&manifest_path(&file))?;
                    SourceKind::Replay {
                        frames: read_capture(&file)?,
                        manifest,
                        speed,
                        looped,
                    }
                }
            };
            let clickhouse = clickhouse_url.filter(|u| !u.is_empty()).map(|url| {
                let mut cfg = ClickHouseConfig::new(url);
                cfg.user = clickhouse_user;
                cfg.password = clickhouse_password;
                cfg
            });
            let cfg = ServeConfig {
                addr,
                source,
                engine: engine.config(),
                ui_dir: Some(ui_dir),
                admin,
                clickhouse,
                store_deltas,
            };
            tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()?
                .block_on(async {
                    let running = server::start(cfg).await?;
                    tracing::info!("serving SIMULATED market data on http://{}", running.addr);
                    if admin {
                        tracing::warn!("admin routes enabled without authentication; keep this bound to localhost");
                    }
                    tokio::select! {
                        _ = tokio::signal::ctrl_c() => {}
                        _ = running.wait() => {}
                    }
                    anyhow::Ok(())
                })
        }
        Cmd::Record {
            out,
            events,
            seed,
            symbols,
            faults,
            checkpoint_every,
            engine,
        } => {
            let plan = parse_fault_plan(&faults).map_err(anyhow::Error::msg)?;
            let sim = SimConfig {
                symbols: sim_symbols(&symbols)?,
                seed,
                checkpoint_every,
                ..SimConfig::default()
            };
            let (frames, manifest) = offline::record(sim, plan, &faults, events, engine.config());
            let mut w = std::io::BufWriter::new(std::fs::File::create(&out)?);
            for frame in &frames {
                write_frame(&mut w, frame)?;
            }
            w.flush()?;
            write_manifest(&manifest_path(&out), &manifest)?;
            println!(
                "recorded {} frames ({} events, {} checkpoints) to {}\noutput digest {}",
                manifest.frames,
                manifest.events,
                manifest.checkpoints.len(),
                out.display(),
                manifest.output_digest
            );
            Ok(())
        }
        Cmd::Replay { file, json, engine } => {
            let manifest = read_manifest(&manifest_path(&file))?;
            let frames = read_capture(&file)?;
            let report = offline::replay(&frames, &manifest, engine.config());
            if json {
                println!("{}", serde_json::to_string_pretty(&report)?);
            } else {
                print_report(&report);
            }
            if !report.passed() {
                bail!("replay verification FAILED");
            }
            Ok(())
        }
        Cmd::Bench {
            events,
            levels,
            seed,
            rounds,
        } => {
            println!(
                "{:<12} {:>10} {:>12} {:>12} {:>14}  final checksum",
                "ladder", "levels", "events", "ns/event", "events/s"
            );
            for levels in levels
                .split(',')
                .filter_map(|l| l.trim().parse::<i64>().ok())
            {
                for r in offline::bench(events, levels.max(2), seed, rounds) {
                    println!(
                        "{:<12} {:>10} {:>12} {:>12.1} {:>14.0}  {}",
                        r.ladder,
                        r.levels_per_side,
                        r.events,
                        r.ns_per_event,
                        r.events_per_sec,
                        r.final_checksum
                    );
                }
            }
            Ok(())
        }
    }
}

fn print_report(r: &offline::ReplayReport) {
    let mark = |ok: bool| if ok { "PASS" } else { "FAIL" };
    println!("replay report (simulated data)");
    println!("  frames            {}", r.frames);
    println!("  decode errors     {}", r.decode_errors);
    println!(
        "  output digest     {} (expected {})  {}",
        r.output_digest,
        r.expected_digest,
        mark(r.digest_matches)
    );
    println!(
        "  checkpoints       {} matched, {} mismatched, {} skipped (inside a gap) of {}  {}",
        r.checkpoints.matched,
        r.checkpoints.mismatched,
        r.checkpoints_skipped,
        r.checkpoints.total,
        mark(r.checkpoints.mismatched == 0)
    );
    println!("  feed events       {:?}", r.status_counts);
    for h in &r.health {
        println!(
            "  {:<6} state={:?} last_seq={} gaps={} duplicates={} recoveries={} crossed={} last_recovery_ms={:?}",
            h.symbol, h.state, h.last_seq, h.gap_count, h.duplicate_count, h.recovery_count, h.crossed_events, h.last_recovery_ms
        );
    }
    println!(
        "  measured here     {:.0} frames/s, per-frame p50 {} ns, p99 {} ns, max {} ns ({:.1} ms wall)",
        r.measured.frames_per_sec,
        r.measured.per_frame_p50_ns,
        r.measured.per_frame_p99_ns,
        r.measured.per_frame_max_ns,
        r.measured.wall_ms
    );
    println!("  result            {}", mark(r.passed()));
}
