package simulator

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdp/src/fixed"
	"mdp/src/ingestors"
	"mdp/src/refdata"
)

// Wire formats a venue can speak.
const (
	WireAlpha = "alpha"
	WireBeta  = "beta"
)

type instrument struct {
	native string
	tickE4 int64 // tick size in units of 0.0001
	lot    fixed.Unit
}

type message struct {
	domain string
	seq    uint64
	frame  []byte
}

// Venue sequences ticks, encodes them in one wire format, keeps a bounded
// resend buffer, and fans frames out to subscribers.
type Venue struct {
	wire string
	inst []instrument

	mu        sync.Mutex
	session   string
	seqs      map[string]uint64
	buf       []message // resend buffer, oldest first
	bufCap    int
	subs      map[*subscriber]struct{}
	published uint64

	// faults
	dropNext    int
	loseNext    int
	dupNext     int
	garbageNext int
	offTickNext int
	staleNext   int
	staleAge    time.Duration
	skew        time.Duration
	paused      bool
}

// NewVenue creates a venue for a provider's instruments. Tick sizes must be
// multiples of 0.0001, the finest price both wire formats can carry.
func NewVenue(wire, session string, reg *refdata.Registry, provider string, resendBuffer int) (*Venue, error) {
	prov, ok := reg.Provider(provider)
	if !ok {
		return nil, fmt.Errorf("simulator: unknown provider %q", provider)
	}
	if wire != WireAlpha && wire != WireBeta {
		return nil, fmt.Errorf("simulator: unknown wire format %q", wire)
	}
	e4, _ := fixed.ParseUnit("0.0001")
	v := &Venue{wire: wire, session: session, seqs: map[string]uint64{}, bufCap: resendBuffer, subs: map[*subscriber]struct{}{}}
	for _, sym := range reg.Symbols(provider) {
		in, _ := reg.Lookup(prov.Venue, sym)
		tickE4, err := e4.Units(in.TickSize)
		if err != nil {
			return nil, fmt.Errorf("simulator: %s tick size %s: %w", in.ID, in.TickSize, err)
		}
		v.inst = append(v.inst, instrument{native: sym, tickE4: tickE4, lot: in.Lot})
	}
	if len(v.inst) == 0 {
		return nil, fmt.Errorf("simulator: provider %q has no instruments", provider)
	}
	return v, nil
}

// Symbols is how many instruments the venue lists.
func (v *Venue) Symbols() int { return len(v.inst) }

// Published is how many sequence numbers the venue has consumed.
func (v *Venue) Published() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.published
}

// Subscribers is how many clients are connected.
func (v *Venue) Subscribers() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.subs)
}

// Fault injection. Each call arms a fault for the next n published events.

// DropNext loses the next n events on the wire. They stay in the resend
// buffer, so a client that asks again gets them: a recoverable gap.
func (v *Venue) DropNext(n int) { v.set(func() { v.dropNext += n }) }

// LoseNext consumes n sequence numbers without sending or buffering
// anything: a gap no resend can fill.
func (v *Venue) LoseNext(n int) { v.set(func() { v.loseNext += n }) }

// DuplicateNext sends each of the next n events twice.
func (v *Venue) DuplicateNext(n int) { v.set(func() { v.dupNext += n }) }

// GarbageNext sends a malformed frame before each of the next n events.
func (v *Venue) GarbageNext(n int) { v.set(func() { v.garbageNext += n }) }

// OffTickNext publishes the next n trades at a price between two ticks.
func (v *Venue) OffTickNext(n int) { v.set(func() { v.offTickNext += n }) }

// StaleNext stamps the next n events with an event time age in the past.
func (v *Venue) StaleNext(n int, age time.Duration) {
	v.set(func() { v.staleNext, v.staleAge = v.staleNext+n, age })
}

// SetSkew shifts the venue clock: positive runs it ahead of true time.
func (v *Venue) SetSkew(d time.Duration) { v.set(func() { v.skew = d }) }

// Pause stops the venue publishing, so its feed goes quiet.
func (v *Venue) Pause(paused bool) { v.set(func() { v.paused = paused }) }

// Disconnect drops every client connection.
func (v *Venue) Disconnect() {
	v.set(func() {
		for s := range v.subs {
			s.close()
		}
		v.subs = map[*subscriber]struct{}{}
	})
}

// NewSession starts a new session: sequence numbers restart at 1, the
// resend buffer is discarded and clients are disconnected.
func (v *Venue) NewSession(session string) {
	v.set(func() {
		v.session, v.seqs, v.buf = session, map[string]uint64{}, nil
		for s := range v.subs {
			s.close()
		}
		v.subs = map[*subscriber]struct{}{}
	})
}

func (v *Venue) set(f func()) {
	v.mu.Lock()
	f()
	v.mu.Unlock()
}

func (v *Venue) domainOf(sym int) string {
	if v.wire == WireBeta {
		return ingestors.BetaDomain
	}
	return v.inst[sym].native
}

// Publish sequences one tick and sends it to subscribers.
func (v *Venue) Publish(t Tick) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.paused {
		return
	}
	ts := t.EventTime.Add(v.skew)
	if v.staleNext > 0 {
		v.staleNext--
		ts = ts.Add(-v.staleAge)
	}
	dom := v.domainOf(t.Sym)
	v.seqs[dom]++
	v.published++
	if v.loseNext > 0 {
		v.loseNext--
		return
	}
	offTick := false
	// Only instruments whose tick is coarser than the wire's finest price
	// step can be quoted between two ticks.
	if v.offTickNext > 0 && t.Kind == "trade" && v.inst[t.Sym].tickE4 > 1 {
		v.offTickNext--
		offTick = true
	}
	msg := message{domain: dom, seq: v.seqs[dom], frame: v.encode(t, v.seqs[dom], ts, offTick)}
	if v.bufCap > 0 {
		if len(v.buf) >= v.bufCap {
			v.buf = v.buf[1:]
		}
		v.buf = append(v.buf, msg)
	}
	if v.garbageNext > 0 {
		v.garbageNext--
		v.fanout(message{domain: dom, frame: []byte(`{"ch":"trade","sym":garbled`)})
	}
	if v.dropNext > 0 {
		v.dropNext--
		return
	}
	v.fanout(msg)
	if v.dupNext > 0 {
		v.dupNext--
		v.fanout(msg)
	}
}

// fanout delivers to every subscriber. A subscriber that cannot keep up is
// disconnected, as real venues do with slow consumers.
func (v *Venue) fanout(m message) {
	for s := range v.subs {
		if s.want != nil && !s.want[m.domain] {
			continue
		}
		if !s.deliver(m.frame) {
			s.close()
			delete(v.subs, s)
		}
	}
}

func (v *Venue) encode(t Tick, seq uint64, ts time.Time, offTick bool) []byte {
	in := v.inst[t.Sym]
	e4 := func(ticks int64) int64 { return ticks * in.tickE4 }
	px := e4(t.PriceTicks)
	if offTick {
		px++ // 0.0001 above a valid tick
	}
	if v.wire == WireAlpha {
		stamp := ts.UTC().Format(time.RFC3339Nano)
		dec := func(n int64) string { return fixed.FromScaled(n, 4) }
		switch t.Kind {
		case "trade":
			side := "S"
			if t.BuyAggressor {
				side = "B"
			}
			return fmt.Appendf(nil, `{"ch":"trade","sym":%q,"seq":%d,"ts":%q,"px":%q,"qty":%q,"side":%q,"tid":"A-%d"}`,
				in.native, seq, stamp, dec(px), in.lot.Format(t.Lots), side, t.TradeNo)
		default:
			return fmt.Appendf(nil, `{"ch":"quote","sym":%q,"seq":%d,"ts":%q,"bid":%q,"bsz":%q,"ask":%q,"asz":%q}`,
				in.native, seq, stamp, dec(e4(t.BidTicks)), in.lot.Format(t.BidLots), dec(e4(t.AskTicks)), in.lot.Format(t.AskLots))
		}
	}
	us := ts.UnixMicro()
	switch t.Kind {
	case "trade":
		resting := "1" // beta reports the resting side: a buy aggressor hit a resting sell
		if t.BuyAggressor {
			resting = "2"
		}
		return fmt.Appendf(nil, "T|%d|%d|%s|%d|%s|%s|%d", seq, us, in.native, px, in.lot.Format(t.Lots), resting, t.TradeNo)
	case "depth":
		side := func(ls []Level) string {
			parts := make([]string, len(ls))
			for i, l := range ls {
				parts[i] = strconv.FormatInt(e4(l.Ticks), 10) + "@" + in.lot.Format(l.Lots)
			}
			return strings.Join(parts, ",")
		}
		return fmt.Appendf(nil, "D|%d|%d|%s|%s|%s", seq, us, in.native, side(t.Bids), side(t.Asks))
	default:
		return fmt.Appendf(nil, "Q|%d|%d|%s|%d|%s|%d|%s", seq, us, in.native,
			e4(t.BidTicks), in.lot.Format(t.BidLots), e4(t.AskTicks), in.lot.Format(t.AskLots))
	}
}

// subscriber is one connected client.
type subscriber struct {
	want    map[string]bool // sequence domains wanted; nil means all
	deliver func(frame []byte) bool
	done    chan struct{}
	once    sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.done) }) }

// subscribe registers a client and returns the session plus the frames it
// missed. resume maps a sequence domain to the client's resume token; frames
// after that position are resent if the token belongs to the current session
// and the frames are still buffered. Registration and the backlog snapshot
// are atomic, so the client sees every later frame exactly once.
func (v *Venue) subscribe(s *subscriber, resume map[string]string) (session string, backlog [][]byte, next uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	after := map[string]uint64{}
	for dom, tok := range resume {
		if sess, seq, err := ingestors.ParseToken(tok); err == nil && sess == v.session {
			after[dom] = seq
		}
	}
	for _, m := range v.buf {
		if s.want != nil && !s.want[m.domain] {
			continue
		}
		if seq, ok := after[m.domain]; ok && m.seq > seq {
			backlog = append(backlog, m.frame)
		}
	}
	v.subs[s] = struct{}{}
	return v.session, backlog, v.seqs[ingestors.BetaDomain] + 1
}

func (v *Venue) unsubscribe(s *subscriber) {
	v.mu.Lock()
	delete(v.subs, s)
	v.mu.Unlock()
}

// lastSeq is the latest sequence number of a domain, for heartbeats.
func (v *Venue) lastSeq(domain string) uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seqs[domain]
}

// Hello is the first frame a client of this venue receives.
func (v *Venue) hello(session string, next uint64) []byte {
	if v.wire == WireAlpha {
		return fmt.Appendf(nil, `{"op":"welcome","session":%q}`, session)
	}
	return fmt.Appendf(nil, "ACK|%s|%d", session, next)
}

// Tap attaches a synchronous in-process listener that receives the hello
// frame and then every frame the venue sends. It is how fixtures are
// generated without sockets.
func (v *Venue) Tap(f func(frame []byte)) {
	s := &subscriber{done: make(chan struct{}), deliver: func(frame []byte) bool { f(frame); return true }}
	session, _, next := v.subscribe(s, nil)
	f(v.hello(session, next))
}
