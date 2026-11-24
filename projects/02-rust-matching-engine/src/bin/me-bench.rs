//! `me-bench` – reproducible benchmark of the matching core.
//!
//! Measures at the engine boundary (`Engine::process`), with no networking
//! and no journaling, and prints a Markdown report containing everything
//! needed to interpret and reproduce the numbers. Nothing in the report is
//! hard-coded: every figure comes from the run that produced it.

use matching_engine::loadgen::{LoadGen, LoadProfile};
use matching_engine::stats::{Fnv1a, LatencyRecorder};
use matching_engine::{Engine, Event, EventKind, SequencedCommand};
use std::alloc::{GlobalAlloc, Layout, System};
use std::fmt::Write as _;
use std::process::Command as Proc;
use std::sync::atomic::{AtomicU64, Ordering::Relaxed};
use std::time::{Instant, SystemTime, UNIX_EPOCH};

/// Counts every heap allocation so the report can state bytes per command.
struct CountingAlloc;

static ALLOC_BYTES: AtomicU64 = AtomicU64::new(0);
static ALLOC_CALLS: AtomicU64 = AtomicU64::new(0);
static LIVE_BYTES: AtomicU64 = AtomicU64::new(0);
static PEAK_BYTES: AtomicU64 = AtomicU64::new(0);

unsafe impl GlobalAlloc for CountingAlloc {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let ptr = System.alloc(layout);
        if !ptr.is_null() {
            let size = layout.size() as u64;
            ALLOC_BYTES.fetch_add(size, Relaxed);
            ALLOC_CALLS.fetch_add(1, Relaxed);
            let live = LIVE_BYTES.fetch_add(size, Relaxed) + size;
            PEAK_BYTES.fetch_max(live, Relaxed);
        }
        ptr
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        System.dealloc(ptr, layout);
        LIVE_BYTES.fetch_sub(layout.size() as u64, Relaxed);
    }
    // `realloc` uses the default (alloc + copy + dealloc), so a growing Vec
    // is charged its full new size. That over-counts slightly, never under.
}

#[global_allocator]
static GLOBAL: CountingAlloc = CountingAlloc;

struct Config {
    commands: usize,
    warmup: usize,
    seed: u64,
    symbols: usize,
    out: Option<String>,
}

fn parse_args() -> Result<Config, String> {
    let mut cfg = Config {
        commands: 2_000_000,
        warmup: 200_000,
        seed: 42,
        symbols: 4,
        out: None,
    };
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        let mut value = || args.next().ok_or(format!("{arg} needs a value"));
        let bad = |v: String| format!("invalid value {v:?}");
        match arg.as_str() {
            "--commands" => cfg.commands = value()?.parse().map_err(|_| bad(arg.clone()))?,
            "--warmup" => cfg.warmup = value()?.parse().map_err(|_| bad(arg.clone()))?,
            "--seed" => cfg.seed = value()?.parse().map_err(|_| bad(arg.clone()))?,
            "--symbols" => cfg.symbols = value()?.parse().map_err(|_| bad(arg.clone()))?,
            "--out" => cfg.out = Some(value()?),
            "-h" | "--help" => {
                println!(
                    "me-bench [--commands N] [--warmup N] [--seed N] [--symbols N] [--out FILE.md]"
                );
                std::process::exit(0);
            }
            other => return Err(format!("unknown argument {other:?}")),
        }
    }
    if cfg.commands == 0 {
        return Err("--commands must be > 0".into());
    }
    Ok(cfg)
}

fn sh(cmd: &str, args: &[&str]) -> Option<String> {
    let out = Proc::new(cmd).args(args).output().ok()?;
    let text = String::from_utf8(out.stdout).ok()?.trim().to_string();
    (out.status.success() && !text.is_empty()).then_some(text)
}

fn cpu_model() -> String {
    sh("sysctl", &["-n", "machdep.cpu.brand_string"])
        .or_else(|| {
            let info = std::fs::read_to_string("/proc/cpuinfo").ok()?;
            let line = info.lines().find(|l| l.starts_with("model name"))?;
            Some(line.split(':').nth(1)?.trim().to_string())
        })
        .unwrap_or_else(|| "unknown".into())
}

fn memory() -> String {
    sh("sysctl", &["-n", "hw.memsize"])
        .and_then(|b| b.parse::<u64>().ok())
        .map(|b| format!("{} GiB", b >> 30))
        .or_else(|| {
            let info = std::fs::read_to_string("/proc/meminfo").ok()?;
            let kb: u64 = info
                .lines()
                .find(|l| l.starts_with("MemTotal"))?
                .split_whitespace()
                .nth(1)?
                .parse()
                .ok()?;
            Some(format!("{} GiB", kb >> 20))
        })
        .unwrap_or_else(|| "unknown".into())
}

fn os() -> String {
    let kernel = sh("uname", &["-srm"]).unwrap_or_else(|| std::env::consts::OS.into());
    match sh("sw_vers", &["-productVersion"]) {
        Some(v) => format!("macOS {v} ({kernel})"),
        None => kernel,
    }
}

/// The `[profile.release]` block of the manifest this binary was built from.
fn release_profile() -> String {
    include_str!("../../Cargo.toml")
        .split("[profile.release]")
        .nth(1)
        .map(|rest| {
            rest.lines()
                .map(str::trim)
                .take_while(|l| !l.starts_with('['))
                .filter(|l| !l.is_empty())
                .collect::<Vec<_>>()
                .join(", ")
        })
        .unwrap_or_default()
}

/// Smallest non-zero difference between consecutive `Instant` readings, and
/// the median cost of taking one reading pair.
fn timer_characteristics() -> (u64, u64) {
    let mut deltas = LatencyRecorder::with_capacity(200_000);
    let mut granularity = u64::MAX;
    for _ in 0..200_000 {
        let t = Instant::now();
        let d = t.elapsed().as_nanos() as u64;
        deltas.record(d);
        if d > 0 {
            granularity = granularity.min(d);
        }
    }
    (granularity, deltas.percentile(50.0))
}

fn run(engine: &mut Engine, cmds: &[SequencedCommand], out: &mut Vec<Event>) {
    for sc in cmds {
        out.clear();
        engine.process(sc, out).expect("generated log is gap-free");
    }
}

fn utc_now() -> String {
    // Civil-from-days (Howard Hinnant), to avoid a date dependency.
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs()) as i64;
    let (days, rem) = (secs.div_euclid(86_400), secs.rem_euclid(86_400));
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(m <= 2);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        rem % 3600 / 60,
        rem % 60
    )
}

fn main() {
    let cfg = match parse_args() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {e}");
            std::process::exit(2);
        }
    };
    if cfg!(debug_assertions) {
        eprintln!("warning: debug build; numbers are not representative (use --release)");
    }

    let profile = LoadProfile::new(cfg.seed, cfg.symbols);
    let total = cfg.warmup + cfg.commands;
    eprintln!("generating {total} commands...");
    let cmds: Vec<SequencedCommand> = LoadGen::new(profile.clone())
        .take(total)
        .enumerate()
        .map(|(i, cmd)| SequencedCommand {
            seq: i as u64 + 1,
            ts_ns: i as u64 + 1,
            cmd,
        })
        .collect();
    let (warm, measured) = cmds.split_at(cfg.warmup);
    let (granularity, timer_cost) = timer_characteristics();

    // Pass 1: per-command latency.
    eprintln!("pass 1/3: latency...");
    let mut engine = Engine::new();
    let mut out: Vec<Event> = Vec::with_capacity(1024);
    run(&mut engine, warm, &mut out);
    let mut lat = LatencyRecorder::with_capacity(measured.len());
    let (mut events, mut trades, mut rejects) = (0u64, 0u64, 0u64);
    for sc in measured {
        out.clear();
        let t = Instant::now();
        engine
            .process(sc, &mut out)
            .expect("generated log is gap-free");
        lat.record(t.elapsed().as_nanos() as u64);
        events += out.len() as u64;
        for e in &out {
            match e.kind {
                EventKind::Trade { .. } => trades += 1,
                EventKind::Rejected { .. } => rejects += 1,
                _ => {}
            }
        }
    }
    let open_orders = engine.open_orders();
    engine
        .check_invariants()
        .expect("invariants hold after run");
    drop(engine);

    // Pass 2: throughput and allocation, without per-command timer calls.
    eprintln!("pass 2/3: throughput and allocation...");
    let live_before_engine = LIVE_BYTES.load(Relaxed);
    let mut engine = Engine::new();
    run(&mut engine, warm, &mut out);
    let (bytes0, calls0) = (ALLOC_BYTES.load(Relaxed), ALLOC_CALLS.load(Relaxed));
    let t = Instant::now();
    run(&mut engine, measured, &mut out);
    let elapsed = t.elapsed();
    let (bytes1, calls1) = (ALLOC_BYTES.load(Relaxed), ALLOC_CALLS.load(Relaxed));
    let engine_live = LIVE_BYTES.load(Relaxed).saturating_sub(live_before_engine);
    let throughput = measured.len() as f64 / elapsed.as_secs_f64();
    let bytes_per_cmd = (bytes1 - bytes0) as f64 / measured.len() as f64;
    let allocs_per_cmd = (calls1 - calls0) as f64 / measured.len() as f64;
    drop(engine);

    // Pass 3: replay the whole log twice and compare serialised events.
    eprintln!("pass 3/3: replay consistency...");
    let digest = || {
        let mut engine = Engine::new();
        let mut out = Vec::with_capacity(1024);
        let mut hash = Fnv1a::default();
        let mut buf = Vec::with_capacity(256);
        let mut n = 0u64;
        for sc in &cmds {
            out.clear();
            engine
                .process(sc, &mut out)
                .expect("generated log is gap-free");
            for e in &out {
                buf.clear();
                serde_json::to_writer(&mut buf, e).expect("events serialise");
                buf.push(b'\n');
                hash.update(&buf);
                n += 1;
            }
        }
        (hash.0, n)
    };
    let (first, second) = (digest(), digest());
    let consistent = first == second;

    let mut r = String::new();
    let w = &mut r;
    let _ = writeln!(w, "# Matching-core benchmark report\n");
    let _ = writeln!(
        w,
        "Generated by `me-bench` on {}. Simulated load only; every number below was measured by this run.\n",
        utc_now()
    );
    let _ = writeln!(w, "## Environment\n");
    let _ = writeln!(w, "| | |\n|---|---|");
    let _ = writeln!(w, "| CPU | {} |", cpu_model());
    let cores = std::thread::available_parallelism().map_or(0, |n| n.get());
    let _ = writeln!(
        w,
        "| Logical cores | {cores} (benchmark is single-threaded) |"
    );
    let _ = writeln!(w, "| Memory | {} |", memory());
    let _ = writeln!(w, "| OS | {} |", os());
    let _ = writeln!(w, "| Compiler | {} |", env!("ME_RUSTC_VERSION"));
    let _ = writeln!(w, "| Target | {} |", env!("ME_TARGET"));
    let _ = writeln!(
        w,
        "| Build | profile `{}`, opt-level {}; `[profile.release]`: {} |",
        env!("ME_PROFILE"),
        env!("ME_OPT_LEVEL"),
        release_profile()
    );
    let _ = writeln!(
        w,
        "| Crate | matching-engine {} |\n",
        env!("CARGO_PKG_VERSION")
    );
    let _ = writeln!(w, "## Load profile\n");
    let _ = writeln!(w, "- {}", profile.describe());
    let _ = writeln!(
        w,
        "- {} warm-up commands (not measured), then {} measured commands",
        cfg.warmup, cfg.commands
    );
    let _ = writeln!(
        w,
        "- Measured commands produced {events} events ({:.2}/command), {trades} trades, {rejects} rejects; {open_orders} orders resting at the end",
        events as f64 / measured.len() as f64
    );
    let _ = writeln!(
        w,
        "- Reproduce: `cargo run --release --bin me-bench -- --commands {} --warmup {} --seed {} --symbols {}`\n",
        cfg.commands, cfg.warmup, cfg.seed, cfg.symbols
    );
    let _ = writeln!(w, "## Results\n");
    let _ = writeln!(w, "| Metric | Value |\n|---|---|");
    let _ = writeln!(
        w,
        "| Throughput (untimed pass) | {:.0} commands/s ({:.1} ns/command mean) |",
        throughput,
        1e9 / throughput
    );
    for (name, p) in [("p50", 50.0), ("p99", 99.0), ("p99.9", 99.9)] {
        let _ = writeln!(w, "| Latency {name} | {} ns |", lat.percentile(p));
    }
    let _ = writeln!(
        w,
        "| Latency min / mean / max | {} / {:.0} / {} ns |",
        lat.min(),
        lat.mean(),
        lat.max()
    );
    let _ = writeln!(
        w,
        "| Allocated bytes per command | {bytes_per_cmd:.2} ({allocs_per_cmd:.4} allocator calls/command) |"
    );
    let _ = writeln!(
        w,
        "| Engine heap after run | {:.2} MiB live ({open_orders} resting orders) |",
        engine_live as f64 / (1 << 20) as f64
    );
    let _ = writeln!(
        w,
        "| Process peak heap | {:.2} MiB (includes the pre-generated command log and latency samples) |",
        PEAK_BYTES.load(Relaxed) as f64 / (1 << 20) as f64
    );
    let _ = writeln!(
        w,
        "| Replay consistency | {} — two replays of all {total} commands, {} events each, FNV-1a `{:016x}` vs `{:016x}` |\n",
        if consistent { "PASS" } else { "FAIL" },
        first.1,
        first.0,
        second.0
    );
    let _ = writeln!(w, "## Methodology and caveats\n");
    let _ = writeln!(
        w,
        "- Latency is the wall time of one `Engine::process` call (command in, events appended to a reused buffer), taken with `std::time::Instant`. No network, journal or serialisation is inside the measured region."
    );
    let _ = writeln!(
        w,
        "- Timer on this machine: smallest observable step {granularity} ns, median cost of an empty measurement {timer_cost} ns. Latencies are quantised to that step, so values within a few steps of it (typically p50, and a minimum of 0) are only accurate to about one step; the mean ns/command in the throughput row comes from a separate pass with no per-command timer calls and is the better central estimate."
    );
    let _ = writeln!(
        w,
        "- A counting global allocator is installed for the whole run (two atomic updates per allocation), which slightly inflates all timings. Bytes per command covers slab/level/index growth during the measured window after warm-up."
    );
    let _ = writeln!(
        w,
        "- Single run on a non-isolated desktop OS: no core pinning, frequency scaling and background load not controlled. Maximum latency in particular reflects scheduler noise. Treat results as indicative for this machine only."
    );

    print!("{r}");
    if let Some(path) = &cfg.out {
        if let Err(e) = std::fs::write(path, &r) {
            eprintln!("error: writing {path}: {e}");
            std::process::exit(1);
        }
        eprintln!("report written to {path}");
    }
    if !consistent {
        eprintln!("error: replay produced different event streams");
        std::process::exit(1);
    }
}
