//! Shared builders for the test suites.

use book_core::{Action, BookDelta, BookSnapshot, FeedEvent, Level, Side};
use feed_adapter::{Frame, SimConfig, SimSession, SimSymbol};
use stream_api::engine::{Engine, EngineConfig, Output, StatusKind};

pub const SYM: &str = "XYZ";

pub fn lv(price_ticks: i64, qty: u64) -> Level {
    Level { price_ticks, qty }
}

pub fn set(seq: u64, side: Side, price_ticks: i64, qty: u64) -> BookDelta {
    BookDelta {
        symbol: SYM.to_string(),
        seq,
        side,
        price_ticks,
        qty,
        action: if qty == 0 {
            Action::Delete
        } else {
            Action::Set
        },
        event_ts: 1_000 * seq as i64,
    }
}

pub fn snap(seq: u64, bids: &[(i64, u64)], asks: &[(i64, u64)]) -> BookSnapshot {
    BookSnapshot {
        symbol: SYM.to_string(),
        seq,
        bids: bids.iter().map(|&(p, q)| lv(p, q)).collect(),
        asks: asks.iter().map(|&(p, q)| lv(p, q)).collect(),
        captured_at: 1_000 * seq as i64,
    }
}

pub fn ev_delta(seq: u64, side: Side, price_ticks: i64, qty: u64) -> FeedEvent {
    FeedEvent::Delta(set(seq, side, price_ticks, qty))
}

/// Raw venue-format frames (tick 0.01, lot 1).
pub fn raw_snapshot(seq: u64, ts: i64, bids: &[(&str, &str)], asks: &[(&str, &str)]) -> String {
    let side = |l: &[(&str, &str)]| -> Vec<[String; 2]> {
        l.iter()
            .map(|(p, q)| [p.to_string(), q.to_string()])
            .collect()
    };
    serde_json::json!({"type":"snapshot","symbol":SYM,"seq":seq,"ts":ts,"bids":side(bids),"asks":side(asks)})
        .to_string()
}

pub fn raw_delta(seq: u64, ts: i64, side: &str, price: &str, size: &str) -> String {
    serde_json::json!({"type":"delta","symbol":SYM,"seq":seq,"ts":ts,"side":side,"price":price,"size":size})
        .to_string()
}

pub fn sim_config(seed: u64) -> SimConfig {
    SimConfig {
        symbols: vec![SimSymbol::new(SYM, 10_000)],
        seed,
        ..SimConfig::default()
    }
}

pub fn engine_for(session: &SimSession) -> Engine {
    Engine::new(EngineConfig::default(), session.instruments())
}

pub fn single_symbol_engine(cfg: EngineConfig) -> Engine {
    Engine::new(cfg, vec![SimSymbol::new(SYM, 10_000).instrument()])
}

/// Closed loop: session → engine, answering resync requests. Returns every
/// output the engine produced.
pub fn pump(session: &mut SimSession, engine: &mut Engine, frames: usize) -> Vec<Output> {
    let mut all = Vec::new();
    for _ in 0..frames {
        let frame: Frame = session.next_frame(None);
        let mut outs = engine.on_frame(&frame.raw, frame.recv_ts);
        outs.extend(engine.tick(frame.recv_ts));
        for o in &outs {
            if let Output::ResyncRequest { symbol } = o {
                session.request_snapshot(symbol);
            }
        }
        all.extend(outs);
    }
    all
}

pub fn kinds(outputs: &[Output]) -> Vec<StatusKind> {
    outputs
        .iter()
        .filter_map(|o| match o {
            Output::Status(s) => Some(s.kind),
            _ => None,
        })
        .collect()
}

pub fn count(outputs: &[Output], kind: StatusKind) -> usize {
    kinds(outputs).into_iter().filter(|k| *k == kind).count()
}
