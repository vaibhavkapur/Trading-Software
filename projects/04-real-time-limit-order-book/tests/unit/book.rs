use book_core::{
    Action, BTreeLadder, BookError, DeltaEffect, Integrity, OrderBook, Side, VecLadder,
};
use lob_tests::{lv, set, snap, SYM};

fn book() -> OrderBook {
    let mut b = OrderBook::new(SYM);
    b.apply_snapshot(&snap(1, &[(100, 10), (99, 20)], &[(101, 5), (102, 7)]))
        .unwrap();
    b
}

#[test]
fn snapshot_orders_levels_best_first() {
    let mut b: OrderBook = OrderBook::new(SYM);
    // Deliberately unsorted input.
    b.apply_snapshot(&snap(
        1,
        &[(98, 1), (100, 2), (99, 3)],
        &[(103, 1), (101, 2), (102, 3)],
    ))
    .unwrap();
    assert_eq!(b.top(Side::Bid, 10), vec![lv(100, 2), lv(99, 3), lv(98, 1)]);
    assert_eq!(
        b.top(Side::Ask, 10),
        vec![lv(101, 2), lv(102, 3), lv(103, 1)]
    );
    assert_eq!(b.best(Side::Bid), Some(lv(100, 2)));
    assert_eq!(b.best(Side::Ask), Some(lv(101, 2)));
}

#[test]
fn delta_insert_update_remove() {
    let mut b = book();
    assert_eq!(
        b.apply_delta(&set(2, Side::Bid, 98, 4)),
        Ok(DeltaEffect::Inserted)
    );
    assert_eq!(
        b.apply_delta(&set(3, Side::Bid, 98, 9)),
        Ok(DeltaEffect::Updated { prev: 4 })
    );
    assert_eq!(
        b.apply_delta(&set(4, Side::Bid, 98, 0)),
        Ok(DeltaEffect::Removed { prev: 9 })
    );
    assert_eq!(b.get(Side::Bid, 98), None);
    assert_eq!(b.levels(Side::Bid), 2);
}

#[test]
fn set_with_zero_quantity_is_a_delete() {
    let mut b = book();
    let mut d = set(2, Side::Ask, 101, 0);
    d.action = Action::Set;
    assert_eq!(b.apply_delta(&d), Ok(DeltaEffect::Removed { prev: 5 }));
    assert_eq!(b.best(Side::Ask), Some(lv(102, 7)));
}

#[test]
fn explicit_delete_ignores_quantity_field() {
    let mut b = book();
    let mut d = set(2, Side::Ask, 101, 999);
    d.action = Action::Delete;
    assert_eq!(b.apply_delta(&d), Ok(DeltaEffect::Removed { prev: 5 }));
}

#[test]
fn deleting_a_missing_level_is_a_reported_noop() {
    let mut b = book();
    let before = b.checksum(usize::MAX);
    assert_eq!(
        b.apply_delta(&set(2, Side::Bid, 50, 0)),
        Ok(DeltaEffect::RemoveMissing)
    );
    assert_eq!(b.checksum(usize::MAX), before);
}

#[test]
fn non_positive_prices_are_rejected() {
    let mut b = book();
    for px in [0, -1, i64::MIN] {
        assert_eq!(
            b.apply_delta(&set(2, Side::Bid, px, 1)),
            Err(BookError::InvalidPrice { price: px })
        );
    }
}

#[test]
fn numeric_boundaries_are_representable() {
    let mut b = book();
    b.apply_delta(&set(2, Side::Ask, i64::MAX, u64::MAX))
        .unwrap();
    assert_eq!(b.get(Side::Ask, i64::MAX), Some(u64::MAX));
    b.apply_delta(&set(3, Side::Bid, 1, 1)).unwrap();
    assert_eq!(b.top(Side::Bid, 10).last(), Some(&lv(1, 1)));
}

#[test]
fn wrong_symbol_is_rejected() {
    let mut b = book();
    let mut d = set(2, Side::Bid, 100, 1);
    d.symbol = "OTHER".into();
    assert!(matches!(
        b.apply_delta(&d),
        Err(BookError::SymbolMismatch { .. })
    ));
}

#[test]
fn invalid_snapshot_leaves_book_untouched() {
    let mut b = book();
    let before = b.checksum(usize::MAX);
    let cases = [
        snap(9, &[(100, 0)], &[]),
        snap(9, &[(-5, 1)], &[]),
        snap(9, &[(100, 1), (100, 2)], &[]),
        snap(9, &[(100, 1)], &[(101, 1), (101, 1)]),
    ];
    for s in cases {
        assert!(b.apply_snapshot(&s).is_err(), "{s:?}");
        assert_eq!(b.checksum(usize::MAX), before);
    }
}

#[test]
fn snapshot_replaces_rather_than_merges() {
    let mut b = book();
    b.apply_snapshot(&snap(5, &[(90, 1)], &[(95, 1)])).unwrap();
    assert_eq!(b.top(Side::Bid, 10), vec![lv(90, 1)]);
    assert_eq!(b.top(Side::Ask, 10), vec![lv(95, 1)]);
}

#[test]
fn integrity_classification() {
    let mut b = book();
    assert_eq!(b.integrity(), Integrity::Ok);
    b.apply_delta(&set(2, Side::Bid, 101, 1)).unwrap();
    assert_eq!(b.integrity(), Integrity::Locked);
    b.apply_delta(&set(3, Side::Bid, 102, 1)).unwrap();
    assert_eq!(b.integrity(), Integrity::Crossed);
    b.apply_delta(&set(4, Side::Bid, 102, 0)).unwrap();
    b.apply_delta(&set(5, Side::Bid, 101, 0)).unwrap();
    assert_eq!(b.integrity(), Integrity::Ok);

    let mut one_sided: OrderBook = OrderBook::new(SYM);
    one_sided
        .apply_snapshot(&snap(1, &[(100, 1)], &[]))
        .unwrap();
    assert_eq!(one_sided.integrity(), Integrity::Ok);
}

#[test]
fn checksum_depends_on_depth_and_content_but_not_on_ladder() {
    let s = snap(
        1,
        &[(100, 10), (99, 20), (98, 30)],
        &[(101, 5), (102, 7), (103, 9)],
    );
    let mut a: OrderBook<BTreeLadder> = OrderBook::new(SYM);
    let mut v: OrderBook<VecLadder> = OrderBook::new(SYM);
    a.apply_snapshot(&s).unwrap();
    v.apply_snapshot(&s).unwrap();
    assert_eq!(a.checksum(2), v.checksum(2));
    assert_eq!(a.checksum(usize::MAX), v.checksum(usize::MAX));
    assert_ne!(a.checksum(1), a.checksum(2));

    // A change below the checksummed depth is invisible; one inside is not.
    let top2 = a.checksum(2);
    a.apply_delta(&set(2, Side::Bid, 98, 31)).unwrap();
    assert_eq!(a.checksum(2), top2);
    a.apply_delta(&set(3, Side::Bid, 99, 21)).unwrap();
    assert_ne!(a.checksum(2), top2);

    // Same price/qty on the other side must hash differently.
    let mut x: OrderBook = OrderBook::new(SYM);
    let mut y: OrderBook = OrderBook::new(SYM);
    x.apply_snapshot(&snap(1, &[(100, 1)], &[])).unwrap();
    y.apply_snapshot(&snap(1, &[], &[(100, 1)])).unwrap();
    assert_ne!(x.checksum(5), y.checksum(5));
}
