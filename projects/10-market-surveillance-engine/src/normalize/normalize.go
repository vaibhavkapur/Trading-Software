// Package normalize is the event-normalizer: it turns whatever a source
// sent into one canonical record, or refuses it with a stable reason code.
//
// It is deliberately stateless. Anything that needs order history (which
// side a bare cancel belongs to, say) is resolved downstream, after events
// have been put back into event-time order.
package normalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"surveillance/src/domain"
	"surveillance/src/fixed"
)

// Reject codes. They are part of the API: clients and dashboards key on them.
const (
	CodeMalformed     = "MALFORMED_EVENT"
	CodeUnknownType   = "UNKNOWN_EVENT_TYPE"
	CodeMissingField  = "MISSING_FIELD"
	CodeInvalidField  = "INVALID_FIELD"
	CodeInvalidTS     = "INVALID_TIMESTAMP"
	CodeInvalidPrice  = "INVALID_PRICE"
	CodeInvalidQty    = "INVALID_QTY"
	CodeInvalidAction = "INVALID_ACTION"
	CodeInvalidSide   = "INVALID_SIDE"
	CodeUnknownVenue  = "UNKNOWN_VENUE"
	CodeFutureTS      = "FUTURE_TIMESTAMP"
)

type Reject struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (r *Reject) Error() string { return r.Code + ": " + r.Detail }

func reject(code, format string, a ...any) *Reject {
	return &Reject{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// Lit keeps the literal text of a JSON number or string, so that prices
// and quantities are parsed from what was sent rather than from a float.
type Lit string

func (l *Lit) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*l = Lit(s)
		return nil
	}
	if string(b) == "null" {
		*l = ""
		return nil
	}
	*l = Lit(b)
	return nil
}

func (l Lit) MarshalJSON() ([]byte, error) { return json.Marshal(string(l)) }

// Raw is the wire format accepted by POST /v1/events.
type Raw struct {
	Type    string `json:"type"` // "order" or "trade"
	EventID string `json:"event_id,omitempty"`
	Venue   string `json:"venue"`
	Symbol  string `json:"symbol"`
	TS      string `json:"ts"` // RFC 3339 with an explicit offset

	AccountID string `json:"account_id,omitempty"`
	OrderID   string `json:"order_id,omitempty"`
	Action    string `json:"action,omitempty"`
	Side      string `json:"side,omitempty"`

	TradeID     string `json:"trade_id,omitempty"`
	BuyAccount  string `json:"buy_account,omitempty"`
	SellAccount string `json:"sell_account,omitempty"`
	BuyOrderID  string `json:"buy_order_id,omitempty"`
	SellOrderID string `json:"sell_order_id,omitempty"`

	Qty   Lit `json:"qty,omitempty"`
	Price Lit `json:"price,omitempty"`
}

// Decode parses one event strictly: unknown fields are an error, because a
// misspelt field silently dropped is missing data nobody notices.
func Decode(b []byte) (Raw, *Reject) {
	var r Raw
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, reject(CodeMalformed, "%v", err)
	}
	if dec.More() {
		return r, reject(CodeMalformed, "trailing data after event")
	}
	return r, nil
}

// RefData maps the names sources use onto canonical ones.
type RefData struct {
	// Venues is the list of registered sources. An event from a venue not
	// listed here is refused unless AllowUnknownVenues is set.
	Venues             map[string]Aliases    `json:"venues"`
	AllowUnknownVenues bool                  `json:"allow_unknown_venues"`
	Accounts           map[string]AccountRef `json:"accounts"`
	Symbols            map[string]Aliases    `json:"symbols"`
}

type Aliases struct {
	Aliases []string `json:"aliases"`
}

type AccountRef struct {
	Aliases []string `json:"aliases"`
	// Owner groups accounts under one beneficial owner. Trades between two
	// accounts of the same owner are treated as trades with oneself.
	Owner string `json:"owner"`
}

func LoadRefData(path string) (RefData, error) {
	var rd RefData
	b, err := os.ReadFile(path)
	if err != nil {
		return rd, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rd); err != nil {
		return rd, fmt.Errorf("reference data %s: %w", path, err)
	}
	return rd, nil
}

type Normalizer struct {
	venues       map[string]string
	allowUnknown bool
	accounts     map[string]string
	owners       map[string]string
	symbols      map[string]string
}

// New builds the lookup tables. Two canonical names claiming the same
// alias is a configuration error, not something to resolve by map order.
func New(rd RefData) (*Normalizer, error) {
	n := &Normalizer{
		venues: map[string]string{}, accounts: map[string]string{},
		owners: map[string]string{}, symbols: map[string]string{},
		allowUnknown: rd.AllowUnknownVenues || len(rd.Venues) == 0,
	}
	add := func(kind string, table map[string]string, canonical string, aliases []string) error {
		c := canon(canonical)
		if c == "" {
			return fmt.Errorf("reference data: empty %s name", kind)
		}
		for _, a := range append([]string{canonical}, aliases...) {
			k := canon(a)
			if prev, ok := table[k]; ok && prev != c {
				return fmt.Errorf("reference data: %s alias %q maps to both %s and %s", kind, a, prev, c)
			}
			table[k] = c
		}
		return nil
	}
	for name, v := range rd.Venues {
		if err := add("venue", n.venues, name, v.Aliases); err != nil {
			return nil, err
		}
	}
	for name, s := range rd.Symbols {
		if err := add("symbol", n.symbols, name, s.Aliases); err != nil {
			return nil, err
		}
	}
	for name, a := range rd.Accounts {
		if err := add("account", n.accounts, name, a.Aliases); err != nil {
			return nil, err
		}
		if a.Owner != "" {
			n.owners[canon(name)] = canon(a.Owner)
		}
	}
	return n, nil
}

func canon(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

const maxIDLen = 64

// ident canonicalizes an identifier and checks it is short and printable.
func ident(field, v string, required bool) (string, *Reject) {
	c := canon(v)
	if c == "" {
		if required {
			return "", reject(CodeMissingField, "%s is required", field)
		}
		return "", nil
	}
	if len(c) > maxIDLen {
		return "", reject(CodeInvalidField, "%s is longer than %d characters", field, maxIDLen)
	}
	for _, r := range c {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' || r == '|' {
			return "", reject(CodeInvalidField, "%s contains a character that is not allowed", field)
		}
	}
	return c, nil
}

var (
	minTime = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ParseTime requires an explicit UTC offset. A timestamp without one is
// ambiguous, and guessing the zone would silently shift every window.
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("ts is required")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, errors.New("ts must be RFC 3339 with an explicit offset, e.g. 2026-01-05T15:00:00.250Z")
	}
	t = t.UTC()
	if t.Before(minTime) || !t.Before(maxTime) {
		return time.Time{}, errors.New("ts is outside the supported range")
	}
	return t, nil
}

const maxQty = 1_000_000_000_000

func parseQty(l Lit) (int64, error) {
	s := string(l)
	if s == "" {
		return 0, errors.New("qty is required")
	}
	if len(s) > 13 {
		return 0, errors.New("qty is out of range")
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("qty must be a whole number of units")
		}
		v = v*10 + int64(c-'0')
	}
	if v <= 0 || v > maxQty {
		return 0, errors.New("qty must be between 1 and 1e12")
	}
	return v, nil
}

// Normalize validates one raw event and returns its canonical form.
// receivedAt is the local clock reading; it never influences detection.
func (n *Normalizer) Normalize(r Raw, receivedAt time.Time) (domain.Event, *Reject) {
	ev := domain.Event{ReceivedAt: receivedAt.UTC()}

	switch strings.ToLower(strings.TrimSpace(r.Type)) {
	case "order":
		ev.Kind = domain.KindOrder
	case "trade":
		ev.Kind = domain.KindTrade
	case "":
		return ev, reject(CodeMissingField, "type is required")
	default:
		return ev, reject(CodeUnknownType, "type %q is not one of order, trade", r.Type)
	}

	venue, rej := ident("venue", r.Venue, true)
	if rej != nil {
		return ev, rej
	}
	if v, ok := n.venues[venue]; ok {
		venue = v
	} else if !n.allowUnknown {
		return ev, reject(CodeUnknownVenue, "venue %q is not registered", r.Venue)
	}
	ev.Venue = venue

	symbol, rej := ident("symbol", r.Symbol, true)
	if rej != nil {
		return ev, rej
	}
	if s, ok := n.symbols[symbol]; ok {
		symbol = s
	}
	ev.Symbol = symbol

	t, err := ParseTime(strings.TrimSpace(r.TS))
	if err != nil {
		return ev, reject(CodeInvalidTS, "%v", err)
	}
	ev.EventTime = t

	if ev.Kind == domain.KindOrder {
		return n.order(ev, r)
	}
	return n.trade(ev, r)
}

func (n *Normalizer) account(field, v string) (id, owner string, rej *Reject) {
	id, rej = ident(field, v, true)
	if rej != nil {
		return "", "", rej
	}
	if c, ok := n.accounts[id]; ok {
		id = c
	}
	owner = id
	if o, ok := n.owners[id]; ok {
		owner = o
	}
	return id, owner, nil
}

func (n *Normalizer) order(ev domain.Event, r Raw) (domain.Event, *Reject) {
	var rej *Reject
	if ev.AccountID, ev.Owner, rej = n.account("account_id", r.AccountID); rej != nil {
		return ev, rej
	}
	if ev.OrderID, rej = ident("order_id", r.OrderID, true); rej != nil {
		return ev, rej
	}
	switch domain.Action(canon(r.Action)) {
	case domain.ActionNew:
		ev.Action = domain.ActionNew
	case domain.ActionAmend:
		ev.Action = domain.ActionAmend
	case domain.ActionCancel:
		ev.Action = domain.ActionCancel
	case "":
		return ev, reject(CodeMissingField, "action is required")
	default:
		return ev, reject(CodeInvalidAction, "action %q is not one of NEW, AMEND, CANCEL", r.Action)
	}
	switch domain.Side(canon(r.Side)) {
	case domain.Buy:
		ev.Side = domain.Buy
	case domain.Sell:
		ev.Side = domain.Sell
	case "":
		// Venues often omit the side on amends and cancels; it is filled
		// in from the order's history where that is known.
		if ev.Action == domain.ActionNew {
			return ev, reject(CodeMissingField, "side is required on a NEW order")
		}
	default:
		return ev, reject(CodeInvalidSide, "side %q is not one of BUY, SELL", r.Side)
	}

	// A cancel may omit quantity (cancel everything left) and price.
	if ev.Action != domain.ActionCancel || r.Qty != "" {
		q, err := parseQty(r.Qty)
		if err != nil {
			return ev, reject(CodeInvalidQty, "%v", err)
		}
		ev.Qty = q
	}
	if ev.Action != domain.ActionCancel || r.Price != "" {
		p, err := fixed.Parse(string(r.Price))
		if err != nil {
			return ev, reject(CodeInvalidPrice, "%v", err)
		}
		ev.Price = p
	}

	if src, rej := ident("event_id", r.EventID, false); rej != nil {
		return ev, rej
	} else if src != "" {
		ev.ID = "ord:" + ev.Venue + ":" + src
	} else {
		// No source ID: identify the message by its content, so that a
		// redelivery of the same message is recognised as a duplicate.
		h := sha256.Sum256([]byte(strings.Join([]string{
			ev.Venue, ev.Symbol, ev.AccountID, ev.OrderID, string(ev.Action), string(ev.Side),
			fmt.Sprint(ev.Qty), fmt.Sprint(int64(ev.Price)), fmt.Sprint(ev.TS()),
		}, "|")))
		ev.ID = "ord:" + ev.Venue + ":h" + hex.EncodeToString(h[:10])
	}
	return ev, nil
}

func (n *Normalizer) trade(ev domain.Event, r Raw) (domain.Event, *Reject) {
	var rej *Reject
	if ev.TradeID, rej = ident("trade_id", r.TradeID, true); rej != nil {
		return ev, rej
	}
	if ev.BuyAccount, ev.BuyOwner, rej = n.account("buy_account", r.BuyAccount); rej != nil {
		return ev, rej
	}
	if ev.SellAccount, ev.SellOwner, rej = n.account("sell_account", r.SellAccount); rej != nil {
		return ev, rej
	}
	if ev.BuyOrderID, rej = ident("buy_order_id", r.BuyOrderID, false); rej != nil {
		return ev, rej
	}
	if ev.SellOrderID, rej = ident("sell_order_id", r.SellOrderID, false); rej != nil {
		return ev, rej
	}
	q, err := parseQty(r.Qty)
	if err != nil {
		return ev, reject(CodeInvalidQty, "%v", err)
	}
	ev.Qty = q
	p, err := fixed.Parse(string(r.Price))
	if err != nil {
		return ev, reject(CodeInvalidPrice, "%v", err)
	}
	ev.Price = p
	// A venue's trade ID identifies the execution, so it is the event ID.
	ev.ID = "trd:" + ev.Venue + ":" + ev.TradeID
	return ev, nil
}
