//! Unit-level tests of the matching core through its public API.

mod properties;
mod semantics;

use matching_engine::*;

pub fn sym(s: &str) -> Symbol {
    Symbol::new(s).unwrap()
}

pub fn limit(order_id: u64, symbol: &str, side: Side, price: i64, qty: u64) -> Command {
    Command::New(NewOrder {
        order_id,
        account_id: 0,
        symbol: sym(symbol),
        side,
        ord_type: OrdType::Limit,
        qty,
        price_ticks: Some(price),
    })
}

pub fn market(order_id: u64, symbol: &str, side: Side, qty: u64) -> Command {
    Command::New(NewOrder {
        order_id,
        account_id: 0,
        symbol: sym(symbol),
        side,
        ord_type: OrdType::Market,
        qty,
        price_ticks: None,
    })
}

/// Engine plus an inline sequencer; checks all invariants after every command.
pub struct Harness {
    pub engine: Engine,
    pub seq: u64,
}

impl Default for Harness {
    fn default() -> Self {
        Harness {
            engine: Engine::new(),
            seq: 0,
        }
    }
}

impl Harness {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn send(&mut self, cmd: Command) -> Vec<Event> {
        self.seq += 1;
        let mut out = Vec::new();
        let sc = SequencedCommand {
            seq: self.seq,
            ts_ns: self.seq,
            cmd,
        };
        self.engine.process(&sc, &mut out).unwrap();
        self.engine.check_invariants().unwrap();
        out
    }

    pub fn top(&self, symbol: &str) -> Top {
        self.engine
            .book(sym(symbol))
            .map(Book::top)
            .unwrap_or_default()
    }
}

/// `(maker, taker, qty, price)` for every trade in `events`.
pub fn trades(events: &[Event]) -> Vec<(u64, u64, u64, i64)> {
    events
        .iter()
        .filter_map(|e| match e.kind {
            EventKind::Trade {
                maker_order_id,
                taker_order_id,
                qty,
                price_ticks,
                ..
            } => Some((maker_order_id, taker_order_id, qty, price_ticks)),
            _ => None,
        })
        .collect()
}

pub fn rejection(events: &[Event]) -> Option<RejectReason> {
    match events {
        [Event {
            kind: EventKind::Rejected { reason, .. },
            ..
        }] => Some(*reason),
        _ => None,
    }
}
