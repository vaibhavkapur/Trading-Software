//! Pinned fixtures: a sequenced command log plus the exact event bytes it
//! must produce. Any change to matching semantics, event schema or
//! serialisation shows up here as a diff. Regenerate deliberately with
//! `make fixtures` and review the result.

use super::*;
use matching_engine::journal;
use matching_engine::loadgen::{LoadGen, LoadProfile};
use std::path::PathBuf;
use std::process::Command as Proc;

fn fixture(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/scenarios/fixtures")
        .join(name)
}

fn check(name: &str) {
    let log = journal::read_jsonl::<SequencedCommand>(&fixture(&format!("{name}.commands.jsonl")))
        .unwrap();
    assert!(!log.torn_tail && !log.records.is_empty());
    let expected = std::fs::read(fixture(&format!("{name}.events.jsonl"))).unwrap();

    let mut engine = Engine::new();
    let mut events = Vec::new();
    journal::replay(&mut engine, &log.records, |e| events.push(*e)).unwrap();
    engine.check_invariants().unwrap();
    let actual = events_to_bytes(&events);
    if actual != expected {
        let (a, e) = (
            String::from_utf8_lossy(&actual),
            String::from_utf8_lossy(&expected),
        );
        let line = a.lines().zip(e.lines()).position(|(x, y)| x != y);
        panic!(
            "{name}: events differ from fixture (first differing line: {:?}; {} vs {} lines)",
            line.map(|i| i + 1),
            a.lines().count(),
            e.lines().count()
        );
    }
}

#[test]
fn handwritten_session_replays_to_pinned_events() {
    check("session");
}

#[test]
fn generated_load_replays_to_pinned_events() {
    check("gen-seed7");
}

#[test]
fn load_generator_output_is_pinned_by_seed() {
    let log =
        journal::read_jsonl::<SequencedCommand>(&fixture("gen-seed7.commands.jsonl")).unwrap();
    let regenerated: Vec<Command> = LoadGen::new(LoadProfile::new(7, 2))
        .take(log.records.len())
        .collect();
    let pinned: Vec<Command> = log.records.iter().map(|c| c.cmd).collect();
    assert_eq!(regenerated, pinned);
}

#[test]
fn cli_replay_output_is_byte_identical_to_the_fixture() {
    for name in ["session", "gen-seed7"] {
        let out = Proc::new(env!("CARGO_BIN_EXE_me"))
            .arg("replay")
            .arg(fixture(&format!("{name}.commands.jsonl")))
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "{}",
            String::from_utf8_lossy(&out.stderr)
        );
        let expected = std::fs::read(fixture(&format!("{name}.events.jsonl"))).unwrap();
        assert!(
            out.stdout == expected,
            "{name}: CLI replay differs from fixture"
        );
    }
}
