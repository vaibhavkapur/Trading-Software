//! Scenario tests: failure modes (duplicate delivery, missing data, clock
//! skew, rejected writes, corruption) and pinned replay fixtures.

mod failures;
mod replay_fixtures;

use matching_engine::*;

pub fn limit(order_id: u64, side: Side, price: i64, qty: u64) -> Command {
    Command::New(NewOrder {
        order_id,
        account_id: 0,
        symbol: Symbol::new("XYZ").unwrap(),
        side,
        ord_type: OrdType::Limit,
        qty,
        price_ticks: Some(price),
    })
}

pub fn events_to_bytes(events: &[Event]) -> Vec<u8> {
    let mut bytes = Vec::new();
    for e in events {
        serde_json::to_writer(&mut bytes, e).unwrap();
        bytes.push(b'\n');
    }
    bytes
}
