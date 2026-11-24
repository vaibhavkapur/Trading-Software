use super::*;
use matching_engine::journal::{self, Durable, JournalError};
use matching_engine::loadgen::{LoadGen, LoadProfile};
use matching_engine::pipeline::{Pipeline, PipelineError};
use matching_engine::sequencer::{Clock, LogicalClock, Sequencer};
use std::io::{self, Write};
use std::sync::{Arc, Mutex};

fn generated_log(n: usize) -> Vec<SequencedCommand> {
    let mut sequencer = Sequencer::new(LogicalClock::default());
    LoadGen::new(LoadProfile::new(3, 2))
        .take(n)
        .map(|c| sequencer.sequence(c))
        .collect()
}

fn replay_all(log: &[SequencedCommand]) -> (Engine, Vec<Event>, journal::ReplayStats) {
    let mut engine = Engine::new();
    let mut events = Vec::new();
    let stats = journal::replay(&mut engine, log, |e| events.push(*e)).unwrap();
    (engine, events, stats)
}

/// A "disk" that accepts a fixed number of writes and then fails forever.
struct FlakyDisk {
    writes_left: usize,
    data: Arc<Mutex<Vec<u8>>>,
}

impl Write for FlakyDisk {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        if self.writes_left == 0 {
            return Err(io::Error::other("simulated: no space left on device"));
        }
        self.writes_left -= 1;
        self.data.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl Durable for FlakyDisk {}

#[test]
fn rejected_journal_write_halts_the_engine_before_state_changes() {
    let data = Arc::new(Mutex::new(Vec::new()));
    let disk = FlakyDisk {
        writes_left: 2,
        data: data.clone(),
    };
    let mut p = Pipeline::with_sinks(Box::new(LogicalClock::default()), Box::new(disk), None);

    p.submit(limit(1, Side::Buy, 100, 10)).unwrap();
    p.submit(limit(2, Side::Buy, 101, 10)).unwrap();
    let before = p.engine().snapshot();

    // This order would trade, but its journal write fails: it must not be
    // applied and must not be acknowledged.
    let err = p.submit(limit(3, Side::Sell, 100, 5)).unwrap_err();
    assert!(matches!(err, PipelineError::JournalWrite(_)), "{err}");
    assert_eq!(p.engine().snapshot(), before);

    // Fail-stop: nothing else gets in either, even though this one is valid.
    let err = p.submit(limit(4, Side::Buy, 99, 1)).unwrap_err();
    assert!(matches!(err, PipelineError::Halted), "{err}");
    assert!(p.is_halted());
    assert_eq!(p.engine().snapshot(), before);

    // What reached the disk replays to exactly the state the engine holds.
    let bytes = data.lock().unwrap().clone();
    let log: Vec<SequencedCommand> = String::from_utf8(bytes)
        .unwrap()
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect();
    assert_eq!(replay_all(&log).0.snapshot(), before);
}

#[test]
fn duplicate_delivery_of_log_records_is_idempotent() {
    let log = generated_log(300);
    let (clean_engine, clean_events, _) = replay_all(&log);

    // Every record delivered twice, plus a whole block re-sent later.
    let mut noisy: Vec<SequencedCommand> = log.iter().flat_map(|c| [*c, *c]).collect();
    noisy.extend_from_slice(&log[100..200]);
    let (engine, events, stats) = replay_all(&noisy);

    assert_eq!((stats.applied, stats.skipped), (300, 400));
    assert_eq!(events_to_bytes(&events), events_to_bytes(&clean_events));
    assert_eq!(engine.snapshot(), clean_engine.snapshot());
}

#[test]
fn client_retry_of_a_new_order_cannot_double_the_order() {
    // The ack was lost, so the client re-sends the same NEW. It is sequenced
    // as a fresh command and rejected, because the id is still live.
    let mut p = Pipeline::in_memory(Box::new(LogicalClock::default()));
    p.submit(limit(1, Side::Buy, 100, 10)).unwrap();
    let (_, events) = p.submit(limit(1, Side::Buy, 100, 10)).unwrap();
    assert!(matches!(
        events,
        [Event {
            kind: EventKind::Rejected {
                reason: RejectReason::DuplicateOrderId,
                ..
            },
            ..
        }]
    ));
    let top = p.engine().book(Symbol::new("XYZ").unwrap()).unwrap().top();
    assert_eq!(top.bid, Some((100, 10)));
}

#[test]
fn missing_log_record_stops_replay_at_the_gap() {
    let mut log = generated_log(50);
    log.remove(20); // seq 21 is lost
    let mut engine = Engine::new();
    let err = journal::replay(&mut engine, &log, |_| {}).unwrap_err();
    assert_eq!(
        err,
        SequenceError {
            expected: 21,
            got: 22
        }
    );
    // State is exactly "everything before the gap" - nothing after it leaked in.
    assert_eq!(engine.last_seq(), 20);
    assert_eq!(engine.snapshot(), replay_all(&log[..20]).0.snapshot());
}

#[test]
fn stale_command_is_refused_and_leaves_state_untouched() {
    let log = generated_log(50);
    let (mut engine, _, _) = replay_all(&log);
    let before = engine.snapshot();
    let mut out = Vec::new();
    assert_eq!(
        engine.process(&log[10], &mut out),
        Err(SequenceError {
            expected: 51,
            got: 11
        })
    );
    assert!(out.is_empty());
    assert_eq!(engine.snapshot(), before);
}

#[test]
fn clock_skew_cannot_move_venue_time_backwards() {
    struct Skewed(std::vec::IntoIter<u64>);
    impl Clock for Skewed {
        fn now_ns(&mut self) -> u64 {
            self.0.next().unwrap()
        }
    }
    // Wall clock steps back twice (e.g. NTP correction), then recovers.
    let clock = Skewed(vec![1_000, 900, 950, 1_000, 2_000].into_iter());
    let mut sequencer = Sequencer::new(clock);
    let stamped: Vec<(u64, u64)> = (1..=5)
        .map(|i| sequencer.sequence(Command::Cancel { order_id: i }))
        .map(|sc| (sc.seq, sc.ts_ns))
        .collect();
    assert_eq!(
        stamped,
        vec![(1, 1_000), (2, 1_000), (3, 1_000), (4, 1_000), (5, 2_000)]
    );
}

#[test]
fn corruption_in_the_middle_of_a_log_is_an_error_not_a_silent_skip() {
    let dir = std::env::temp_dir().join(format!("me-scenario-corrupt-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join(journal::COMMANDS_FILE);
    let log = generated_log(5);
    let mut text: Vec<String> = log
        .iter()
        .map(|c| serde_json::to_string(c).unwrap())
        .collect();
    text[2] = text[2].replace("\"seq\":3", "\"seq\":oops");
    std::fs::write(&path, text.join("\n") + "\n").unwrap();

    let err = journal::read_jsonl::<SequencedCommand>(&path).unwrap_err();
    assert!(
        matches!(err, JournalError::Corrupt { line: 3, .. }),
        "{err}"
    );
    assert!(journal::recover(&dir, |_| {}).is_err());

    // By contrast a torn *final* line is expected after a crash and tolerated.
    std::fs::write(&path, text[..2].join("\n") + "\n{\"seq\":3,\"ts").unwrap();
    let ok = journal::read_jsonl::<SequencedCommand>(&path).unwrap();
    assert!(ok.torn_tail);
    assert_eq!(ok.records, log[..2]);
    std::fs::remove_dir_all(&dir).unwrap();
}
