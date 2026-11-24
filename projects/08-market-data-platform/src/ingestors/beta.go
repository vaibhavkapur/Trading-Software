package ingestors

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"mdp/src/domain"
	"mdp/src/fixed"
)

// Beta is the adapter for the simulated "beta" feed.
//
// Wire: newline-terminated, pipe-delimited text over TCP.
//
//	client → LOGIN|<session or empty>|<last seq seen>
//	server → ACK|<session>|<next seq>
//	server → H|<last seq sent>|<ts_us>
//	server → T|seq|ts_us|sym|px_e4|qty|side|match_id
//	server → Q|seq|ts_us|sym|bid_e4|bid_qty|ask_e4|ask_qty
//	server → D|seq|ts_us|sym|px_e4@qty,px_e4@qty,…|px_e4@qty,…      (bids|asks, best first)
//
// One sequence counter covers the whole feed. Prices are integers scaled by
// 10^4. Timestamps are epoch microseconds. Trade "side" is the resting
// order's side: 1 = buy, 2 = sell. An empty quote price means no order on
// that side.
type Beta struct {
	Addr string
}

const (
	ProviderBeta = "beta"
	// BetaDomain is beta's single sequence domain.
	BetaDomain     = "main"
	betaPriceScale = 4
)

func (b *Beta) Provider() string  { return ProviderBeta }
func (b *Beta) Source() string    { return domain.SourceLive }
func (b *Beta) Live() bool        { return true }
func (b *Beta) NewParser() Parser { return &BetaParser{} }

// Domains: beta numbers the whole feed as one sequence.
func (b *Beta) Domains() []string { return []string{BetaDomain} }

func (b *Beta) Dial(ctx context.Context, resume []domain.FeedOffset) (Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", b.Addr)
	if err != nil {
		return nil, fmt.Errorf("beta: dial: %w", err)
	}
	session, last := "", uint64(0)
	for _, off := range resume {
		if off.Partition != BetaDomain {
			continue
		}
		if s, seq, err := ParseToken(off.ResumeToken); err == nil {
			session, last = s, seq
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetWriteDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "LOGIN|%s|%d\n", session, last); err != nil {
		c.Close()
		return nil, fmt.Errorf("beta: login: %w", err)
	}
	c.SetWriteDeadline(time.Time{})
	return &lineConn{c: c, r: bufio.NewReaderSize(c, 64<<10)}, nil
}

type lineConn struct {
	c net.Conn
	r *bufio.Reader
}

func (l *lineConn) ReadFrame(ctx context.Context) (Frame, error) {
	if dl, ok := ctx.Deadline(); ok {
		l.c.SetReadDeadline(dl)
	} else {
		l.c.SetReadDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { l.c.SetReadDeadline(time.Now()) })
	defer stop()
	line, err := l.r.ReadSlice('\n')
	if err != nil {
		if ctx.Err() != nil {
			return Frame{}, ctx.Err()
		}
		return Frame{}, err
	}
	// ReadSlice's buffer is reused by the next read.
	out := make([]byte, len(line)-1)
	copy(out, line)
	return Frame{Data: out}, nil
}
func (l *lineConn) Close() error { return l.c.Close() }

// BetaParser decodes beta lines.
type BetaParser struct{ session string }

func (p *BetaParser) Parse(frame []byte) ([]Event, error) {
	f := strings.Split(strings.TrimRight(string(frame), "\r"), "|")
	switch f[0] {
	case "ACK":
		if len(f) != 3 || f[1] == "" {
			return nil, parseErr("beta: bad ACK")
		}
		p.session = f[1]
		return nil, nil
	case "H":
		return nil, nil
	case "T", "Q", "D":
	default:
		return nil, parseErr("beta: unknown record type %q", f[0])
	}
	if p.session == "" {
		return nil, parseErr("beta: data before ACK")
	}
	want := map[string]int{"T": 8, "Q": 8, "D": 6}[f[0]]
	if len(f) != want {
		return nil, parseErr("beta: %s record has %d fields, want %d", f[0], len(f), want)
	}
	seq, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return nil, parseErr("beta: bad seq %q", f[1])
	}
	us, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return nil, parseErr("beta: bad timestamp %q", f[2])
	}
	if f[3] == "" {
		return nil, parseErr("beta: missing symbol")
	}
	ev := Event{
		Provider: ProviderBeta, Session: p.session, Domain: BetaDomain, Seq: seq,
		Symbol: f[3], EventTime: time.UnixMicro(us).UTC(),
	}
	switch f[0] {
	case "T":
		ev.Kind = domain.KindTrade
		if ev.Price, err = betaPrice(f[4]); err != nil {
			return nil, err
		}
		ev.Qty, ev.TradeID = f[5], f[7]
		switch f[6] {
		case "1":
			ev.Side = "buy"
		case "2":
			ev.Side = "sell"
		case "":
		default:
			return nil, parseErr("beta: unknown side %q", f[6])
		}
	case "Q":
		ev.Kind = domain.KindQuote
		if ev.Bid, err = betaPrice(f[4]); err != nil {
			return nil, err
		}
		if ev.Ask, err = betaPrice(f[6]); err != nil {
			return nil, err
		}
		ev.BidQty, ev.AskQty = f[5], f[7]
	case "D":
		ev.Kind = domain.KindDepth
		if ev.Bids, err = betaLevels(f[4]); err != nil {
			return nil, err
		}
		if ev.Asks, err = betaLevels(f[5]); err != nil {
			return nil, err
		}
	}
	return []Event{ev}, nil
}

// betaPrice converts a 10^4-scaled integer price to an exact decimal string.
// An empty field stays empty (no price on that side).
func betaPrice(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "", parseErr("beta: bad price %q", s)
	}
	return fixed.FromScaled(n, betaPriceScale), nil
}

func betaLevels(s string) ([][2]string, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([][2]string, 0, len(parts))
	for _, part := range parts {
		px, qty, ok := strings.Cut(part, "@")
		if !ok || px == "" {
			return nil, parseErr("beta: bad level %q", part)
		}
		price, err := betaPrice(px)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]string{price, qty})
	}
	return out, nil
}
