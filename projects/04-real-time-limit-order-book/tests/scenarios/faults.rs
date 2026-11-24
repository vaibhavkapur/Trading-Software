//! Failure scenarios driven through the whole decode → sequence → book path,
//! scored against the simulator's ground truth.

use book_core::{Integrity, Side, SyncState};
use feed_adapter::{Fault, SimSession};
use lob_tests::{
    count, engine_for, kinds, pump, raw_delta, raw_snapshot, sim_config, single_symbol_engine, SYM,
};
use stream_api::engine::{Engine, EngineConfig, Output, StatusKind};

/// The reconstructed book equals the venue's true book.
fn assert_matches_truth(session: &SimSession, engine: &Engine) {
    let (seq, truth) = session.truth(SYM).unwrap();
    let view = engine.view(SYM, usize::MAX, 0).unwrap();
    assert_eq!(view.state, SyncState::Synced);
    assert_eq!(view.seq, seq);
    assert_eq!(view.bids, truth.top(Side::Bid, usize::MAX));
    assert_eq!(view.asks, truth.top(Side::Ask, usize::MAX));
}

fn started(seed: u64) -> (SimSession, Engine) {
    let mut session = SimSession::new(sim_config(seed));
    let mut engine = engine_for(&session);
    pump(&mut session, &mut engine, 500);
    assert_matches_truth(&session, &engine);
    (session, engine)
}

#[test]
fn clean_feed_tracks_truth_exactly() {
    let (mut session, mut engine) = started(1);
    let outs = pump(&mut session, &mut engine, 20_000);
    assert_matches_truth(&session, &engine);
    assert!(
        kinds(&outs).is_empty(),
        "a clean feed raises no events: {:?}",
        kinds(&outs)
    );
    let h = engine.health(SYM, 0).unwrap();
    assert_eq!(
        (h.gap_count, h.duplicate_count, h.rejected_count),
        (0, 0, 0)
    );
}

#[test]
fn dropped_deltas_trigger_resync_and_recover() {
    let (mut session, mut engine) = started(2);
    session.inject(Fault::Drop { count: 4 });

    // First frame after the loss exposes the gap.
    let outs = pump(&mut session, &mut engine, 1);
    assert_eq!(
        kinds(&outs),
        [StatusKind::GapDetected, StatusKind::ResyncRequested]
    );
    assert!(outs
        .iter()
        .any(|o| matches!(o, Output::ResyncRequest { symbol } if symbol == SYM)));
    assert!(
        !outs.iter().any(|o| matches!(o, Output::Delta(_))),
        "nothing past a gap is published"
    );

    // While resyncing: flagged as such, and no metrics are produced.
    let view = engine.view(SYM, 10, 0).unwrap();
    assert_eq!(view.state, SyncState::Resyncing);
    assert!(engine.take_metric(SYM).is_none());
    let seq_at_gap = view.seq;
    pump(&mut session, &mut engine, 5);
    assert_eq!(
        engine.view(SYM, 10, 0).unwrap().seq,
        seq_at_gap,
        "book frozen during resync"
    );

    let outs = pump(&mut session, &mut engine, 100);
    assert_eq!(count(&outs, StatusKind::Resynced), 1);
    // Recovery publishes a new base image for subscribers.
    assert!(outs
        .iter()
        .any(|o| matches!(o, Output::Snapshot(v) if v.state == SyncState::Synced)));
    assert_matches_truth(&session, &engine);
    let h = engine.health(SYM, 0).unwrap();
    assert_eq!((h.gap_count, h.recovery_count), (1, 1));
    assert!(h.last_recovery_ms.is_some());
    assert!(engine.take_metric(SYM).is_some());
}

#[test]
fn duplicate_delivery_is_ignored() {
    let (mut session, mut engine) = started(3);
    session.inject(Fault::Duplicate);
    let outs = pump(&mut session, &mut engine, 200);
    assert_eq!(kinds(&outs), [StatusKind::Duplicate]);
    assert_matches_truth(&session, &engine);
    assert_eq!(engine.health(SYM, 0).unwrap().duplicate_count, 1);
}

#[test]
fn out_of_order_delivery_heals_from_the_reorder_buffer() {
    let (mut session, mut engine) = started(4);
    session.inject(Fault::Reorder);
    let outs = pump(&mut session, &mut engine, 2);
    assert_eq!(
        kinds(&outs),
        [
            StatusKind::GapDetected,
            StatusKind::ResyncRequested,
            StatusKind::Resynced
        ]
    );
    assert_matches_truth(&session, &engine);
    // The snapshot that was requested anyway arrives later and agrees.
    let outs = pump(&mut session, &mut engine, 200);
    assert_eq!(count(&outs, StatusKind::DriftDetected), 0);
    assert_matches_truth(&session, &engine);
}

#[test]
fn corrupt_frame_is_counted_and_recovered_from() {
    let (mut session, mut engine) = started(5);
    session.inject(Fault::Garbage);
    let outs = pump(&mut session, &mut engine, 200);
    let k = kinds(&outs);
    assert_eq!(k[0], StatusKind::DecodeError);
    assert_eq!(
        k[1],
        StatusKind::GapDetected,
        "the corrupt frame consumed a sequence number"
    );
    assert_eq!(count(&outs, StatusKind::Resynced), 1);
    assert_eq!(engine.decode_errors(), 1);
    assert_matches_truth(&session, &engine);
}

#[test]
fn crossed_book_is_always_flagged() {
    let (mut session, mut engine) = started(6);
    session.inject(Fault::Cross);
    let mut saw_crossed = false;
    let mut events = Vec::new();
    for _ in 0..200 {
        events.extend(kinds(&pump(&mut session, &mut engine, 1)));
        let view = engine.view(SYM, 5, 0).unwrap();
        let (bid, ask) = (view.bids[0].price_ticks, view.asks[0].price_ticks);
        // The published flag must agree with the published levels, always.
        assert_eq!(view.integrity == Integrity::Crossed, bid > ask);
        assert_eq!(view.integrity == Integrity::Locked, bid == ask);
        if bid > ask {
            saw_crossed = true;
            assert_eq!(engine.health(SYM, 0).unwrap().integrity, Integrity::Crossed);
            assert!(engine.take_metric(SYM).unwrap().spread_ticks.unwrap() < 0);
        }
    }
    assert!(saw_crossed);
    assert!(events.contains(&StatusKind::Crossed));
    assert!(events.contains(&StatusKind::Uncrossed));
    assert!(engine.health(SYM, 0).unwrap().crossed_events >= 1);
    assert_matches_truth(&session, &engine);
}

#[test]
fn many_mixed_faults_never_leave_the_book_wrong() {
    for seed in 0..8 {
        let mut session = SimSession::new(sim_config(100 + seed));
        let mut engine = engine_for(&session);
        let mut rng = feed_adapter::rng::Rng::new(seed);
        let mut compared = 0;
        for step in 0..30_000 {
            if step % 400 == 399 {
                session.inject(match rng.below(5) {
                    0 => Fault::Drop {
                        count: 1 + rng.below(20) as u32,
                    },
                    1 => Fault::Duplicate,
                    2 => Fault::Reorder,
                    3 => Fault::Garbage,
                    _ => Fault::Cross,
                });
            }
            pump(&mut session, &mut engine, 1);
            let (truth_seq, truth) = session.truth(SYM).unwrap();
            let view = engine.view(SYM, usize::MAX, 0).unwrap();
            // Whenever the engine claims to be synced at the venue's sequence
            // number, the two books must be identical.
            if view.state == SyncState::Synced && view.seq == truth_seq {
                compared += 1;
                assert_eq!(
                    view.bids,
                    truth.top(Side::Bid, usize::MAX),
                    "seed {seed} step {step}"
                );
                assert_eq!(
                    view.asks,
                    truth.top(Side::Ask, usize::MAX),
                    "seed {seed} step {step}"
                );
            }
        }
        pump(&mut session, &mut engine, 500);
        assert_matches_truth(&session, &engine);
        assert!(
            compared > 20_000,
            "seed {seed}: only {compared} comparable steps"
        );
        assert_eq!(engine.seq_stats(SYM).unwrap().drift, 0, "seed {seed}");
    }
}

// --- time-based scenarios use hand-written frames and an explicit clock ----

const T0: i64 = 1_767_225_600_000_000;
const MS: i64 = 1_000;

fn timed_engine() -> Engine {
    let mut e = single_symbol_engine(EngineConfig {
        stale_after_ms: 2_000,
        resync_retry_ms: 1_000,
        skew_tolerance_ms: 1_000,
        ..EngineConfig::default()
    });
    let snap = raw_snapshot(
        10,
        T0,
        &[("100.00", "10"), ("99.99", "20")],
        &[("100.01", "5"), ("100.02", "7")],
    );
    e.on_frame(&snap, T0);
    e
}

#[test]
fn stale_feed_is_raised_once_and_cleared_on_data() {
    let mut e = timed_engine();
    assert!(e.tick(T0 + 1_999 * MS).is_empty());
    assert!(!e.view(SYM, 5, T0 + 1_999 * MS).unwrap().stale);

    let outs = e.tick(T0 + 2_001 * MS);
    assert_eq!(kinds(&outs), [StatusKind::Stale]);
    assert!(
        e.tick(T0 + 5_000 * MS).is_empty(),
        "raised once, not on every tick"
    );

    // Every read path reports it; the book is still served but marked.
    let view = e.view(SYM, 5, T0 + 5_000 * MS).unwrap();
    assert!(view.stale);
    assert_eq!(view.state, SyncState::Synced);
    let h = e.health(SYM, T0 + 5_000 * MS).unwrap();
    assert!(h.stale);
    assert_eq!(h.stale_ms, 5_000);

    let outs = e.on_frame(
        &raw_delta(11, T0 + 6_000 * MS, "buy", "99.98", "3"),
        T0 + 6_000 * MS,
    );
    assert_eq!(kinds(&outs), [StatusKind::Fresh]);
    assert!(!e.view(SYM, 5, T0 + 6_000 * MS).unwrap().stale);
}

#[test]
fn a_feed_that_never_delivers_is_stale_not_silently_empty() {
    let mut e = single_symbol_engine(EngineConfig::default());
    assert!(e.tick(T0).is_empty());
    let outs = e.tick(T0 + 2_500 * MS);
    assert!(kinds(&outs).contains(&StatusKind::Stale));
    assert!(outs
        .iter()
        .any(|o| matches!(o, Output::ResyncRequest { .. })));
    let view = e.view(SYM, 5, T0 + 2_500 * MS).unwrap();
    assert!(view.stale);
    assert_eq!(view.state, SyncState::AwaitingSnapshot);
}

#[test]
fn connection_loss_retries_the_snapshot_request_until_answered() {
    let mut e = timed_engine();
    // Reconnect after an outage: sequence jumped from 10 to 500.
    let outs = e.on_frame(
        &raw_delta(500, T0 + 100 * MS, "buy", "99.98", "3"),
        T0 + 100 * MS,
    );
    assert_eq!(
        kinds(&outs),
        [StatusKind::GapDetected, StatusKind::ResyncRequested]
    );

    let requests = |outs: &[Output]| {
        outs.iter()
            .filter(|o| matches!(o, Output::ResyncRequest { .. }))
            .count()
    };
    assert_eq!(
        requests(&e.tick(T0 + 600 * MS)),
        0,
        "not before the retry interval"
    );
    assert_eq!(requests(&e.tick(T0 + 1_100 * MS)), 1);
    assert_eq!(requests(&e.tick(T0 + 1_200 * MS)), 0);
    assert_eq!(requests(&e.tick(T0 + 2_100 * MS)), 1);
    assert_eq!(
        e.health(SYM, T0 + 2_100 * MS).unwrap().unsynced_for_ms,
        Some(2_000)
    );

    let snap = raw_snapshot(
        500,
        T0 + 2_300 * MS,
        &[("100.00", "10"), ("99.98", "3")],
        &[("100.01", "5")],
    );
    let outs = e.on_frame(&snap, T0 + 2_300 * MS);
    assert_eq!(kinds(&outs), [StatusKind::Resynced]);
    let h = e.health(SYM, T0 + 2_300 * MS).unwrap();
    assert_eq!(h.state, SyncState::Synced);
    assert_eq!(
        h.last_recovery_ms,
        Some(2_200),
        "gap at +100ms, synced at +2300ms"
    );
    assert_eq!(requests(&e.tick(T0 + 9_000 * MS)), 0);
}

#[test]
fn stale_snapshot_during_resync_does_not_end_the_resync() {
    let mut e = timed_engine();
    e.on_frame(&raw_delta(11, T0, "buy", "99.98", "3"), T0);
    e.on_frame(&raw_delta(13, T0, "buy", "99.97", "3"), T0); // 12 lost
    let old = raw_snapshot(10, T0, &[("100.00", "10")], &[("100.01", "5")]);
    let outs = e.on_frame(&old, T0 + MS);
    assert_eq!(kinds(&outs), [StatusKind::StaleSnapshot]);
    assert_eq!(e.view(SYM, 5, T0).unwrap().state, SyncState::Resyncing);
}

#[test]
fn venue_clock_ahead_of_local_clock_is_flagged_not_trusted() {
    let mut e = timed_engine();
    let recv = T0 + 100 * MS;
    let outs = e.on_frame(&raw_delta(11, recv + 5_000 * MS, "buy", "99.98", "3"), recv);
    assert_eq!(kinds(&outs), [StatusKind::ClockSkew]);
    let h = e.health(SYM, recv).unwrap();
    assert!(h.clock_skew_suspected);
    assert_eq!(h.venue_lag_ms, Some(-5_000));
    // Freshness is judged on the local clock only, so skew cannot fake it.
    assert!(e.view(SYM, 5, recv + 3_000 * MS).unwrap().stale);

    let outs = e.on_frame(
        &raw_delta(12, recv + 200 * MS, "buy", "99.97", "3"),
        recv + 200 * MS,
    );
    assert_eq!(kinds(&outs), [StatusKind::ClockSkewCleared]);
    assert!(!e.health(SYM, recv + 200 * MS).unwrap().clock_skew_suspected);
}

#[test]
fn old_venue_timestamps_do_not_make_fresh_data_look_stale_or_vice_versa() {
    let mut e = timed_engine();
    // Event stamped an hour ago by the venue but received just now.
    let recv = T0 + 500 * MS;
    e.on_frame(
        &raw_delta(11, T0 - 3_600_000 * MS, "buy", "99.98", "3"),
        recv,
    );
    let h = e.health(SYM, recv + 10 * MS).unwrap();
    assert!(!h.stale);
    assert_eq!(h.venue_lag_ms, Some(3_600_500));
}

#[test]
fn invalid_snapshot_is_rejected_and_counted() {
    let mut e = timed_engine();
    // Valid on the wire, but a snapshot with a duplicated level is invalid.
    let bad = raw_snapshot(11, T0, &[("100.00", "10"), ("100.00", "11")], &[]);
    assert_eq!(kinds(&e.on_frame(&bad, T0)), [StatusKind::Rejected]);
    assert_eq!(
        e.view(SYM, 5, T0).unwrap().state,
        SyncState::Synced,
        "bad snapshot is discarded"
    );
    assert_eq!(e.health(SYM, T0).unwrap().rejected_count, 1);
}

#[test]
fn source_restart_discards_the_old_book() {
    let mut e = timed_engine();
    let outs = e.reset(T0 + MS);
    assert_eq!(kinds(&outs), [StatusKind::SourceRestarted]);
    let view = e.view(SYM, 5, T0 + MS).unwrap();
    assert_eq!(view.state, SyncState::AwaitingSnapshot);
    assert!(view.bids.is_empty() && view.asks.is_empty());
    // Sequence numbers restart from a low value and are accepted again.
    let snap = raw_snapshot(0, T0 + 2 * MS, &[("50.00", "1")], &[("50.01", "1")]);
    e.on_frame(&snap, T0 + 2 * MS);
    let outs = e.on_frame(&raw_delta(1, T0 + 3 * MS, "buy", "49.99", "2"), T0 + 3 * MS);
    assert!(outs
        .iter()
        .any(|o| matches!(o, Output::Delta(d) if d.seq == 1)));
}
