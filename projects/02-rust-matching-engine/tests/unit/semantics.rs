use super::*;
use Side::{Buy, Sell};

const S: &str = "XYZ";

#[test]
fn readme_example_fills_40_then_cancels_remaining_60() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 10025, 100));
    let ev = h.send(limit(2, S, Sell, 10025, 40));
    assert_eq!(trades(&ev), vec![(1, 2, 40, 10025)]);
    assert_eq!(h.top(S).bid, Some((10025, 60)));
    let ev = h.send(Command::Cancel { order_id: 1 });
    assert!(matches!(
        ev[0].kind,
        EventKind::Canceled {
            order_id: 1,
            canceled_qty: 60,
            reason: CancelReason::UserRequested,
            ..
        }
    ));
    assert_eq!(h.engine.open_orders(), 0);
}

#[test]
fn non_crossing_orders_rest_bid_descending_ask_ascending() {
    let mut h = Harness::new();
    for (id, px) in [(1, 99), (2, 101), (3, 100)] {
        h.send(limit(id, S, Buy, px, 10));
    }
    for (id, px) in [(4, 106), (5, 104), (6, 105)] {
        h.send(limit(id, S, Sell, px, 10));
    }
    let book = h.engine.book(sym(S)).unwrap();
    let px = |side| -> Vec<i64> { book.depth(side, 10).iter().map(|l| l.0).collect() };
    assert_eq!(px(Buy), vec![101, 100, 99]);
    assert_eq!(px(Sell), vec![104, 105, 106]);
    assert_eq!(
        h.top(S),
        Top {
            bid: Some((101, 10)),
            ask: Some((104, 10))
        }
    );
}

#[test]
fn better_price_trades_first() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 102, 10));
    h.send(limit(2, S, Sell, 101, 10));
    let ev = h.send(limit(3, S, Buy, 102, 15));
    assert_eq!(trades(&ev), vec![(2, 3, 10, 101), (1, 3, 5, 102)]);
}

#[test]
fn same_price_trades_in_arrival_order() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Buy, 100, 10));
    h.send(limit(3, S, Buy, 100, 10));
    let ev = h.send(limit(4, S, Sell, 100, 25));
    assert_eq!(
        trades(&ev),
        vec![(1, 4, 10, 100), (2, 4, 10, 100), (3, 4, 5, 100)]
    );
    assert_eq!(h.top(S).bid, Some((100, 5)));
}

#[test]
fn trade_price_is_the_makers_price() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 100, 10));
    let ev = h.send(limit(2, S, Buy, 105, 10));
    assert_eq!(trades(&ev), vec![(1, 2, 10, 100)]);
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 105, 10));
    let ev = h.send(limit(2, S, Sell, 100, 10));
    assert_eq!(trades(&ev), vec![(1, 2, 10, 105)]);
}

#[test]
fn aggressive_limit_rests_its_remainder() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 100, 10));
    let ev = h.send(limit(2, S, Buy, 100, 30));
    assert_eq!(trades(&ev), vec![(1, 2, 10, 100)]);
    assert_eq!(
        h.top(S),
        Top {
            bid: Some((100, 20)),
            ask: None
        }
    );
}

#[test]
fn each_trade_emits_trade_then_maker_then_taker_execution() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 100, 10));
    let ev = h.send(limit(2, S, Buy, 100, 4));
    let kinds: Vec<&EventKind> = ev.iter().map(|e| &e.kind).collect();
    assert!(matches!(kinds[0], EventKind::Accepted { order_id: 2, .. }));
    assert!(matches!(
        kinds[1],
        EventKind::Trade {
            trade_id: 1,
            qty: 4,
            ..
        }
    ));
    assert!(matches!(
        kinds[2],
        EventKind::Execution {
            order_id: 1,
            role: Role::Maker,
            side: Sell,
            qty: 4,
            leaves_qty: 6,
            trade_id: 1,
            ..
        }
    ));
    assert!(matches!(
        kinds[3],
        EventKind::Execution {
            order_id: 2,
            role: Role::Taker,
            side: Buy,
            qty: 4,
            leaves_qty: 0,
            trade_id: 1,
            ..
        }
    ));
    assert!(matches!(
        kinds[4],
        EventKind::BookTop {
            bid_px: None,
            ask_px: Some(100),
            ask_qty: 6,
            ..
        }
    ));
    assert_eq!(kinds.len(), 5);
}

#[test]
fn market_order_sweeps_levels_and_never_rests() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 100, 10));
    h.send(limit(2, S, Sell, 101, 10));
    let ev = h.send(market(3, S, Buy, 25));
    assert_eq!(trades(&ev), vec![(1, 3, 10, 100), (2, 3, 10, 101)]);
    assert!(ev.iter().any(|e| matches!(
        e.kind,
        EventKind::Canceled {
            order_id: 3,
            canceled_qty: 5,
            reason: CancelReason::NoLiquidity,
            ..
        }
    )));
    assert_eq!(h.engine.open_orders(), 0);
    assert_eq!(h.top(S), Top::default());
}

#[test]
fn market_order_into_empty_book_is_accepted_then_canceled() {
    let mut h = Harness::new();
    let ev = h.send(market(1, S, Sell, 7));
    assert_eq!(ev.len(), 2);
    assert!(matches!(
        ev[0].kind,
        EventKind::Accepted {
            ord_type: OrdType::Market,
            ..
        }
    ));
    assert!(matches!(
        ev[1].kind,
        EventKind::Canceled {
            canceled_qty: 7,
            reason: CancelReason::NoLiquidity,
            ..
        }
    ));
}

#[test]
fn cancel_removes_from_the_middle_of_a_queue() {
    let mut h = Harness::new();
    for id in 1..=3 {
        h.send(limit(id, S, Buy, 100, 10));
    }
    h.send(Command::Cancel { order_id: 2 });
    let ev = h.send(limit(4, S, Sell, 100, 20));
    assert_eq!(trades(&ev), vec![(1, 4, 10, 100), (3, 4, 10, 100)]);
}

#[test]
fn cancel_of_unknown_filled_or_already_canceled_order_is_rejected() {
    let mut h = Harness::new();
    assert_eq!(
        rejection(&h.send(Command::Cancel { order_id: 9 })),
        Some(RejectReason::UnknownOrder)
    );
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Sell, 100, 10)); // fills order 1 completely
    assert_eq!(
        rejection(&h.send(Command::Cancel { order_id: 1 })),
        Some(RejectReason::UnknownOrder)
    );
    h.send(limit(3, S, Buy, 100, 10));
    h.send(Command::Cancel { order_id: 3 });
    assert_eq!(
        rejection(&h.send(Command::Cancel { order_id: 3 })),
        Some(RejectReason::UnknownOrder)
    );
}

#[test]
fn duplicate_live_order_id_is_rejected_and_changes_nothing() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    let before = h.engine.snapshot().books;
    let ev = h.send(limit(1, S, Sell, 90, 10));
    assert_eq!(rejection(&ev), Some(RejectReason::DuplicateOrderId));
    assert_eq!(h.engine.snapshot().books, before);
    // Same id on another symbol is still a duplicate: ids are engine-wide.
    let ev = h.send(limit(1, "ABC", Buy, 100, 10));
    assert_eq!(rejection(&ev), Some(RejectReason::DuplicateOrderId));
}

#[test]
fn validation_rejects() {
    let mut h = Harness::new();
    let cases = [
        (limit(1, S, Buy, 100, 0), RejectReason::InvalidQty),
        (limit(1, S, Buy, 100, MAX_QTY + 1), RejectReason::InvalidQty),
        (limit(1, S, Buy, 0, 10), RejectReason::InvalidPrice),
        (limit(1, S, Buy, -5, 10), RejectReason::InvalidPrice),
        (
            limit(1, S, Buy, MAX_PRICE_TICKS + 1, 10),
            RejectReason::InvalidPrice,
        ),
        (market(1, S, Buy, 0), RejectReason::InvalidQty),
    ];
    for (cmd, reason) in cases {
        assert_eq!(rejection(&h.send(cmd)), Some(reason), "{cmd:?}");
    }
    let mut no_price = limit(1, S, Buy, 100, 10);
    let mut priced_market = market(1, S, Buy, 10);
    if let (Command::New(a), Command::New(b)) = (&mut no_price, &mut priced_market) {
        a.price_ticks = None;
        b.price_ticks = Some(100);
    }
    assert_eq!(
        rejection(&h.send(no_price)),
        Some(RejectReason::MissingPrice)
    );
    assert_eq!(
        rejection(&h.send(priced_market)),
        Some(RejectReason::UnexpectedPrice)
    );
    assert_eq!(h.engine.open_orders(), 0);
    assert!(
        h.engine.symbols().is_empty(),
        "rejects must not create books"
    );
}

#[test]
fn numeric_boundaries_are_accepted_and_aggregate_without_overflow() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, MAX_PRICE_TICKS, MAX_QTY));
    h.send(limit(2, S, Sell, MAX_PRICE_TICKS, MAX_QTY));
    h.send(limit(3, S, Buy, 1, MAX_QTY));
    assert_eq!(
        h.top(S),
        Top {
            bid: Some((1, MAX_QTY)),
            ask: Some((MAX_PRICE_TICKS, 2 * MAX_QTY))
        }
    );
    let ev = h.send(market(4, S, Buy, MAX_QTY));
    assert_eq!(trades(&ev), vec![(1, 4, MAX_QTY, MAX_PRICE_TICKS)]);
}

#[test]
fn replace_down_at_same_price_keeps_priority() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Buy, 100, 10));
    let ev = h.send(Command::Replace {
        order_id: 1,
        qty: 4,
        price_ticks: 100,
    });
    assert!(matches!(
        ev[0].kind,
        EventKind::Replaced {
            order_id: 1,
            qty: 4,
            priority_retained: true,
            ..
        }
    ));
    assert_eq!(h.top(S).bid, Some((100, 14)));
    let ev = h.send(limit(3, S, Sell, 100, 6));
    assert_eq!(trades(&ev), vec![(1, 3, 4, 100), (2, 3, 2, 100)]);
}

#[test]
fn replace_up_or_to_new_price_loses_priority() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Buy, 100, 10));
    let ev = h.send(Command::Replace {
        order_id: 1,
        qty: 11,
        price_ticks: 100,
    });
    assert!(matches!(
        ev[0].kind,
        EventKind::Replaced {
            priority_retained: false,
            ..
        }
    ));
    let ev = h.send(limit(3, S, Sell, 100, 12));
    assert_eq!(trades(&ev), vec![(2, 3, 10, 100), (1, 3, 2, 100)]);

    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Buy, 101, 10));
    h.send(Command::Replace {
        order_id: 1,
        qty: 10,
        price_ticks: 101,
    });
    let ev = h.send(limit(3, S, Sell, 101, 15));
    assert_eq!(trades(&ev), vec![(2, 3, 10, 101), (1, 3, 5, 101)]);
}

#[test]
fn replace_to_a_crossing_price_trades_immediately() {
    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 105, 10));
    h.send(limit(2, S, Buy, 100, 15));
    let ev = h.send(Command::Replace {
        order_id: 2,
        qty: 15,
        price_ticks: 105,
    });
    assert_eq!(trades(&ev), vec![(1, 2, 10, 105)]);
    assert_eq!(
        h.top(S),
        Top {
            bid: Some((105, 5)),
            ask: None
        }
    );
    // The id survives the replace and can still be canceled.
    let ev = h.send(Command::Cancel { order_id: 2 });
    assert!(matches!(
        ev[0].kind,
        EventKind::Canceled {
            canceled_qty: 5,
            ..
        }
    ));
}

#[test]
fn replace_rejects() {
    let mut h = Harness::new();
    let rep = |order_id, qty, price_ticks| Command::Replace {
        order_id,
        qty,
        price_ticks,
    };
    assert_eq!(
        rejection(&h.send(rep(1, 5, 100))),
        Some(RejectReason::UnknownOrder)
    );
    h.send(limit(1, S, Buy, 100, 10));
    assert_eq!(
        rejection(&h.send(rep(1, 0, 100))),
        Some(RejectReason::InvalidQty)
    );
    assert_eq!(
        rejection(&h.send(rep(1, 5, 0))),
        Some(RejectReason::InvalidPrice)
    );
    assert_eq!(
        h.top(S).bid,
        Some((100, 10)),
        "rejected replace leaves order intact"
    );
}

#[test]
fn order_id_may_be_reused_once_the_order_is_gone() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(Command::Cancel { order_id: 1 });
    let ev = h.send(limit(1, S, Buy, 101, 5));
    assert!(matches!(
        ev[0].kind,
        EventKind::Accepted { order_id: 1, .. }
    ));
}

#[test]
fn symbols_are_isolated() {
    let mut h = Harness::new();
    h.send(limit(1, "AAA", Sell, 100, 10));
    let ev = h.send(limit(2, "BBB", Buy, 100, 10));
    assert!(trades(&ev).is_empty());
    assert_eq!(h.top("AAA").ask, Some((100, 10)));
    assert_eq!(h.top("BBB").bid, Some((100, 10)));
}

#[test]
fn book_top_is_emitted_only_when_the_top_changes() {
    let mut h = Harness::new();
    let has_top = |ev: &[Event]| {
        ev.iter()
            .any(|e| matches!(e.kind, EventKind::BookTop { .. }))
    };
    assert!(has_top(&h.send(limit(1, S, Buy, 100, 10))));
    assert!(
        !has_top(&h.send(limit(2, S, Buy, 99, 10))),
        "behind the top"
    );
    assert!(
        has_top(&h.send(limit(3, S, Buy, 100, 5))),
        "top qty changed"
    );
    assert!(!has_top(&h.send(Command::Cancel { order_id: 2 })));
    assert!(!has_top(&h.send(Command::Cancel { order_id: 2 })), "reject");
    assert!(has_top(&h.send(Command::Cancel { order_id: 1 })));
}

#[test]
fn event_and_trade_ids_are_gap_free_and_carry_command_metadata() {
    let mut h = Harness::new();
    let mut all = Vec::new();
    all.extend(h.send(limit(1, S, Sell, 100, 5)));
    all.extend(h.send(limit(2, S, Sell, 100, 5)));
    all.extend(h.send(limit(3, S, Buy, 100, 10)));
    all.extend(h.send(Command::Cancel { order_id: 77 }));
    for (i, e) in all.iter().enumerate() {
        assert_eq!(e.seq, i as u64 + 1);
        assert_eq!(e.ts_ns, e.cmd_seq);
    }
    let ids: Vec<u64> = all
        .iter()
        .filter_map(|e| match e.kind {
            EventKind::Trade { trade_id, .. } => Some(trade_id),
            _ => None,
        })
        .collect();
    assert_eq!(ids, vec![1, 2]);
    assert_eq!(all.last().unwrap().cmd_seq, 4);
}

#[test]
fn out_of_order_sequence_numbers_are_refused_without_side_effects() {
    let mut engine = Engine::new();
    let mut out = Vec::new();
    let at = |seq| SequencedCommand {
        seq,
        ts_ns: seq,
        cmd: limit(seq, S, Buy, 100, 1),
    };
    assert_eq!(
        engine.process(&at(2), &mut out),
        Err(SequenceError {
            expected: 1,
            got: 2
        })
    );
    engine.process(&at(1), &mut out).unwrap();
    let before = engine.snapshot();
    assert_eq!(
        engine.process(&at(1), &mut out),
        Err(SequenceError {
            expected: 2,
            got: 1
        })
    );
    assert_eq!(
        engine.process(&at(3), &mut out),
        Err(SequenceError {
            expected: 2,
            got: 3
        })
    );
    assert_eq!(engine.snapshot(), before);
    assert_eq!(
        out.len(),
        2,
        "accepted + book top from the one valid command"
    );
}

#[test]
fn symbol_validation() {
    for ok in ["A", "XYZ", "BRK.B", "ES-Z6", "ABCDEFGH"] {
        assert_eq!(Symbol::new(ok).unwrap().as_str(), ok);
    }
    for bad in ["", "ABCDEFGHI", "xyz", "A B", "É"] {
        assert!(Symbol::new(bad).is_err(), "{bad:?}");
    }
    assert!(sym("AB") < sym("ABC") && sym("ABC") < sym("B"));
}

#[test]
fn wire_format_matches_the_documented_protocol() {
    let cmd: Command = serde_json::from_str(
        r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":100,"price_ticks":10025}"#,
    )
    .unwrap();
    assert_eq!(cmd, limit(1, S, Buy, 10025, 100));
    let cmd: Command = serde_json::from_str(
        r#"{"type":"NEW","order_id":2,"symbol":"XYZ","side":"SELL","ord_type":"MARKET","qty":5}"#,
    )
    .unwrap();
    assert_eq!(cmd, market(2, S, Sell, 5));
    let cmd: Command = serde_json::from_str(r#"{"type":"CANCEL","order_id":1}"#).unwrap();
    assert_eq!(cmd, Command::Cancel { order_id: 1 });
    let cmd: Command =
        serde_json::from_str(r#"{"type":"REPLACE","order_id":1,"qty":3,"price_ticks":9}"#).unwrap();
    assert_eq!(
        cmd,
        Command::Replace {
            order_id: 1,
            qty: 3,
            price_ticks: 9
        }
    );

    for bad in [
        r#"{"type":"NEW","order_id":1,"symbol":"toolongsymbol","side":"BUY","qty":1}"#,
        r#"{"type":"NEW","order_id":-1,"symbol":"XYZ","side":"BUY","qty":1}"#,
        r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"BUY","qty":1.5}"#,
        r#"{"type":"NEW","order_id":1,"symbol":"XYZ","side":"HOLD","qty":1}"#,
        r#"{"type":"NOPE"}"#,
    ] {
        assert!(serde_json::from_str::<Command>(bad).is_err(), "{bad}");
    }

    let mut h = Harness::new();
    h.send(limit(1, S, Sell, 10025, 40));
    let ev = h.send(limit(2, S, Buy, 10025, 40));
    assert_eq!(
        serde_json::to_string(&ev[1]).unwrap(),
        r#"{"seq":4,"cmd_seq":2,"ts_ns":2,"type":"TRADE","trade_id":1,"symbol":"XYZ","price_ticks":10025,"qty":40,"maker_order_id":1,"taker_order_id":2,"taker_side":"BUY"}"#
    );
    for e in &ev {
        let json = serde_json::to_string(e).unwrap();
        assert_eq!(&serde_json::from_str::<Event>(&json).unwrap(), e);
    }
}

#[test]
fn snapshot_round_trips_and_preserves_fifo() {
    let mut h = Harness::new();
    for id in 1..=3 {
        h.send(limit(id, S, Buy, 100, 10));
    }
    h.send(limit(4, S, Sell, 100, 3)); // partial fill of order 1
    h.send(limit(5, "ABC", Sell, 50, 1));
    let snap = h.engine.snapshot();
    let json = serde_json::to_string(&snap).unwrap();
    let restored = Engine::from_snapshot(&serde_json::from_str(&json).unwrap()).unwrap();
    assert_eq!(restored.snapshot(), snap);

    let mut a = h.engine;
    let mut b = restored;
    let next = SequencedCommand {
        seq: 6,
        ts_ns: 6,
        cmd: limit(6, S, Sell, 100, 20),
    };
    let (mut ea, mut eb) = (Vec::new(), Vec::new());
    a.process(&next, &mut ea).unwrap();
    b.process(&next, &mut eb).unwrap();
    assert_eq!(ea, eb);
    assert_eq!(
        trades(&ea),
        vec![(1, 6, 7, 100), (2, 6, 10, 100), (3, 6, 3, 100)]
    );
}

#[test]
fn invalid_snapshots_are_refused() {
    let mut h = Harness::new();
    h.send(limit(1, S, Buy, 100, 10));
    h.send(limit(2, S, Sell, 101, 10));
    let good = h.engine.snapshot();

    let mut crossed = good.clone();
    crossed.books[0].asks[0].price_ticks = 100;
    assert!(Engine::from_snapshot(&crossed)
        .unwrap_err()
        .contains("crossed"));

    let mut dup = good.clone();
    dup.books[0].asks[0].order_id = 1;
    assert!(Engine::from_snapshot(&dup).is_err());

    let mut zero = good.clone();
    zero.books[0].bids[0].remaining_qty = 0;
    assert!(Engine::from_snapshot(&zero).is_err());

    let mut version = good;
    version.version = 99;
    assert!(Engine::from_snapshot(&version).is_err());
}
