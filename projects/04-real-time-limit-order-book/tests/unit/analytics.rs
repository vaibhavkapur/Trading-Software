use analytics::{compute_metric, BestQuote, OfiTracker};
use lob_tests::{lv, SYM};

#[test]
fn metrics_on_a_known_book() {
    let bids = [lv(10_000, 300), lv(9_999, 100)];
    let asks = [lv(10_002, 100), lv(10_003, 500)];
    let m = compute_metric(SYM, 7, 42, &bids, &asks, 5);
    assert_eq!(m.spread_ticks, Some(2));
    assert_eq!(m.mid_ticks_x2, Some(20_002));
    assert_eq!(m.mid_px, Some(10_001.0));
    // (10000*100 + 10002*300) / 400 = 10001.5: pulled towards the thin ask.
    assert_eq!(m.microprice, Some(10_001.5));
    assert_eq!(m.imbalance, Some(0.5));
    assert_eq!((m.bid_depth, m.ask_depth), (400, 600));
    assert_eq!(m.depth_imbalance, Some(-0.2));
    assert_eq!((m.bid_levels, m.ask_levels), (2, 2));
    assert_eq!((m.ts, m.seq, m.ofi), (7, 42, 5));
}

#[test]
fn half_tick_mid_is_exact_in_the_integer_field() {
    let m = compute_metric(SYM, 0, 0, &[lv(100, 1)], &[lv(101, 1)], 0);
    assert_eq!(m.mid_ticks_x2, Some(201));
    assert_eq!(m.mid_px, Some(100.5));
}

#[test]
fn one_sided_and_empty_books_yield_no_two_sided_metrics() {
    let m = compute_metric(SYM, 0, 0, &[lv(100, 10)], &[], 0);
    assert_eq!(
        (m.spread_ticks, m.mid_px, m.microprice, m.imbalance),
        (None, None, None, None)
    );
    assert_eq!(m.depth_imbalance, Some(1.0));
    let e = compute_metric(SYM, 0, 0, &[], &[], 0);
    assert_eq!(e.depth_imbalance, None);
    assert_eq!((e.bid_depth, e.ask_depth), (0, 0));
}

#[test]
fn crossed_book_reports_a_negative_spread() {
    let m = compute_metric(SYM, 0, 0, &[lv(102, 1)], &[lv(100, 1)], 0);
    assert_eq!(m.spread_ticks, Some(-2));
}

#[test]
fn imbalance_stays_within_bounds_at_extremes() {
    let m = compute_metric(SYM, 0, 0, &[lv(100, u64::MAX)], &[lv(101, 1)], 0);
    let i = m.imbalance.unwrap();
    assert!((-1.0..=1.0).contains(&i) && i > 0.99);
    let mp = m.microprice.unwrap();
    assert!((100.0..=101.0).contains(&mp));
}

#[test]
fn best_quote_from_levels() {
    let q = BestQuote::from_levels(SYM, &[lv(100, 3)], &[], 9);
    assert_eq!(
        (q.bid_px, q.bid_qty, q.ask_px, q.ask_qty, q.asof),
        (Some(100), Some(3), None, None, 9)
    );
}

#[test]
fn order_flow_imbalance_cases() {
    let mut t = OfiTracker::default();
    t.update(Some(lv(100, 10)), Some(lv(101, 10)));
    assert_eq!(t.take(), 0, "first observation only sets the baseline");

    t.update(Some(lv(100, 15)), Some(lv(101, 10))); // bid size grows: +5
    assert_eq!(t.take(), 5);
    t.update(Some(lv(100, 15)), Some(lv(101, 4))); // ask size shrinks: +6
    assert_eq!(t.take(), 6);
    t.update(Some(lv(101, 7)), Some(lv(102, 9))); // bid up: +7; ask up: +4 (old ask gone)
    assert_eq!(t.take(), 11);
    t.update(Some(lv(100, 3)), Some(lv(101, 2))); // bid down: -7; ask down: -2
    assert_eq!(t.take(), -9);

    t.update(Some(lv(100, 4)), Some(lv(101, 2)));
    t.update(Some(lv(100, 6)), Some(lv(101, 2)));
    assert_eq!(t.take(), 3, "accumulates across updates within a window");

    t.update(None, Some(lv(101, 2))); // bid side emptied
    assert_eq!(t.take(), -6);

    t.reset();
    t.update(Some(lv(500, 1_000)), Some(lv(501, 1)));
    assert_eq!(t.take(), 0, "no flow is attributed across a resync");
}
