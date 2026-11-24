//! Replay reproducibility: fixed seeds and fixtures give fixed outputs.

use feed_adapter::{
    manifest_path, parse_fault_plan, read_capture, read_manifest, Frame, SimConfig, SimSymbol,
};
use std::path::PathBuf;
use stream_api::engine::EngineConfig;
use stream_api::offline::{record, replay};

const FAULTS: &str =
    "drop@700:3,dup@1500,reorder@2300,cross@3100,garbage@3900,skew@4700,drop@5500:40";

fn recording(seed: u64, checkpoint_every: u64) -> (Vec<Frame>, feed_adapter::Manifest) {
    let sim = SimConfig {
        symbols: vec![SimSymbol::new("XYZ", 10_000), SimSymbol::new("ABC", 12_500)],
        seed,
        checkpoint_every,
        ..SimConfig::default()
    };
    record(
        sim,
        parse_fault_plan(FAULTS).unwrap(),
        FAULTS,
        8_000,
        EngineConfig::default(),
    )
}

#[test]
fn same_seed_produces_the_same_capture() {
    let (frames_a, manifest_a) = recording(11, 100);
    let (frames_b, manifest_b) = recording(11, 100);
    assert_eq!(frames_a, frames_b);
    assert_eq!(manifest_a, manifest_b);
    let (frames_c, manifest_c) = recording(12, 100);
    assert_ne!(frames_a, frames_c);
    assert_ne!(manifest_a.output_digest, manifest_c.output_digest);
}

#[test]
fn replay_reproduces_recorded_output_and_reference_depth() {
    let (frames, manifest) = recording(11, 100);
    let report = replay(&frames, &manifest, EngineConfig::default());
    assert!(
        report.digest_matches,
        "{} != {}",
        report.output_digest, report.expected_digest
    );
    assert_eq!(report.checkpoints.mismatched, 0);
    assert!(report.checkpoints.total >= 70, "{:?}", report.checkpoints);
    // Only checkpoints that fell inside a gap may be unverifiable.
    assert!(report.checkpoints_skipped <= 8, "{:?}", report.checkpoints);
    assert!(report.passed());
    assert_eq!(report.decode_errors, 1);
    // Every scheduled fault left its trace.
    for kind in [
        "gap_detected",
        "resynced",
        "duplicate",
        "crossed",
        "uncrossed",
        "decode_error",
        "clock_skew",
    ] {
        assert!(
            report.status_counts.get(kind).copied().unwrap_or(0) >= 1,
            "missing {kind}: {:?}",
            report.status_counts
        );
    }
    for h in &report.health {
        assert_eq!(h.state, book_core::SyncState::Synced);
    }
    // Replaying again gives the same digest: no hidden state or clock reads.
    assert_eq!(
        replay(&frames, &manifest, EngineConfig::default()).output_digest,
        report.output_digest
    );
}

#[test]
fn tampered_capture_fails_verification() {
    let (mut frames, manifest) = recording(11, 10);
    // Rewrite every bid size in the opening snapshot.
    let mut snapshot: serde_json::Value = serde_json::from_str(&frames[0].raw).unwrap();
    for level in snapshot["bids"].as_array_mut().unwrap() {
        level[1] = "7777".into();
    }
    frames[0].raw = snapshot.to_string();

    let report = replay(&frames, &manifest, EngineConfig::default());
    assert!(!report.digest_matches);
    assert!(
        report.checkpoints.mismatched >= 1,
        "{:?}",
        report.checkpoints
    );
    assert!(!report.passed());
    assert!(report.status_counts.contains_key("checkpoint_mismatch"));
}

#[test]
fn dropping_a_frame_from_a_capture_is_detected() {
    let (mut frames, manifest) = recording(11, 100);
    frames.remove(300);
    let report = replay(&frames, &manifest, EngineConfig::default());
    assert!(!report.digest_matches);
    // The gap is seen, but this capture holds no snapshot to answer it until
    // the next recorded one, so correctness is preserved rather than faked.
    assert!(report.status_counts["gap_detected"] >= 1);
    assert_eq!(report.checkpoints.mismatched, 0);
}

fn fixture() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../examples/fixtures/session-faults.ndjson")
}

/// The committed fixture pins the dataset: any change to decoding, sequencing
/// or book logic that alters published output shows up here.
#[test]
fn committed_fixture_replays_to_its_pinned_digest() {
    let path = fixture();
    let manifest =
        read_manifest(&manifest_path(&path)).expect("fixture manifest (run `make fixtures`)");
    let frames = read_capture(&path).expect("fixture capture (run `make fixtures`)");
    assert_eq!(manifest.data_origin, "simulated");
    assert_eq!(frames.len() as u64, manifest.frames);
    let report = replay(&frames, &manifest, EngineConfig::default());
    assert!(report.passed(), "{report:#?}");
    assert!(report.checkpoints.matched > 0);
}
