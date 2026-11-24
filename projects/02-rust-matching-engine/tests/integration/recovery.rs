use super::*;
use matching_engine::journal::{self, COMMANDS_FILE, EVENTS_FILE, SNAPSHOT_DIR};
use matching_engine::loadgen::{LoadGen, LoadProfile};
use matching_engine::pipeline::{Pipeline, PipelineOptions};
use matching_engine::sequencer::LogicalClock;
use std::fs;
use std::io::Write;

fn open(dir: &Path, opts: PipelineOptions) -> (Pipeline, journal::RecoveryReport) {
    Pipeline::open(dir, Box::new(LogicalClock::default()), opts).unwrap()
}

fn workload(n: usize) -> Vec<Command> {
    LoadGen::new(LoadProfile::new(11, 2)).take(n).collect()
}

/// Events of an uninterrupted in-memory run, as the reference.
fn reference_events(cmds: &[Command]) -> Vec<Event> {
    let mut p = Pipeline::in_memory(Box::new(LogicalClock::default()));
    let mut all = Vec::new();
    for cmd in cmds {
        all.extend_from_slice(p.submit(*cmd).unwrap().1);
    }
    all
}

fn events_on_disk(dir: &Path) -> Vec<Event> {
    let log = journal::read_jsonl::<Event>(&dir.join(EVENTS_FILE)).unwrap();
    assert!(!log.torn_tail);
    log.records
}

#[test]
fn restart_recovers_identical_state_and_continues_the_sequence() {
    let dir = TempDir::new("restart");
    let cmds = workload(600);
    let (first, rest) = cmds.split_at(400);

    let (mut p, report) = open(dir.path(), PipelineOptions::default());
    assert_eq!((report.snapshot_seq, report.replay.applied), (None, 0));
    for cmd in first {
        p.submit(*cmd).unwrap();
    }
    let before = p.engine().snapshot();
    drop(p); // "crash": no snapshot, no clean shutdown

    // The restarted clock carries on from where the first run's left off, so
    // timestamps line up with the uninterrupted reference run.
    let clock = Box::new(LogicalClock::new(401, 1));
    let (mut p, report) = Pipeline::open(dir.path(), clock, PipelineOptions::default()).unwrap();
    assert_eq!(report.replay.applied, 400);
    assert_eq!(p.engine().snapshot(), before);
    for cmd in rest {
        p.submit(*cmd).unwrap();
    }
    assert_eq!(p.engine().last_seq(), 600);
    drop(p);

    // The event log across the restart equals one uninterrupted run:
    // nothing lost, nothing duplicated.
    assert_eq!(events_on_disk(dir.path()), reference_events(&cmds));
}

#[test]
fn recovery_from_snapshot_plus_log_tail_matches_full_replay() {
    let dir = TempDir::new("snapshot");
    let cmds = workload(1000);
    let opts = PipelineOptions {
        fsync: false,
        snapshot_every: 300,
    };
    let (mut p, _) = open(dir.path(), opts.clone());
    for cmd in &cmds {
        p.submit(*cmd).unwrap();
    }
    let expected = p.engine().snapshot();
    drop(p);

    let snapshots = fs::read_dir(dir.path().join(SNAPSHOT_DIR)).unwrap().count();
    assert_eq!(snapshots, 3, "snapshots at seq 300, 600, 900");

    let (engine, report) = journal::recover(dir.path(), |_| {}).unwrap();
    assert_eq!(report.snapshot_seq, Some(900));
    assert_eq!((report.replay.applied, report.replay.skipped), (100, 900));
    assert_eq!(engine.snapshot(), expected);

    // Same answer with the snapshots thrown away (pure log replay).
    fs::remove_dir_all(dir.path().join(SNAPSHOT_DIR)).unwrap();
    let (engine, report) = journal::recover(dir.path(), |_| {}).unwrap();
    assert_eq!((report.snapshot_seq, report.replay.applied), (None, 1000));
    assert_eq!(engine.snapshot(), expected);
}

#[test]
fn corrupt_newest_snapshot_falls_back_to_an_older_one() {
    let dir = TempDir::new("badsnap");
    let cmds = workload(500);
    let opts = PipelineOptions {
        fsync: false,
        snapshot_every: 200,
    };
    let (mut p, _) = open(dir.path(), opts);
    for cmd in &cmds {
        p.submit(*cmd).unwrap();
    }
    let expected = p.engine().snapshot();
    drop(p);

    let newest = dir
        .path()
        .join(SNAPSHOT_DIR)
        .join(format!("snapshot-{:020}.json", 400));
    fs::write(&newest, b"{\"version\":1,\"last_seq\":").unwrap();

    let (engine, report) = journal::recover(dir.path(), |_| {}).unwrap();
    assert_eq!(report.snapshot_seq, Some(200));
    assert_eq!(engine.snapshot(), expected);
}

#[test]
fn torn_final_line_is_dropped_and_the_log_stays_appendable() {
    let dir = TempDir::new("torn");
    let (mut p, _) = open(dir.path(), PipelineOptions::default());
    p.submit(limit(1, Side::Buy, 100, 10)).unwrap();
    p.submit(limit(2, Side::Buy, 101, 10)).unwrap();
    drop(p);

    // Simulate a crash half-way through writing the third command.
    let path = dir.path().join(COMMANDS_FILE);
    let mut f = fs::OpenOptions::new().append(true).open(&path).unwrap();
    f.write_all(br#"{"seq":3,"ts_ns":3,"cmd":{"type":"NEW","order_"#)
        .unwrap();
    drop(f);

    let (mut p, report) = open(dir.path(), PipelineOptions::default());
    assert!(report.torn_tail);
    assert_eq!(p.engine().last_seq(), 2);
    let (seq, _) = p.submit(limit(3, Side::Sell, 101, 4)).unwrap();
    assert_eq!(
        seq, 3,
        "the torn command was never acknowledged, so seq 3 is reused"
    );
    drop(p);

    let log = journal::read_jsonl::<SequencedCommand>(&path).unwrap();
    assert!(!log.torn_tail);
    assert_eq!(
        log.records.iter().map(|c| c.seq).collect::<Vec<_>>(),
        vec![1, 2, 3]
    );
    let (engine, _) = journal::recover(dir.path(), |_| {}).unwrap();
    assert_eq!(
        engine.book(Symbol::new("XYZ").unwrap()).unwrap().top().bid,
        Some((101, 6))
    );
}

#[test]
fn event_log_that_lags_the_command_log_is_caught_up_on_recovery() {
    let dir = TempDir::new("lag");
    let cmds = workload(200);
    let (mut p, _) = open(dir.path(), PipelineOptions::default());
    for cmd in &cmds {
        p.submit(*cmd).unwrap();
    }
    drop(p);

    // Lose the tail of the event log (plus leave a torn line behind).
    let path = dir.path().join(EVENTS_FILE);
    let bytes = fs::read(&path).unwrap();
    fs::write(&path, &bytes[..bytes.len() / 2]).unwrap();

    let (p, _) = open(dir.path(), PipelineOptions::default());
    drop(p);
    assert_eq!(events_on_disk(dir.path()), reference_events(&cmds));
}

#[test]
fn venue_time_never_goes_backwards_across_a_restart() {
    let dir = TempDir::new("clock");
    let (mut p, _) = Pipeline::open(
        dir.path(),
        Box::new(LogicalClock::new(5_000, 10)),
        Default::default(),
    )
    .unwrap();
    p.submit(limit(1, Side::Buy, 100, 1)).unwrap();
    p.submit(limit(2, Side::Buy, 100, 1)).unwrap();
    drop(p);

    // Restart with a clock that is far behind the journal.
    let (mut p, _) = open(dir.path(), PipelineOptions::default());
    let (_, events) = p.submit(limit(3, Side::Buy, 100, 1)).unwrap();
    assert_eq!(
        events[0].ts_ns, 5_010,
        "clamped to the last journaled timestamp"
    );
}
