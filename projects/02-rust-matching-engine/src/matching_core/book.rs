//! One symbol's limit order book.
//!
//! Price levels live in two `BTreeMap`s (bids and asks). Orders live in a slab
//! (`Vec` + free list) and each level threads its orders into an intrusive
//! doubly-linked FIFO, so insert, cancel and fill are O(log levels) with no
//! per-order heap allocation once the slab has grown.

use super::types::{AccountId, OrderId, Price, Qty, Seq, Side, Symbol};
use std::collections::BTreeMap;

const NIL: u32 = u32::MAX;

/// A resting order.
#[derive(Clone, Copy, Debug)]
pub struct OrderNode {
    pub order_id: OrderId,
    pub account_id: AccountId,
    pub side: Side,
    pub price_ticks: Price,
    pub remaining_qty: Qty,
    /// Command sequence at which the order gained its current queue priority.
    pub seq: Seq,
    prev: u32,
    next: u32,
}

#[derive(Clone, Copy, Debug)]
struct Level {
    head: u32,
    tail: u32,
    total_qty: Qty,
    count: u32,
}

/// One maker fill produced while matching an incoming order.
#[derive(Clone, Copy, Debug)]
pub struct Fill {
    pub maker_order_id: OrderId,
    pub price_ticks: Price,
    pub qty: Qty,
    pub maker_leaves_qty: Qty,
}

/// Best bid and ask as `(price, aggregate quantity)`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct Top {
    pub bid: Option<(Price, Qty)>,
    pub ask: Option<(Price, Qty)>,
}

#[derive(Debug)]
pub struct Book {
    symbol: Symbol,
    bids: BTreeMap<Price, Level>,
    asks: BTreeMap<Price, Level>,
    nodes: Vec<OrderNode>,
    free: Vec<u32>,
    live: usize,
}

impl Book {
    pub fn new(symbol: Symbol) -> Self {
        Book {
            symbol,
            bids: BTreeMap::new(),
            asks: BTreeMap::new(),
            nodes: Vec::new(),
            free: Vec::new(),
            live: 0,
        }
    }

    pub fn symbol(&self) -> Symbol {
        self.symbol
    }

    /// Number of resting orders.
    pub fn len(&self) -> usize {
        self.live
    }

    pub fn is_empty(&self) -> bool {
        self.live == 0
    }

    pub fn top(&self) -> Top {
        Top {
            bid: self.bids.last_key_value().map(|(p, l)| (*p, l.total_qty)),
            ask: self.asks.first_key_value().map(|(p, l)| (*p, l.total_qty)),
        }
    }

    pub(crate) fn node(&self, slot: u32) -> &OrderNode {
        &self.nodes[slot as usize]
    }

    /// Appends an order to the back of its price level and returns its slot.
    pub(crate) fn insert(
        &mut self,
        order_id: OrderId,
        account_id: AccountId,
        side: Side,
        price_ticks: Price,
        qty: Qty,
        seq: Seq,
    ) -> u32 {
        debug_assert!(qty > 0);
        let node = OrderNode {
            order_id,
            account_id,
            side,
            price_ticks,
            remaining_qty: qty,
            seq,
            prev: NIL,
            next: NIL,
        };
        let slot = match self.free.pop() {
            Some(slot) => {
                self.nodes[slot as usize] = node;
                slot
            }
            None => {
                let slot = u32::try_from(self.nodes.len()).expect("book slab exceeds u32 slots");
                assert!(slot != NIL, "book slab exceeds u32 slots");
                self.nodes.push(node);
                slot
            }
        };
        let levels = match side {
            Side::Buy => &mut self.bids,
            Side::Sell => &mut self.asks,
        };
        let level = levels.entry(price_ticks).or_insert(Level {
            head: NIL,
            tail: NIL,
            total_qty: 0,
            count: 0,
        });
        if level.tail == NIL {
            level.head = slot;
        } else {
            self.nodes[level.tail as usize].next = slot;
            self.nodes[slot as usize].prev = level.tail;
        }
        level.tail = slot;
        level.total_qty += qty;
        level.count += 1;
        self.live += 1;
        slot
    }

    /// Unlinks a resting order and returns it.
    pub(crate) fn remove(&mut self, slot: u32) -> OrderNode {
        let node = self.nodes[slot as usize];
        let levels = match node.side {
            Side::Buy => &mut self.bids,
            Side::Sell => &mut self.asks,
        };
        let level = levels
            .get_mut(&node.price_ticks)
            .expect("resting order has a price level");
        if node.prev == NIL {
            level.head = node.next;
        } else {
            self.nodes[node.prev as usize].next = node.next;
        }
        if node.next == NIL {
            level.tail = node.prev;
        } else {
            self.nodes[node.next as usize].prev = node.prev;
        }
        level.total_qty -= node.remaining_qty;
        level.count -= 1;
        if level.count == 0 {
            levels.remove(&node.price_ticks);
        }
        self.release(slot);
        node
    }

    /// Reduces a resting order's quantity in place (queue position unchanged).
    pub(crate) fn reduce(&mut self, slot: u32, new_qty: Qty) {
        let node = &mut self.nodes[slot as usize];
        debug_assert!(new_qty > 0 && new_qty <= node.remaining_qty);
        let delta = node.remaining_qty - new_qty;
        node.remaining_qty = new_qty;
        let levels = match node.side {
            Side::Buy => &mut self.bids,
            Side::Sell => &mut self.asks,
        };
        levels
            .get_mut(&node.price_ticks)
            .expect("resting order has a price level")
            .total_qty -= delta;
    }

    /// Matches an incoming order against the opposite side in price-time
    /// priority. `limit` of `None` means a market order. Calls `on_fill` once
    /// per maker fill, in execution order, and returns the unfilled quantity.
    pub(crate) fn match_incoming(
        &mut self,
        taker_side: Side,
        limit: Option<Price>,
        mut qty: Qty,
        on_fill: &mut impl FnMut(Fill),
    ) -> Qty {
        while qty > 0 {
            let best = match taker_side {
                Side::Buy => self.asks.first_entry(),
                Side::Sell => self.bids.last_entry(),
            };
            let Some(mut entry) = best else { break };
            let price = *entry.key();
            if let Some(limit) = limit {
                let crosses = match taker_side {
                    Side::Buy => price <= limit,
                    Side::Sell => price >= limit,
                };
                if !crosses {
                    break;
                }
            }
            let level = entry.get_mut();
            while qty > 0 && level.head != NIL {
                let slot = level.head;
                let node = &mut self.nodes[slot as usize];
                let fill = qty.min(node.remaining_qty);
                node.remaining_qty -= fill;
                level.total_qty -= fill;
                qty -= fill;
                let maker_order_id = node.order_id;
                let maker_leaves_qty = node.remaining_qty;
                if maker_leaves_qty == 0 {
                    let next = node.next;
                    level.head = next;
                    if next == NIL {
                        level.tail = NIL;
                    } else {
                        self.nodes[next as usize].prev = NIL;
                    }
                    level.count -= 1;
                    self.free.push(slot);
                    self.live -= 1;
                }
                on_fill(Fill {
                    maker_order_id,
                    price_ticks: price,
                    qty: fill,
                    maker_leaves_qty,
                });
            }
            if level.count == 0 {
                entry.remove();
            }
        }
        qty
    }

    fn release(&mut self, slot: u32) {
        self.nodes[slot as usize].remaining_qty = 0;
        self.free.push(slot);
        self.live -= 1;
    }

    /// Resting orders on one side, best price first, FIFO within a level.
    pub fn orders(&self, side: Side) -> Vec<OrderNode> {
        let mut out = Vec::new();
        let mut walk = |level: &Level| {
            let mut cur = level.head;
            while cur != NIL {
                let node = self.nodes[cur as usize];
                out.push(node);
                cur = node.next;
            }
        };
        match side {
            Side::Buy => self.bids.values().rev().for_each(&mut walk),
            Side::Sell => self.asks.values().for_each(&mut walk),
        }
        out
    }

    /// Up to `levels` price levels as `(price, aggregate qty, order count)`,
    /// best first.
    pub fn depth(&self, side: Side, levels: usize) -> Vec<(Price, Qty, u32)> {
        let row = |(p, l): (&Price, &Level)| (*p, l.total_qty, l.count);
        match side {
            Side::Buy => self.bids.iter().rev().take(levels).map(row).collect(),
            Side::Sell => self.asks.iter().take(levels).map(row).collect(),
        }
    }

    /// Verifies every structural invariant of the book. Intended for tests
    /// and recovery checks; cost is linear in the number of resting orders.
    pub fn check_invariants(&self) -> Result<(), String> {
        let top = self.top();
        if let (Some((bid, _)), Some((ask, _))) = (top.bid, top.ask) {
            if bid >= ask {
                return Err(format!(
                    "{}: crossed book, bid {bid} >= ask {ask}",
                    self.symbol
                ));
            }
        }
        let mut seen = 0usize;
        for (side, levels) in [(Side::Buy, &self.bids), (Side::Sell, &self.asks)] {
            for (price, level) in levels {
                let ctx = format!("{} {side:?} level {price}", self.symbol);
                if level.count == 0 || level.head == NIL || level.tail == NIL {
                    return Err(format!("{ctx}: empty level left in book"));
                }
                let (mut cur, mut prev) = (level.head, NIL);
                let (mut count, mut total) = (0u32, 0u64);
                let mut last_seq = None;
                while cur != NIL {
                    let node = &self.nodes[cur as usize];
                    if node.prev != prev {
                        return Err(format!("{ctx}: broken prev link at slot {cur}"));
                    }
                    if node.remaining_qty == 0 {
                        return Err(format!("{ctx}: order {} has zero qty", node.order_id));
                    }
                    if node.price_ticks != *price || node.side != side {
                        return Err(format!("{ctx}: order {} is misfiled", node.order_id));
                    }
                    if last_seq.is_some_and(|s| s >= node.seq) {
                        return Err(format!(
                            "{ctx}: FIFO violated at order {} (seq {})",
                            node.order_id, node.seq
                        ));
                    }
                    last_seq = Some(node.seq);
                    count += 1;
                    total += node.remaining_qty;
                    prev = cur;
                    cur = node.next;
                }
                if prev != level.tail {
                    return Err(format!("{ctx}: tail does not match last order"));
                }
                if count != level.count || total != level.total_qty {
                    return Err(format!(
                        "{ctx}: aggregate mismatch (count {count} vs {}, qty {total} vs {})",
                        level.count, level.total_qty
                    ));
                }
                seen += count as usize;
            }
        }
        if seen != self.live || self.nodes.len() - self.free.len() != self.live {
            return Err(format!(
                "{}: live count mismatch (walked {seen}, live {}, slab {} - free {})",
                self.symbol,
                self.live,
                self.nodes.len(),
                self.free.len()
            ));
        }
        Ok(())
    }
}
