package ingestors

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"mdp/src/domain"
)

// Alpha is the adapter for the simulated "alpha" feed.
//
// Wire: JSON text frames over WebSocket, one message per frame.
//
//	client → {"op":"subscribe","symbols":["XYZ"],"resume":{"XYZ":"A1@41"}}
//	server → {"op":"welcome","session":"A1"}
//	server → {"op":"hb","ts":"…"}
//	server → {"ch":"trade","sym":"XYZ","seq":42,"ts":"2026-01-05T09:30:00.000123456Z",
//	          "px":"101.25","qty":"100","side":"B","tid":"A-7781"}
//	server → {"ch":"quote","sym":"XYZ","seq":43,"ts":"…","bid":"101.24","bsz":"300","ask":"101.26","asz":"200"}
//
// Sequence numbers are per symbol. Prices and sizes are decimal strings.
// Timestamps are RFC 3339 with nanoseconds. "side" is the aggressor: B or S.
type Alpha struct {
	URL     string
	Symbols []string
}

const ProviderAlpha = "alpha"

func (a *Alpha) Provider() string  { return ProviderAlpha }
func (a *Alpha) Source() string    { return domain.SourceLive }
func (a *Alpha) Live() bool        { return true }
func (a *Alpha) NewParser() Parser { return &AlphaParser{} }

// Domains: alpha numbers each symbol separately.
func (a *Alpha) Domains() []string { return a.Symbols }

func (a *Alpha) Dial(ctx context.Context, resume []domain.FeedOffset) (Conn, error) {
	c, _, err := websocket.Dial(ctx, a.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("alpha: dial: %w", err)
	}
	c.SetReadLimit(1 << 20)
	// Each symbol resumes from its own "<session>@<seq>" token; the provider
	// ignores tokens from a session that has ended.
	sub := struct {
		Op      string            `json:"op"`
		Symbols []string          `json:"symbols"`
		Resume  map[string]string `json:"resume,omitempty"`
	}{Op: "subscribe", Symbols: a.Symbols, Resume: map[string]string{}}
	for _, off := range resume {
		sub.Resume[off.Partition] = off.ResumeToken
	}
	b, _ := json.Marshal(sub)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		c.CloseNow()
		return nil, fmt.Errorf("alpha: subscribe: %w", err)
	}
	return &wsConn{c: c}, nil
}

type wsConn struct{ c *websocket.Conn }

func (w *wsConn) ReadFrame(ctx context.Context) (Frame, error) {
	_, b, err := w.c.Read(ctx)
	return Frame{Data: b}, err
}
func (w *wsConn) Close() error { return w.c.CloseNow() }

// AlphaParser decodes alpha frames.
type AlphaParser struct{ session string }

type alphaMsg struct {
	Op      string  `json:"op"`
	Ch      string  `json:"ch"`
	Session string  `json:"session"`
	Sym     string  `json:"sym"`
	Seq     *uint64 `json:"seq"`
	TS      string  `json:"ts"`
	Px      string  `json:"px"`
	Qty     string  `json:"qty"`
	Side    string  `json:"side"`
	TID     string  `json:"tid"`
	Bid     string  `json:"bid"`
	Bsz     string  `json:"bsz"`
	Ask     string  `json:"ask"`
	Asz     string  `json:"asz"`
}

func (p *AlphaParser) Parse(frame []byte) ([]Event, error) {
	var m alphaMsg
	// Prices are declared as strings, so a provider sending JSON numbers
	// (binary floating point) fails here instead of being silently accepted.
	if err := json.Unmarshal(frame, &m); err != nil {
		return nil, parseErr("alpha: %v", err)
	}
	switch m.Op {
	case "welcome":
		if m.Session == "" {
			return nil, parseErr("alpha: welcome without session")
		}
		p.session = m.Session
		return nil, nil
	case "hb":
		return nil, nil
	case "":
	default:
		return nil, parseErr("alpha: unknown op %q", m.Op)
	}
	if p.session == "" {
		return nil, parseErr("alpha: data before welcome")
	}
	if m.Sym == "" || m.Seq == nil {
		return nil, parseErr("alpha: missing sym or seq")
	}
	ts, err := time.Parse(time.RFC3339Nano, m.TS)
	if err != nil {
		return nil, parseErr("alpha: bad ts %q", m.TS)
	}
	ev := Event{
		Provider: ProviderAlpha, Session: p.session, Domain: m.Sym, Seq: *m.Seq,
		Symbol: m.Sym, EventTime: ts.UTC(),
	}
	switch m.Ch {
	case "trade":
		ev.Kind = domain.KindTrade
		ev.Price, ev.Qty, ev.TradeID = m.Px, m.Qty, m.TID
		switch m.Side {
		case "B":
			ev.Side = "buy"
		case "S":
			ev.Side = "sell"
		case "":
		default:
			return nil, parseErr("alpha: unknown side %q", m.Side)
		}
	case "quote":
		ev.Kind = domain.KindQuote
		ev.Bid, ev.BidQty, ev.Ask, ev.AskQty = m.Bid, m.Bsz, m.Ask, m.Asz
	default:
		return nil, parseErr("alpha: unknown channel %q", m.Ch)
	}
	return []Event{ev}, nil
}
