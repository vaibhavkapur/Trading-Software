//! `me` – command-line front end for the matching engine (simulated only).

use matching_engine::gateway;
use matching_engine::journal;
use matching_engine::loadgen::{LoadGen, LoadProfile};
use matching_engine::pipeline::{Pipeline, PipelineOptions};
use matching_engine::sequencer::{LogicalClock, Sequencer, SystemClock};
use matching_engine::{Command, Engine, SequencedCommand, Side};
use std::error::Error;
use std::fs::File;
use std::io::{self, BufRead, BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};

const USAGE: &str = "\
me - deterministic price-time-priority matching engine (SIMULATED / PAPER ONLY)

USAGE:
  me run [FILE|-] [--emit-log PATH]
      Read wire commands (one JSON object per line; '#' comments allowed),
      sequence them with a logical clock and print the events as JSON lines.
      --emit-log also writes the sequenced command log, for use with `replay`.

  me replay FILE
      Replay a sequenced command log and print the events as JSON lines.
      The output is byte-identical for identical input.

  me serve [--addr 127.0.0.1:9001] [--data-dir data] [--fsync]
           [--snapshot-every N]
      Run the TCP gateway with a journaled engine. State is recovered from
      the data directory on start. A final snapshot is written on Ctrl-C.

  me recover --data-dir DIR
      Recover state from a data directory and print a summary (read-only
      apart from truncating a torn final log line).

  me gen [--seed 42] [--count 1000] [--symbols 2]
      Print seeded synthetic wire commands.
";

type Res<T> = Result<T, Box<dyn Error>>;

struct Args(Vec<String>);

impl Args {
    /// Removes `--name VALUE` and returns the value.
    fn opt(&mut self, name: &str) -> Res<Option<String>> {
        match self.0.iter().position(|a| a == name) {
            None => Ok(None),
            Some(i) if i + 1 < self.0.len() => {
                let value = self.0.remove(i + 1);
                self.0.remove(i);
                Ok(Some(value))
            }
            Some(_) => Err(format!("{name} needs a value").into()),
        }
    }

    fn num<T: std::str::FromStr>(&mut self, name: &str, default: T) -> Res<T> {
        match self.opt(name)? {
            None => Ok(default),
            Some(v) => v
                .parse()
                .map_err(|_| format!("{name}: invalid value {v:?}").into()),
        }
    }

    fn flag(&mut self, name: &str) -> bool {
        match self.0.iter().position(|a| a == name) {
            Some(i) => {
                self.0.remove(i);
                true
            }
            None => false,
        }
    }

    fn positional(&mut self) -> Res<Option<String>> {
        if let Some(bad) = self.0.iter().find(|a| a.starts_with("--")) {
            return Err(format!("unknown option {bad}").into());
        }
        match self.0.len() {
            0 => Ok(None),
            1 => Ok(Some(self.0.remove(0))),
            _ => Err(format!("unexpected arguments: {:?}", &self.0[1..]).into()),
        }
    }
}

fn main() {
    let mut argv: Vec<String> = std::env::args().skip(1).collect();
    if argv.is_empty() || argv.iter().any(|a| a == "-h" || a == "--help") {
        print!("{USAGE}");
        return;
    }
    let sub = argv.remove(0);
    let args = Args(argv);
    let result = match sub.as_str() {
        "run" => run(args),
        "replay" => replay(args),
        "serve" => serve(args),
        "recover" => recover(args),
        "gen" => gen(args),
        other => Err(format!("unknown subcommand {other:?}\n\n{USAGE}").into()),
    };
    match result {
        Ok(()) => {}
        // A closed stdout (e.g. `me gen | head`) is not an error.
        Err(e)
            if e.downcast_ref::<io::Error>()
                .is_some_and(|e| e.kind() == io::ErrorKind::BrokenPipe) => {}
        Err(e) => {
            eprintln!("error: {e}");
            std::process::exit(1);
        }
    }
}

fn open_input(path: Option<&str>) -> Res<Box<dyn BufRead>> {
    Ok(match path {
        None | Some("-") => Box::new(io::stdin().lock()),
        Some(p) => Box::new(BufReader::new(
            File::open(p).map_err(|e| format!("{p}: {e}"))?,
        )),
    })
}

fn run(mut args: Args) -> Res<()> {
    let emit_log = args.opt("--emit-log")?;
    let input = args.positional()?;
    let mut log = match emit_log {
        Some(p) => Some(BufWriter::new(
            File::create(&p).map_err(|e| format!("{p}: {e}"))?,
        )),
        None => None,
    };
    let mut sequencer = Sequencer::new(LogicalClock::default());
    let mut engine = Engine::new();
    let mut events = Vec::new();
    let mut out = BufWriter::new(io::stdout().lock());
    for (n, line) in open_input(input.as_deref())?.lines().enumerate() {
        let line = line?;
        let text = line.trim();
        if text.is_empty() || text.starts_with('#') {
            continue;
        }
        let cmd: Command =
            serde_json::from_str(text).map_err(|e| format!("line {}: {e}", n + 1))?;
        let sc = sequencer.sequence(cmd);
        if let Some(log) = &mut log {
            serde_json::to_writer(&mut *log, &sc)?;
            log.write_all(b"\n")?;
        }
        events.clear();
        engine.process(&sc, &mut events)?;
        for event in &events {
            serde_json::to_writer(&mut out, event)?;
            out.write_all(b"\n")?;
        }
    }
    if let Some(mut log) = log {
        log.flush()?;
    }
    out.flush()?;
    Ok(())
}

fn replay(mut args: Args) -> Res<()> {
    let path = args.positional()?.ok_or("replay needs a FILE")?;
    let log = journal::read_jsonl::<SequencedCommand>(Path::new(&path))?;
    if log.torn_tail {
        eprintln!("warning: ignoring torn final line in {path}");
    }
    let mut engine = Engine::new();
    let mut out = BufWriter::new(io::stdout().lock());
    let mut write_error = None;
    let stats = journal::replay(&mut engine, &log.records, |event| {
        if write_error.is_none() {
            let written = serde_json::to_writer(&mut out, event)
                .map_err(io::Error::from)
                .and_then(|()| out.write_all(b"\n"));
            write_error = written.err();
        }
    })?;
    if let Some(e) = write_error {
        return Err(e.into());
    }
    out.flush()?;
    eprintln!(
        "replayed {} command(s), skipped {} duplicate(s), {} open order(s)",
        stats.applied,
        stats.skipped,
        engine.open_orders()
    );
    Ok(())
}

fn print_summary(engine: &Engine) {
    eprintln!(
        "last_seq={} open_orders={}",
        engine.last_seq(),
        engine.open_orders()
    );
    for symbol in engine.symbols() {
        let book = engine.book(symbol).expect("listed symbol has a book");
        let fmt = |side| match book.depth(side, 1).first() {
            Some((px, qty, n)) => format!("{qty}@{px} ({n} order(s))"),
            None => "-".to_string(),
        };
        eprintln!(
            "  {symbol}: bid {} | ask {} | resting {}",
            fmt(Side::Buy),
            fmt(Side::Sell),
            book.len()
        );
    }
}

fn recover(mut args: Args) -> Res<()> {
    let dir = PathBuf::from(args.opt("--data-dir")?.ok_or("recover needs --data-dir")?);
    args.positional()?;
    if !dir.is_dir() {
        return Err(format!("{}: not a directory", dir.display()).into());
    }
    let (engine, report) = journal::recover(&dir, |_| {})?;
    eprintln!(
        "recovered from {}: snapshot_seq={:?} replayed={} skipped={} torn_tail={}",
        dir.display(),
        report.snapshot_seq,
        report.replay.applied,
        report.replay.skipped,
        report.torn_tail
    );
    print_summary(&engine);
    Ok(())
}

fn gen(mut args: Args) -> Res<()> {
    let seed = args.num("--seed", 42u64)?;
    let count = args.num("--count", 1000usize)?;
    let symbols = args.num("--symbols", 2usize)?;
    args.positional()?;
    let mut out = BufWriter::new(io::stdout().lock());
    for cmd in LoadGen::new(LoadProfile::new(seed, symbols)).take(count) {
        serde_json::to_writer(&mut out, &cmd)?;
        out.write_all(b"\n")?;
    }
    out.flush()?;
    Ok(())
}

fn serve(mut args: Args) -> Res<()> {
    let addr = args
        .opt("--addr")?
        .unwrap_or_else(|| "127.0.0.1:9001".to_string());
    let dir = PathBuf::from(
        args.opt("--data-dir")?
            .unwrap_or_else(|| "data".to_string()),
    );
    let opts = PipelineOptions {
        fsync: args.flag("--fsync"),
        snapshot_every: args.num("--snapshot-every", 0u64)?,
    };
    args.positional()?;

    let (pipeline, report) = Pipeline::open(&dir, Box::new(SystemClock), opts)?;
    eprintln!(
        "recovered: snapshot_seq={:?} replayed={} torn_tail={}",
        report.snapshot_seq, report.replay.applied, report.torn_tail
    );
    print_summary(pipeline.engine());

    let runtime = tokio::runtime::Runtime::new()?;
    let mut pipeline = runtime.block_on(async {
        let listener = tokio::net::TcpListener::bind(&addr).await?;
        eprintln!(
            "SIMULATED matching engine listening on {} (data dir {}); Ctrl-C to stop",
            listener.local_addr()?,
            dir.display()
        );
        let (tx, rx) = tokio::sync::mpsc::channel(4096);
        let engine_thread = gateway::spawn_engine(pipeline, rx)?;
        tokio::select! {
            result = gateway::serve(listener, tx) => result?,
            _ = shutdown_signal() => eprintln!("shutting down"),
        }
        // Dropping `serve` closed every connection and request sender, so
        // the matching loop drains what is queued and returns the pipeline.
        let joined = tokio::task::spawn_blocking(move || engine_thread.join()).await?;
        joined.map_err(|_| -> Box<dyn Error> { "matching loop panicked".into() })
    })?;
    runtime.shutdown_background();
    if let Some(path) = pipeline.snapshot()? {
        eprintln!("final snapshot: {}", path.display());
    }
    print_summary(pipeline.engine());
    Ok(())
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        if let Ok(mut term) = signal(SignalKind::terminate()) {
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {}
                _ = term.recv() => {}
            }
            return;
        }
    }
    let _ = tokio::signal::ctrl_c().await;
}
