//! The deterministic, single-writer matching core.
//!
//! `Engine::process` is a pure function of (current state, sequenced command):
//! it reads no clock, performs no I/O and never iterates a hash map, so the
//! same command log always produces the same event stream.

use super::book::{Book, Fill, Top};
use super::types::*;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::fmt;
use std::hash::{BuildHasherDefault, Hasher};

/// Cheap hasher for integer order ids. Only used for point lookups, never
/// iteration, so it cannot affect determinism.
#[derive(Default)]
pub struct IdHasher(u64);

impl Hasher for IdHasher {
    fn write(&mut self, bytes: &[u8]) {
        for b in bytes {
            self.0 = (self.0 ^ u64::from(*b)).wrapping_mul(0x0000_0100_0000_01b3);
        }
    }
    fn write_u64(&mut self, n: u64) {
        let x = n.wrapping_mul(0x9E37_79B9_7F4A_7C15);
        self.0 = x ^ (x >> 32);
    }
    fn finish(&self) -> u64 {
        self.0
    }
}

type IdMap<V> = HashMap<OrderId, V, BuildHasherDefault<IdHasher>>;

#[derive(Clone, Copy, Debug)]
struct Loc {
    book: u32,
    slot: u32,
}

/// The command's sequence number was not exactly `last_seq + 1`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SequenceError {
    pub expected: Seq,
    pub got: Seq,
}

impl fmt::Display for SequenceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let what = if self.got < self.expected {
            "stale or duplicate"
        } else {
            "gap in"
        };
        write!(
            f,
            "{what} command sequence: expected {}, got {}",
            self.expected, self.got
        )
    }
}

impl std::error::Error for SequenceError {}

pub const SNAPSHOT_VERSION: u32 = 1;

/// Canonical, serialisable image of the whole engine. Two engines are in the
/// same state iff their snapshots are equal.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct EngineSnapshot {
    pub version: u32,
    pub last_seq: Seq,
    pub last_ts_ns: u64,
    pub next_event_seq: Seq,
    pub next_trade_id: u64,
    /// Sorted by symbol.
    pub books: Vec<BookSnapshot>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct BookSnapshot {
    pub symbol: Symbol,
    /// Best (highest) price first, FIFO within a price.
    pub bids: Vec<OrderSnapshot>,
    /// Best (lowest) price first, FIFO within a price.
    pub asks: Vec<OrderSnapshot>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct OrderSnapshot {
    pub order_id: OrderId,
    pub account_id: AccountId,
    pub price_ticks: Price,
    pub remaining_qty: Qty,
    pub seq: Seq,
}

struct Emitter<'a> {
    out: &'a mut Vec<Event>,
    next_event_seq: &'a mut Seq,
    cmd_seq: Seq,
    ts_ns: u64,
}

impl Emitter<'_> {
    #[inline]
    fn emit(&mut self, kind: EventKind) {
        self.out.push(Event {
            seq: *self.next_event_seq,
            cmd_seq: self.cmd_seq,
            ts_ns: self.ts_ns,
            kind,
        });
        *self.next_event_seq += 1;
    }

    fn reject(&mut self, order_id: OrderId, reason: RejectReason) {
        self.emit(EventKind::Rejected { order_id, reason });
    }
}

#[derive(Default)]
struct State {
    books: Vec<Book>,
    symbols: HashMap<Symbol, u32>,
    index: IdMap<Loc>,
    next_trade_id: u64,
}

pub struct Engine {
    st: State,
    last_seq: Seq,
    last_ts_ns: u64,
    next_event_seq: Seq,
}

impl fmt::Debug for Engine {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Engine")
            .field("last_seq", &self.last_seq)
            .field("next_event_seq", &self.next_event_seq)
            .field("books", &self.st.books.len())
            .field("open_orders", &self.st.index.len())
            .finish()
    }
}

impl Default for Engine {
    fn default() -> Self {
        Self::new()
    }
}

impl Engine {
    pub fn new() -> Self {
        Engine {
            st: State {
                next_trade_id: 1,
                ..State::default()
            },
            last_seq: 0,
            last_ts_ns: 0,
            next_event_seq: 1,
        }
    }

    /// Sequence number of the last command applied (0 if none).
    pub fn last_seq(&self) -> Seq {
        self.last_seq
    }

    /// Venue timestamp of the last command applied.
    pub fn last_ts_ns(&self) -> u64 {
        self.last_ts_ns
    }

    /// Number of resting orders across all books.
    pub fn open_orders(&self) -> usize {
        self.st.index.len()
    }

    pub fn book(&self, symbol: Symbol) -> Option<&Book> {
        self.st
            .symbols
            .get(&symbol)
            .map(|i| &self.st.books[*i as usize])
    }

    /// Symbols with a book, sorted.
    pub fn symbols(&self) -> Vec<Symbol> {
        let mut v: Vec<Symbol> = self.st.books.iter().map(Book::symbol).collect();
        v.sort_unstable();
        v
    }

    /// Applies one command and appends the resulting events to `out`.
    ///
    /// Commands must arrive with consecutive sequence numbers starting at
    /// `last_seq() + 1`; anything else is refused without touching state.
    /// Business-level failures (unknown order, bad price, ...) are not errors:
    /// they produce a `REJECTED` event and consume the sequence number.
    pub fn process(
        &mut self,
        sc: &SequencedCommand,
        out: &mut Vec<Event>,
    ) -> Result<(), SequenceError> {
        let expected = self.last_seq + 1;
        if sc.seq != expected {
            return Err(SequenceError {
                expected,
                got: sc.seq,
            });
        }
        self.last_seq = sc.seq;
        self.last_ts_ns = sc.ts_ns;
        let mut em = Emitter {
            out,
            next_event_seq: &mut self.next_event_seq,
            cmd_seq: sc.seq,
            ts_ns: sc.ts_ns,
        };
        match &sc.cmd {
            Command::New(order) => self.st.new_order(&mut em, order, sc.seq),
            Command::Cancel { order_id } => self.st.cancel(&mut em, *order_id),
            Command::Replace {
                order_id,
                qty,
                price_ticks,
            } => self
                .st
                .replace(&mut em, *order_id, *qty, *price_ticks, sc.seq),
        }
        Ok(())
    }

    pub fn snapshot(&self) -> EngineSnapshot {
        let mut books: Vec<BookSnapshot> = self
            .st
            .books
            .iter()
            .map(|b| {
                let side = |s| {
                    b.orders(s)
                        .into_iter()
                        .map(|n| OrderSnapshot {
                            order_id: n.order_id,
                            account_id: n.account_id,
                            price_ticks: n.price_ticks,
                            remaining_qty: n.remaining_qty,
                            seq: n.seq,
                        })
                        .collect()
                };
                BookSnapshot {
                    symbol: b.symbol(),
                    bids: side(Side::Buy),
                    asks: side(Side::Sell),
                }
            })
            .collect();
        books.sort_unstable_by_key(|b| b.symbol);
        EngineSnapshot {
            version: SNAPSHOT_VERSION,
            last_seq: self.last_seq,
            last_ts_ns: self.last_ts_ns,
            next_event_seq: self.next_event_seq,
            next_trade_id: self.st.next_trade_id,
            books,
        }
    }

    /// Rebuilds an engine from a snapshot, validating it on the way in.
    pub fn from_snapshot(snap: &EngineSnapshot) -> Result<Self, String> {
        if snap.version != SNAPSHOT_VERSION {
            return Err(format!(
                "unsupported snapshot version {} (expected {SNAPSHOT_VERSION})",
                snap.version
            ));
        }
        let mut st = State {
            next_trade_id: snap.next_trade_id,
            ..State::default()
        };
        for bs in &snap.books {
            if st.symbols.contains_key(&bs.symbol) {
                return Err(format!("duplicate book for {}", bs.symbol));
            }
            let bi = st.book_index(bs.symbol);
            for (side, orders) in [(Side::Buy, &bs.bids), (Side::Sell, &bs.asks)] {
                for o in orders {
                    if o.remaining_qty == 0 || o.remaining_qty > MAX_QTY {
                        return Err(format!("order {}: invalid quantity", o.order_id));
                    }
                    if o.price_ticks <= 0 || o.price_ticks > MAX_PRICE_TICKS {
                        return Err(format!("order {}: invalid price", o.order_id));
                    }
                    if o.seq > snap.last_seq {
                        return Err(format!("order {}: seq is after snapshot", o.order_id));
                    }
                    if st.index.contains_key(&o.order_id) {
                        return Err(format!("order {}: duplicate id", o.order_id));
                    }
                    let slot = st.books[bi].insert(
                        o.order_id,
                        o.account_id,
                        side,
                        o.price_ticks,
                        o.remaining_qty,
                        o.seq,
                    );
                    st.index.insert(
                        o.order_id,
                        Loc {
                            book: bi as u32,
                            slot,
                        },
                    );
                }
            }
        }
        let engine = Engine {
            st,
            last_seq: snap.last_seq,
            last_ts_ns: snap.last_ts_ns,
            next_event_seq: snap.next_event_seq,
        };
        engine.check_invariants()?;
        Ok(engine)
    }

    /// Verifies all book invariants and that the order index matches them.
    pub fn check_invariants(&self) -> Result<(), String> {
        let mut resting = 0;
        for (i, book) in self.st.books.iter().enumerate() {
            book.check_invariants()?;
            resting += book.len();
            if self.st.symbols.get(&book.symbol()) != Some(&(i as u32)) {
                return Err(format!("symbol table out of sync for {}", book.symbol()));
            }
            for side in [Side::Buy, Side::Sell] {
                for node in book.orders(side) {
                    let ok = self.st.index.get(&node.order_id).is_some_and(|loc| {
                        loc.book as usize == i && book.node(loc.slot).order_id == node.order_id
                    });
                    if !ok {
                        return Err(format!("order {} missing from index", node.order_id));
                    }
                }
            }
        }
        if resting != self.st.index.len() {
            return Err(format!(
                "index has {} orders, books have {resting}",
                self.st.index.len()
            ));
        }
        Ok(())
    }
}

impl State {
    fn book_index(&mut self, symbol: Symbol) -> usize {
        if let Some(i) = self.symbols.get(&symbol) {
            return *i as usize;
        }
        let i = self.books.len();
        self.books.push(Book::new(symbol));
        self.symbols.insert(symbol, i as u32);
        i
    }

    fn new_order(&mut self, em: &mut Emitter<'_>, o: &NewOrder, seq: Seq) {
        if o.qty == 0 || o.qty > MAX_QTY {
            return em.reject(o.order_id, RejectReason::InvalidQty);
        }
        match (o.ord_type, o.price_ticks) {
            (OrdType::Limit, None) => return em.reject(o.order_id, RejectReason::MissingPrice),
            (OrdType::Limit, Some(p)) if p <= 0 || p > MAX_PRICE_TICKS => {
                return em.reject(o.order_id, RejectReason::InvalidPrice)
            }
            (OrdType::Market, Some(_)) => {
                return em.reject(o.order_id, RejectReason::UnexpectedPrice)
            }
            _ => {}
        }
        if self.index.contains_key(&o.order_id) {
            return em.reject(o.order_id, RejectReason::DuplicateOrderId);
        }
        let bi = self.book_index(o.symbol);
        let before = self.books[bi].top();
        em.emit(EventKind::Accepted {
            order_id: o.order_id,
            account_id: o.account_id,
            symbol: o.symbol,
            side: o.side,
            ord_type: o.ord_type,
            qty: o.qty,
            price_ticks: o.price_ticks,
        });
        self.execute(
            em,
            bi,
            o.order_id,
            o.account_id,
            o.side,
            o.price_ticks,
            o.qty,
            seq,
        );
        self.emit_top_if_changed(em, bi, before);
    }

    fn cancel(&mut self, em: &mut Emitter<'_>, order_id: OrderId) {
        let Some(loc) = self.index.remove(&order_id) else {
            return em.reject(order_id, RejectReason::UnknownOrder);
        };
        let bi = loc.book as usize;
        let before = self.books[bi].top();
        let node = self.books[bi].remove(loc.slot);
        em.emit(EventKind::Canceled {
            order_id,
            symbol: self.books[bi].symbol(),
            canceled_qty: node.remaining_qty,
            reason: CancelReason::UserRequested,
        });
        self.emit_top_if_changed(em, bi, before);
    }

    fn replace(
        &mut self,
        em: &mut Emitter<'_>,
        order_id: OrderId,
        qty: Qty,
        price_ticks: Price,
        seq: Seq,
    ) {
        let Some(loc) = self.index.get(&order_id).copied() else {
            return em.reject(order_id, RejectReason::UnknownOrder);
        };
        if qty == 0 || qty > MAX_QTY {
            return em.reject(order_id, RejectReason::InvalidQty);
        }
        if price_ticks <= 0 || price_ticks > MAX_PRICE_TICKS {
            return em.reject(order_id, RejectReason::InvalidPrice);
        }
        let bi = loc.book as usize;
        let book = &mut self.books[bi];
        let symbol = book.symbol();
        let before = book.top();
        let node = *book.node(loc.slot);
        if price_ticks == node.price_ticks && qty <= node.remaining_qty {
            // Same price, size not increased: amend in place, keep the queue spot.
            if qty < node.remaining_qty {
                book.reduce(loc.slot, qty);
            }
            em.emit(EventKind::Replaced {
                order_id,
                symbol,
                qty,
                price_ticks,
                priority_retained: true,
            });
        } else {
            // Price change or size increase: the order goes to the back of the
            // queue and may trade immediately if the new price crosses.
            book.remove(loc.slot);
            self.index.remove(&order_id);
            em.emit(EventKind::Replaced {
                order_id,
                symbol,
                qty,
                price_ticks,
                priority_retained: false,
            });
            self.execute(
                em,
                bi,
                order_id,
                node.account_id,
                node.side,
                Some(price_ticks),
                qty,
                seq,
            );
        }
        self.emit_top_if_changed(em, bi, before);
    }

    /// Matches an incoming order, then rests (limit) or cancels (market) any
    /// remainder.
    #[allow(clippy::too_many_arguments)]
    fn execute(
        &mut self,
        em: &mut Emitter<'_>,
        bi: usize,
        order_id: OrderId,
        account_id: AccountId,
        side: Side,
        limit: Option<Price>,
        qty: Qty,
        seq: Seq,
    ) {
        let State {
            books,
            index,
            next_trade_id,
            ..
        } = self;
        let book = &mut books[bi];
        let symbol = book.symbol();
        let mut taker_leaves = qty;
        let remaining = book.match_incoming(side, limit, qty, &mut |f: Fill| {
            let trade_id = *next_trade_id;
            *next_trade_id += 1;
            taker_leaves -= f.qty;
            if f.maker_leaves_qty == 0 {
                index.remove(&f.maker_order_id);
            }
            em.emit(EventKind::Trade {
                trade_id,
                symbol,
                price_ticks: f.price_ticks,
                qty: f.qty,
                maker_order_id: f.maker_order_id,
                taker_order_id: order_id,
                taker_side: side,
            });
            em.emit(EventKind::Execution {
                order_id: f.maker_order_id,
                trade_id,
                role: Role::Maker,
                side: side.opposite(),
                price_ticks: f.price_ticks,
                qty: f.qty,
                leaves_qty: f.maker_leaves_qty,
            });
            em.emit(EventKind::Execution {
                order_id,
                trade_id,
                role: Role::Taker,
                side,
                price_ticks: f.price_ticks,
                qty: f.qty,
                leaves_qty: taker_leaves,
            });
        });
        if remaining > 0 {
            match limit {
                Some(price_ticks) => {
                    let slot = book.insert(order_id, account_id, side, price_ticks, remaining, seq);
                    index.insert(
                        order_id,
                        Loc {
                            book: bi as u32,
                            slot,
                        },
                    );
                }
                None => em.emit(EventKind::Canceled {
                    order_id,
                    symbol,
                    canceled_qty: remaining,
                    reason: CancelReason::NoLiquidity,
                }),
            }
        }
    }

    fn emit_top_if_changed(&self, em: &mut Emitter<'_>, bi: usize, before: Top) {
        let book = &self.books[bi];
        let after = book.top();
        if after != before {
            em.emit(EventKind::BookTop {
                symbol: book.symbol(),
                bid_px: after.bid.map(|(p, _)| p),
                bid_qty: after.bid.map_or(0, |(_, q)| q),
                ask_px: after.ask.map(|(p, _)| p),
                ask_qty: after.ask.map_or(0, |(_, q)| q),
            });
        }
    }
}
