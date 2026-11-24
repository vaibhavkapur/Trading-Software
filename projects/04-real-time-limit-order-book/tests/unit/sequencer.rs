use book_core::{BookError, Outcome, SequencedBook, Side, SyncState};
use lob_tests::{lv, set, snap, SYM};

fn synced() -> SequencedBook {
    let mut b = SequencedBook::new(SYM);
    b.on_snapshot(&snap(10, &[(100, 10), (99, 20)], &[(101, 5), (102, 7)]));
    assert_eq!(b.state(), SyncState::Synced);
    b
}

#[test]
fn starts_awaiting_and_buffers_deltas_until_a_snapshot() {
    let mut b: SequencedBook = SequencedBook::new(SYM);
    assert_eq!(b.state(), SyncState::AwaitingSnapshot);
    assert_eq!(
        b.on_delta(&set(11, Side::Bid, 98, 1)),
        Outcome::Buffered { seq: 11 }
    );
    assert_eq!(
        b.on_delta(&set(12, Side::Bid, 97, 2)),
        Outcome::Buffered { seq: 12 }
    );
    // Covered by the snapshot; must be discarded, not applied.
    assert_eq!(
        b.on_delta(&set(10, Side::Bid, 100, 999)),
        Outcome::Buffered { seq: 10 }
    );

    let out = b.on_snapshot(&snap(10, &[(100, 10)], &[(101, 5)]));
    assert_eq!(
        out,
        Outcome::SnapshotApplied {
            seq: 10,
            drained: 2,
            synced: true,
            was: SyncState::AwaitingSnapshot,
            drift: false
        }
    );
    assert_eq!(b.last_seq(), 12);
    assert_eq!(
        b.book().top(Side::Bid, 5),
        vec![lv(100, 10), lv(98, 1), lv(97, 2)]
    );
    assert_eq!(b.buffered(), 0);
}

#[test]
fn in_order_deltas_apply() {
    let mut b = synced();
    assert!(matches!(
        b.on_delta(&set(11, Side::Bid, 98, 1)),
        Outcome::DeltaApplied {
            recovered: false,
            ..
        }
    ));
    assert_eq!(b.last_seq(), 11);
    assert_eq!(b.stats().deltas_applied, 1);
}

#[test]
fn duplicates_are_ignored_and_counted() {
    let mut b = synced();
    b.on_delta(&set(11, Side::Bid, 98, 1));
    let sum = b.book().checksum(usize::MAX);
    assert_eq!(
        b.on_delta(&set(11, Side::Bid, 98, 555)),
        Outcome::Duplicate {
            seq: 11,
            last_seq: 11
        }
    );
    assert_eq!(
        b.on_delta(&set(3, Side::Bid, 98, 555)),
        Outcome::Duplicate {
            seq: 3,
            last_seq: 11
        }
    );
    assert_eq!(b.book().checksum(usize::MAX), sum);
    assert_eq!(b.stats().duplicates, 2);
    assert_eq!(b.state(), SyncState::Synced);
}

#[test]
fn gap_stops_trusting_the_book_and_buffers() {
    let mut b = synced();
    assert_eq!(
        b.on_delta(&set(13, Side::Bid, 98, 1)),
        Outcome::GapDetected {
            expected: 11,
            got: 13
        }
    );
    assert_eq!(b.state(), SyncState::Resyncing);
    assert_eq!(b.last_seq(), 10, "nothing past the gap may be applied");
    assert_eq!(b.book().get(Side::Bid, 98), None);
    assert_eq!(
        b.on_delta(&set(14, Side::Bid, 97, 1)),
        Outcome::Buffered { seq: 14 }
    );
    assert_eq!(b.stats().gaps, 1, "one gap, however many deltas follow");
}

#[test]
fn snapshot_covering_the_gap_recovers_and_replays_newer_deltas() {
    let mut b = synced();
    b.on_delta(&set(13, Side::Bid, 98, 1));
    b.on_delta(&set(14, Side::Bid, 97, 2));
    b.on_delta(&set(15, Side::Ask, 101, 0));
    let out = b.on_snapshot(&snap(13, &[(100, 10), (98, 1)], &[(101, 5)]));
    assert_eq!(
        out,
        Outcome::SnapshotApplied {
            seq: 13,
            drained: 2,
            synced: true,
            was: SyncState::Resyncing,
            drift: false
        }
    );
    assert_eq!(b.state(), SyncState::Synced);
    assert_eq!(b.last_seq(), 15);
    assert_eq!(
        b.book().top(Side::Bid, 5),
        vec![lv(100, 10), lv(98, 1), lv(97, 2)]
    );
    assert_eq!(b.book().best(Side::Ask), None);
    assert_eq!(b.stats().recoveries, 1);
}

#[test]
fn snapshot_older_than_the_buffered_deltas_does_not_fake_a_recovery() {
    let mut b = synced();
    b.on_delta(&set(15, Side::Bid, 98, 1)); // 11..=14 lost
    let out = b.on_snapshot(&snap(12, &[(100, 10)], &[(101, 5)]));
    assert!(matches!(
        out,
        Outcome::SnapshotApplied {
            seq: 12,
            drained: 0,
            synced: false,
            ..
        }
    ));
    assert_eq!(b.state(), SyncState::Resyncing);
    assert_eq!(b.buffered(), 1);
    // A later snapshot that reaches the buffered delta completes recovery.
    let out = b.on_snapshot(&snap(14, &[(100, 10)], &[(101, 5)]));
    assert!(matches!(
        out,
        Outcome::SnapshotApplied {
            synced: true,
            drained: 1,
            ..
        }
    ));
    assert_eq!(b.last_seq(), 15);
}

#[test]
fn out_of_order_delivery_heals_without_a_snapshot() {
    let mut b = synced();
    assert!(matches!(
        b.on_delta(&set(12, Side::Bid, 98, 1)),
        Outcome::GapDetected { .. }
    ));
    let out = b.on_delta(&set(11, Side::Bid, 97, 2));
    assert!(matches!(
        out,
        Outcome::DeltaApplied {
            drained: 1,
            recovered: true,
            ..
        }
    ));
    assert_eq!(b.state(), SyncState::Synced);
    assert_eq!(b.last_seq(), 12);
    assert_eq!(
        b.book().top(Side::Bid, 5),
        vec![lv(100, 10), lv(99, 20), lv(98, 1), lv(97, 2)]
    );
}

#[test]
fn partial_heal_keeps_resyncing() {
    let mut b = synced();
    b.on_delta(&set(12, Side::Bid, 98, 1));
    b.on_delta(&set(14, Side::Bid, 96, 1)); // 13 also missing
    let out = b.on_delta(&set(11, Side::Bid, 97, 2));
    assert!(matches!(
        out,
        Outcome::DeltaApplied {
            drained: 1,
            recovered: false,
            ..
        }
    ));
    assert_eq!(b.state(), SyncState::Resyncing);
    assert_eq!(b.last_seq(), 12);
}

#[test]
fn stale_snapshot_is_ignored() {
    let mut b = synced();
    b.on_delta(&set(11, Side::Bid, 98, 1));
    let sum = b.book().checksum(usize::MAX);
    assert_eq!(
        b.on_snapshot(&snap(9, &[(1, 1)], &[(2, 1)])),
        Outcome::StaleSnapshot {
            seq: 9,
            last_seq: 11
        }
    );
    assert_eq!(b.book().checksum(usize::MAX), sum);
    assert_eq!(b.stats().stale_snapshots, 1);
}

#[test]
fn same_sequence_snapshot_detects_silent_drift() {
    let mut b = synced();
    let same = snap(10, &[(99, 20), (100, 10)], &[(102, 7), (101, 5)]);
    assert!(matches!(
        b.on_snapshot(&same),
        Outcome::SnapshotApplied {
            drift: false,
            synced: true,
            ..
        }
    ));
    let different = snap(10, &[(100, 11), (99, 20)], &[(101, 5), (102, 7)]);
    assert!(matches!(
        b.on_snapshot(&different),
        Outcome::SnapshotApplied {
            drift: true,
            synced: true,
            ..
        }
    ));
    assert_eq!(
        b.book().get(Side::Bid, 100),
        Some(11),
        "the venue's image wins"
    );
    assert_eq!(b.stats().drift, 1);
}

#[test]
fn newer_snapshot_while_synced_jumps_forward() {
    let mut b = synced();
    let out = b.on_snapshot(&snap(20, &[(90, 1)], &[(95, 1)]));
    assert!(matches!(
        out,
        Outcome::SnapshotApplied {
            seq: 20,
            synced: true,
            drift: false,
            ..
        }
    ));
    assert_eq!(b.last_seq(), 20);
    assert!(matches!(
        b.on_delta(&set(21, Side::Bid, 91, 1)),
        Outcome::DeltaApplied { .. }
    ));
}

#[test]
fn invalid_in_sequence_delta_forces_a_resync() {
    let mut b = synced();
    let out = b.on_delta(&set(11, Side::Bid, 0, 1));
    assert_eq!(
        out,
        Outcome::Rejected {
            error: BookError::InvalidPrice { price: 0 }
        }
    );
    assert_eq!(
        b.state(),
        SyncState::Resyncing,
        "seq 11 was consumed by an unusable message"
    );
    assert_eq!(b.last_seq(), 10);
    assert_eq!(
        b.on_delta(&set(12, Side::Bid, 98, 1)),
        Outcome::Buffered { seq: 12 }
    );
    b.on_snapshot(&snap(12, &[(100, 10)], &[(101, 5)]));
    assert_eq!(b.state(), SyncState::Synced);
}

#[test]
fn invalid_snapshot_is_rejected_without_state_change() {
    let mut b = synced();
    let out = b.on_snapshot(&snap(11, &[(100, 0)], &[]));
    assert!(matches!(out, Outcome::Rejected { .. }));
    assert_eq!(b.state(), SyncState::Synced);
    assert_eq!(b.last_seq(), 10);
}

#[test]
fn reorder_buffer_is_bounded() {
    let mut b: SequencedBook = SequencedBook::with_max_buffer(SYM, 3);
    b.on_snapshot(&snap(10, &[(100, 10)], &[(101, 5)]));
    for seq in 12..20 {
        b.on_delta(&set(seq, Side::Bid, 90, seq));
    }
    assert_eq!(b.buffered(), 3);
    assert_eq!(b.stats().buffer_overflow, 5);
    // The snapshot is older than what survived in the buffer → still a hole.
    assert!(matches!(
        b.on_snapshot(&snap(12, &[(100, 10)], &[(101, 5)])),
        Outcome::SnapshotApplied { synced: false, .. }
    ));
    assert!(matches!(
        b.on_snapshot(&snap(16, &[(100, 10)], &[(101, 5)])),
        Outcome::SnapshotApplied { synced: true, .. }
    ));
    assert_eq!(b.last_seq(), 19);
}

#[test]
fn reset_returns_to_awaiting_snapshot() {
    let mut b = synced();
    b.on_delta(&set(11, Side::Bid, 98, 1));
    b.reset();
    assert_eq!(b.state(), SyncState::AwaitingSnapshot);
    assert_eq!(b.last_seq(), 0);
    assert_eq!(b.book().best(Side::Bid), None);
    // Sequence numbers may legitimately start again from 1.
    b.on_snapshot(&snap(0, &[(50, 1)], &[(51, 1)]));
    assert!(matches!(
        b.on_delta(&set(1, Side::Bid, 49, 1)),
        Outcome::DeltaApplied { .. }
    ));
}
