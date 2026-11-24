//! Property tests: the engine is compared, command by command, against a
//! deliberately naive reference implementation of price-time priority.

use super::*;
use proptest::prelude::*;

#[derive(Clone, Debug)]
struct RefOrder {
    id: u64,
    side: Side,
    px: i64,
    qty: u64,
    prio: u64,
}

/// O(n) per operation, written for obviousness rather than speed.
#[derive(Default)]
struct RefBook {
    orders: Vec<RefOrder>,
}

impl RefBook {
    fn execute(
        &mut self,
        id: u64,
        side: Side,
        limit: Option<i64>,
        mut qty: u64,
        prio: u64,
    ) -> Vec<(u64, u64, u64, i64)> {
        let mut trades = Vec::new();
        while qty > 0 {
            let best = self
                .orders
                .iter()
                .enumerate()
                .filter(|(_, o)| o.side != side)
                .filter(|(_, o)| match (limit, side) {
                    (None, _) => true,
                    (Some(l), Side::Buy) => o.px <= l,
                    (Some(l), Side::Sell) => o.px >= l,
                })
                .min_by_key(|(_, o)| (if side == Side::Buy { o.px } else { -o.px }, o.prio))
                .map(|(i, _)| i);
            let Some(i) = best else { break };
            let fill = qty.min(self.orders[i].qty);
            trades.push((self.orders[i].id, id, fill, self.orders[i].px));
            self.orders[i].qty -= fill;
            qty -= fill;
            if self.orders[i].qty == 0 {
                self.orders.remove(i);
            }
        }
        if let (true, Some(px)) = (qty > 0, limit) {
            self.orders.push(RefOrder {
                id,
                side,
                px,
                qty,
                prio,
            });
        }
        trades
    }

    fn apply(&mut self, cmd: &Command, seq: u64) -> Vec<(u64, u64, u64, i64)> {
        let live = |orders: &[RefOrder], id| orders.iter().position(|o| o.id == id);
        match *cmd {
            Command::New(o) => {
                if live(&self.orders, o.order_id).is_some() {
                    return vec![];
                }
                self.execute(o.order_id, o.side, o.price_ticks, o.qty, seq)
            }
            Command::Cancel { order_id } => {
                if let Some(i) = live(&self.orders, order_id) {
                    self.orders.remove(i);
                }
                vec![]
            }
            Command::Replace {
                order_id,
                qty,
                price_ticks,
            } => {
                let Some(i) = live(&self.orders, order_id) else {
                    return vec![];
                };
                if self.orders[i].px == price_ticks && qty <= self.orders[i].qty {
                    self.orders[i].qty = qty;
                    vec![]
                } else {
                    let old = self.orders.remove(i);
                    self.execute(order_id, old.side, Some(price_ticks), qty, seq)
                }
            }
        }
    }

    /// `(id, price, qty)` best-first, FIFO within price.
    fn side(&self, side: Side) -> Vec<(u64, i64, u64)> {
        let mut v: Vec<&RefOrder> = self.orders.iter().filter(|o| o.side == side).collect();
        v.sort_by_key(|o| (if side == Side::Buy { -o.px } else { o.px }, o.prio));
        v.iter().map(|o| (o.id, o.px, o.qty)).collect()
    }
}

fn side() -> impl Strategy<Value = Side> {
    prop_oneof![Just(Side::Buy), Just(Side::Sell)]
}

/// Tight id and price ranges so commands collide and interact constantly.
fn command(symbols: &'static [&'static str]) -> impl Strategy<Value = Command> {
    let sym = proptest::sample::select(symbols);
    prop_oneof![
        5 => (1u64..40, sym.clone(), side(), 95i64..=105, 1u64..=20)
            .prop_map(|(id, s, sd, px, q)| limit(id, s, sd, px, q)),
        1 => (1u64..40, sym, side(), 1u64..=40).prop_map(|(id, s, sd, q)| market(id, s, sd, q)),
        2 => (1u64..40).prop_map(|order_id| Command::Cancel { order_id }),
        2 => (1u64..40, 1u64..=25, 95i64..=105)
            .prop_map(|(order_id, qty, price_ticks)| Command::Replace { order_id, qty, price_ticks }),
    ]
}

fn sequenced(cmds: &[Command]) -> Vec<SequencedCommand> {
    cmds.iter()
        .enumerate()
        .map(|(i, cmd)| SequencedCommand {
            seq: i as u64 + 1,
            ts_ns: 1000 + i as u64,
            cmd: *cmd,
        })
        .collect()
}

fn run_bytes(engine: &mut Engine, cmds: &[SequencedCommand]) -> Vec<u8> {
    let mut bytes = Vec::new();
    let mut out = Vec::new();
    for sc in cmds {
        out.clear();
        engine.process(sc, &mut out).unwrap();
        for e in &out {
            serde_json::to_writer(&mut bytes, e).unwrap();
            bytes.push(b'\n');
        }
    }
    bytes
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(400))]

    #[test]
    fn engine_matches_reference_model(cmds in prop::collection::vec(command(&["XYZ"]), 1..200)) {
        let mut h = Harness::new();
        let mut model = RefBook::default();
        let mut next_event_seq = 1;
        let mut last_top: Option<EventKind> = None;
        for cmd in &cmds {
            let events = h.send(*cmd); // also checks every book invariant
            let expected = model.apply(cmd, h.seq);
            prop_assert_eq!(trades(&events), expected, "trades differ at seq {}", h.seq);

            prop_assert!(!events.is_empty(), "every command yields at least one event");
            for e in &events {
                prop_assert_eq!(e.seq, next_event_seq);
                next_event_seq += 1;
                prop_assert_eq!(e.cmd_seq, h.seq);
                if let EventKind::BookTop { bid_px, ask_px, .. } = e.kind {
                    if let (Some(b), Some(a)) = (bid_px, ask_px) {
                        prop_assert!(b < a, "published a crossed top: {} >= {}", b, a);
                    }
                    last_top = Some(e.kind);
                }
            }

            let snap = h.engine.snapshot();
            let book = snap.books.first();
            let got = |pick: fn(&BookSnapshot) -> &Vec<OrderSnapshot>| -> Vec<(u64, i64, u64)> {
                book.map(|b| pick(b).iter().map(|o| (o.order_id, o.price_ticks, o.remaining_qty)).collect())
                    .unwrap_or_default()
            };
            prop_assert_eq!(got(|b| &b.bids), model.side(Side::Buy));
            prop_assert_eq!(got(|b| &b.asks), model.side(Side::Sell));
        }
        // The last published top of book is the real top of book.
        let top = h.top("XYZ");
        match last_top {
            Some(EventKind::BookTop { bid_px, bid_qty, ask_px, ask_qty, .. }) => {
                prop_assert_eq!(bid_px.map(|p| (p, bid_qty)), top.bid);
                prop_assert_eq!(ask_px.map(|p| (p, ask_qty)), top.ask);
            }
            _ => prop_assert_eq!(top, Top::default()),
        }
    }

    #[test]
    fn replay_is_byte_identical(cmds in prop::collection::vec(command(&["AAA", "BBB"]), 1..200)) {
        let log = sequenced(&cmds);
        let (mut a, mut b) = (Engine::new(), Engine::new());
        prop_assert_eq!(run_bytes(&mut a, &log), run_bytes(&mut b, &log));
        prop_assert_eq!(a.snapshot(), b.snapshot());
    }

    #[test]
    fn snapshot_plus_tail_equals_full_replay(
        cmds in prop::collection::vec(command(&["AAA", "BBB"]), 1..200),
        cut in any::<prop::sample::Index>(),
    ) {
        let log = sequenced(&cmds);
        let cut = cut.index(log.len() + 1);

        let mut full = Engine::new();
        run_bytes(&mut full, &log[..cut]);
        let json = serde_json::to_vec(&full.snapshot()).unwrap();
        let tail_full = run_bytes(&mut full, &log[cut..]);

        let mut restored = Engine::from_snapshot(&serde_json::from_slice(&json).unwrap()).unwrap();
        let tail_restored = run_bytes(&mut restored, &log[cut..]);

        prop_assert_eq!(tail_restored, tail_full);
        prop_assert_eq!(restored.snapshot(), full.snapshot());
        restored.check_invariants().unwrap();
    }

    #[test]
    fn fills_never_exceed_order_quantity(cmds in prop::collection::vec(command(&["XYZ"]), 1..200)) {
        // Per order *lifetime*: executions count down leaves_qty exactly and
        // never go below zero (u64 underflow would panic in debug builds).
        let mut h = Harness::new();
        let mut leaves: std::collections::HashMap<u64, u64> = Default::default();
        for cmd in &cmds {
            for e in h.send(*cmd) {
                match e.kind {
                    EventKind::Accepted { order_id, qty, .. }
                    | EventKind::Replaced { order_id, qty, .. } => { leaves.insert(order_id, qty); }
                    EventKind::Execution { order_id, qty, leaves_qty, .. } => {
                        let before = leaves[&order_id];
                        prop_assert!(qty > 0 && qty <= before);
                        prop_assert_eq!(leaves_qty, before - qty);
                        leaves.insert(order_id, leaves_qty);
                    }
                    EventKind::Canceled { order_id, canceled_qty, .. } => {
                        prop_assert_eq!(leaves.remove(&order_id), Some(canceled_qty));
                    }
                    _ => {}
                }
            }
        }
    }
}
