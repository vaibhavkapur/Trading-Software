use book_core::{Action, FeedEvent, Side};
use feed_adapter::decimal::{format_units, to_units, DecimalError};
use feed_adapter::{parse_fault_plan, DecodeError, Fault, Normalizer, SimSymbol};
use lob_tests::{lv, raw_delta, raw_snapshot, SYM};

fn normalizer() -> Normalizer {
    Normalizer::new([SimSymbol::new(SYM, 10_000).instrument()])
}

#[test]
fn decimal_to_units_is_exact() {
    assert_eq!(to_units("100.25", "0.01"), Ok(10_025));
    assert_eq!(to_units("100.2", "0.01"), Ok(10_020));
    assert_eq!(to_units("100", "0.01"), Ok(10_000));
    assert_eq!(
        to_units("0.07", "0.01"),
        Ok(7),
        "0.07 is not exact in binary floating point"
    );
    assert_eq!(to_units("1.10", "0.05"), Ok(22));
    assert_eq!(to_units("0.00000001", "0.00000001"), Ok(1));
    assert_eq!(to_units("250", "25"), Ok(10));
    assert_eq!(to_units("0", "1"), Ok(0));
    assert_eq!(to_units(".5", "0.5"), Ok(1));
    assert_eq!(to_units("9223372036854775807", "1"), Ok(i64::MAX));
}

#[test]
fn decimal_rejects_what_it_cannot_represent() {
    assert!(matches!(
        to_units("100.255", "0.01"),
        Err(DecimalError::NotMultiple { .. })
    ));
    assert!(matches!(
        to_units("1.12", "0.05"),
        Err(DecimalError::NotMultiple { .. })
    ));
    assert!(matches!(
        to_units("9223372036854775808", "1"),
        Err(DecimalError::OutOfRange(_))
    ));
    assert!(matches!(to_units("1", "0"), Err(DecimalError::ZeroUnit(_))));
    for bad in [
        "", ".", "-1", "+1", "1e5", "1,000", "abc", "1.2.3", " 1", "NaN",
    ] {
        assert!(
            matches!(to_units(bad, "0.01"), Err(DecimalError::Malformed(_))),
            "{bad:?}"
        );
    }
    assert!(to_units(&"9".repeat(40), "1").is_err());
}

#[test]
fn format_units_round_trips() {
    assert_eq!(format_units(10_025, "0.01").unwrap(), "100.25");
    assert_eq!(format_units(7, "0.01").unwrap(), "0.07");
    assert_eq!(format_units(22, "0.05").unwrap(), "1.10");
    assert_eq!(format_units(10, "25").unwrap(), "250");
    for ticks in [1, 99, 100, 12_345, 1_000_000_007] {
        for unit in ["0.01", "0.05", "0.0001", "1", "5"] {
            assert_eq!(
                to_units(&format_units(ticks, unit).unwrap(), unit),
                Ok(ticks)
            );
        }
    }
}

#[test]
fn delta_is_normalized_to_ticks_lots_and_sides() {
    let n = normalizer();
    let ev = n
        .decode(&raw_delta(10_204, 777, "buy", "100.25", "350"))
        .unwrap()
        .unwrap();
    let FeedEvent::Delta(d) = &ev else {
        panic!("expected delta")
    };
    assert_eq!(
        (d.seq, d.side, d.price_ticks, d.qty, d.action, d.event_ts),
        (10_204, Side::Bid, 10_025, 350, Action::Set, 777)
    );

    // The normalized form matches the protocol sketch in the README.
    let json = serde_json::to_value(&ev).unwrap();
    for (k, v) in [
        ("type", serde_json::json!("delta")),
        ("symbol", serde_json::json!("XYZ")),
        ("seq", serde_json::json!(10_204)),
        ("side", serde_json::json!("BID")),
        ("price_ticks", serde_json::json!(10_025)),
        ("quantity", serde_json::json!(350)),
    ] {
        assert_eq!(json[k], v, "{k}");
    }
}

#[test]
fn zero_size_becomes_an_explicit_delete() {
    let ev = normalizer()
        .decode(&raw_delta(1, 0, "sell", "100.30", "0"))
        .unwrap()
        .unwrap();
    let FeedEvent::Delta(d) = ev else { panic!() };
    assert_eq!((d.side, d.action, d.qty), (Side::Ask, Action::Delete, 0));
}

#[test]
fn snapshot_is_normalized() {
    let raw = raw_snapshot(
        5,
        99,
        &[("100.00", "10"), ("99.99", "20")],
        &[("100.01", "5")],
    );
    let FeedEvent::Snapshot(s) = normalizer().decode(&raw).unwrap().unwrap() else {
        panic!()
    };
    assert_eq!((s.seq, s.captured_at), (5, 99));
    assert_eq!(s.bids, vec![lv(10_000, 10), lv(9_999, 20)]);
    assert_eq!(s.asks, vec![lv(10_001, 5)]);
}

#[test]
fn heartbeat_carries_no_event() {
    assert_eq!(
        normalizer().decode(r#"{"type":"heartbeat","ts":1}"#),
        Ok(None)
    );
}

#[test]
fn undecodable_frames_are_errors_not_panics() {
    let n = normalizer();
    assert!(matches!(
        n.decode("{\"type\":\"delta\",\"symb"),
        Err(DecodeError::Json(_))
    ));
    assert!(matches!(n.decode(""), Err(DecodeError::Json(_))));
    assert!(matches!(
        n.decode(r#"{"type":"trade"}"#),
        Err(DecodeError::Json(_))
    ));
    let other = raw_delta(1, 0, "buy", "1.00", "1").replace("XYZ", "NOPE");
    assert_eq!(
        n.decode(&other),
        Err(DecodeError::UnknownSymbol("NOPE".into()))
    );
    assert!(matches!(
        n.decode(&raw_delta(1, 0, "hold", "1.00", "1")),
        Err(DecodeError::UnknownSide(_))
    ));
    assert!(matches!(
        n.decode(&raw_delta(1, 0, "buy", "1.005", "1")),
        Err(DecodeError::Price(_))
    ));
    assert!(matches!(
        n.decode(&raw_delta(1, 0, "buy", "1.00", "1.5")),
        Err(DecodeError::Size(_))
    ));
    assert!(matches!(
        n.decode(&raw_delta(1, 0, "buy", "1.00", "-3")),
        Err(DecodeError::Size(_))
    ));
    assert_eq!(
        n.decode(&raw_delta(1, 0, "buy", "0.00", "1")),
        Err(DecodeError::NonPositivePrice)
    );
    // Numbers instead of decimal strings would invite float parsing: refused.
    let floaty =
        r#"{"type":"delta","symbol":"XYZ","seq":1,"ts":0,"side":"buy","price":100.25,"size":"1"}"#;
    assert!(matches!(n.decode(floaty), Err(DecodeError::Json(_))));
}

#[test]
fn fault_plan_parsing() {
    let plan =
        parse_fault_plan("dup@900, drop@500:3,reorder@1300,cross@1700,garbage@2,skew@40:-2500")
            .unwrap();
    assert_eq!(plan[0], (2, Fault::Garbage));
    assert_eq!(
        plan[1],
        (
            40,
            Fault::ClockSkew {
                offset_ms: -2_500,
                count: 50
            }
        )
    );
    assert_eq!(plan[2], (500, Fault::Drop { count: 3 }));
    assert_eq!(plan[3], (900, Fault::Duplicate));
    assert_eq!(plan.len(), 6);
    assert_eq!(parse_fault_plan("").unwrap(), vec![]);
    assert!(parse_fault_plan("drop").is_err());
    assert!(parse_fault_plan("drop@x").is_err());
    assert!(parse_fault_plan("explode@5").is_err());
}
