package unit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"surveillance/src/domain"
	"surveillance/src/fixed"
	"surveillance/src/normalize"
	"surveillance/tests/harness"
)

func TestPriceParsingIsExact(t *testing.T) {
	good := map[string]fixed.Price{"100": 1000000, "100.2": 1002000, "100.25": 1002500, "0.0001": 1, "9999999999.9999": 99999999999999}
	for in, want := range good {
		got, err := fixed.Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0.0000", "-1", "+1", "1e2", "1.", ".5", "1.00001", "1,000", " 1", "12345678901", "NaN", "0x10"} {
		if p, err := fixed.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d, want an error", in, p)
		}
	}
	if s := fixed.Price(1002500).String(); s != "100.2500" {
		t.Errorf("String() = %q", s)
	}
}

func TestPriceJSONNeverGoesThroughAFloat(t *testing.T) {
	// 0.1 + 0.2 style literals must come out exactly as written.
	for _, in := range []string{`"0.3"`, `0.3`, `"1234567.8901"`, `1234567.8901`} {
		var p fixed.Price
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("unmarshal %s: %v", in, err)
		}
		out, _ := json.Marshal(p)
		want := `"` + strings.Trim(in, `"`)
		if !strings.HasPrefix(string(out), want) {
			t.Errorf("%s round-tripped to %s", in, out)
		}
	}
	var p fixed.Price
	if err := json.Unmarshal([]byte(`1e2`), &p); err == nil {
		t.Error("an exponent literal was accepted")
	}
}

func normalizer(t *testing.T) *normalize.Normalizer {
	t.Helper()
	var rd normalize.RefData
	if err := json.Unmarshal([]byte(harness.RefData), &rd); err != nil {
		t.Fatal(err)
	}
	n, err := normalize.New(rd)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func decode(t *testing.T, s string) normalize.Raw {
	t.Helper()
	r, rej := normalize.Decode([]byte(s))
	if rej != nil {
		t.Fatalf("decode %s: %v", s, rej)
	}
	return r
}

func TestNormalizeCanonicalisesOrder(t *testing.T) {
	n := normalizer(t)
	recv := harness.T0.Add(time.Hour)
	// Lower-case names, aliases for venue, symbol and account, a numeric
	// price literal, and a timestamp in another zone.
	ev, rej := n.Normalize(decode(t, `{"type":"order","event_id":"m-1","venue":"sim","symbol":"sima.xsim",
		"account_id":"1001","order_id":"o-1","action":"new","side":"buy","qty":100,"price":100.25,
		"ts":"2026-01-05T20:30:00.250+05:30"}`), recv)
	if rej != nil {
		t.Fatal(rej)
	}
	want := domain.Event{ID: "ord:XSIM:M-1", Kind: domain.KindOrder, Venue: "XSIM", Symbol: "SIMA",
		EventTime: harness.At(250), ReceivedAt: recv,
		AccountID: "ACC-1001", Owner: "OWNER-A", OrderID: "O-1", Action: domain.ActionNew, Side: domain.Buy,
		Qty: 100, Price: fixed.MustParse("100.25")}
	if ev != want {
		t.Errorf("got  %+v\nwant %+v", ev, want)
	}
	if ev.EventTime.Location() != time.UTC {
		t.Error("event time is not in UTC")
	}
}

func TestNormalizeTradeResolvesOwners(t *testing.T) {
	n := normalizer(t)
	ev, rej := n.Normalize(decode(t, `{"type":"trade","venue":"XSIM","symbol":"SIMA","trade_id":"t-9",
		"buy_account":"1001","sell_account":"ACC-1002","qty":"50","price":"10.5","ts":"2026-01-05T15:00:00Z"}`), harness.T0)
	if rej != nil {
		t.Fatal(rej)
	}
	if ev.ID != "trd:XSIM:T-9" || ev.BuyAccount != "ACC-1001" || ev.SellAccount != "ACC-1002" {
		t.Errorf("unexpected trade %+v", ev)
	}
	// Two different accounts, one beneficial owner.
	if ev.BuyOwner != "OWNER-A" || ev.SellOwner != "OWNER-A" {
		t.Errorf("owners = %q, %q", ev.BuyOwner, ev.SellOwner)
	}
	// An account the reference data does not know is its own owner.
	ev, _ = n.Normalize(decode(t, `{"type":"trade","venue":"XSIM","symbol":"SIMA","trade_id":"t-10",
		"buy_account":"zed","sell_account":"1001","qty":1,"price":1,"ts":"2026-01-05T15:00:00Z"}`), harness.T0)
	if ev.BuyAccount != "ZED" || ev.BuyOwner != "ZED" {
		t.Errorf("unknown account: %+v", ev)
	}
}

func TestNormalizeRejectsWithStableCodes(t *testing.T) {
	n := normalizer(t)
	base := map[string]any{"type": "order", "venue": "XSIM", "symbol": "SIMA", "account_id": "A", "order_id": "o",
		"action": "NEW", "side": "BUY", "qty": 10, "price": "1.00", "ts": "2026-01-05T15:00:00Z"}
	cases := []struct {
		name string
		edit func(m map[string]any)
		code string
	}{
		{"no type", func(m map[string]any) { delete(m, "type") }, normalize.CodeMissingField},
		{"unknown type", func(m map[string]any) { m["type"] = "quote" }, normalize.CodeUnknownType},
		{"unregistered venue", func(m map[string]any) { m["venue"] = "XBAD" }, normalize.CodeUnknownVenue},
		{"no symbol", func(m map[string]any) { m["symbol"] = " " }, normalize.CodeMissingField},
		{"no account", func(m map[string]any) { delete(m, "account_id") }, normalize.CodeMissingField},
		{"no order id", func(m map[string]any) { delete(m, "order_id") }, normalize.CodeMissingField},
		{"bad action", func(m map[string]any) { m["action"] = "REPLACE" }, normalize.CodeInvalidAction},
		{"no action", func(m map[string]any) { delete(m, "action") }, normalize.CodeMissingField},
		{"bad side", func(m map[string]any) { m["side"] = "SHORT" }, normalize.CodeInvalidSide},
		{"new without side", func(m map[string]any) { delete(m, "side") }, normalize.CodeMissingField},
		{"fractional qty", func(m map[string]any) { m["qty"] = 10.5 }, normalize.CodeInvalidQty},
		{"zero qty", func(m map[string]any) { m["qty"] = 0 }, normalize.CodeInvalidQty},
		{"negative qty", func(m map[string]any) { m["qty"] = -5 }, normalize.CodeInvalidQty},
		{"no qty", func(m map[string]any) { delete(m, "qty") }, normalize.CodeInvalidQty},
		{"zero price", func(m map[string]any) { m["price"] = "0" }, normalize.CodeInvalidPrice},
		{"five decimals", func(m map[string]any) { m["price"] = "1.00001" }, normalize.CodeInvalidPrice},
		{"no price", func(m map[string]any) { delete(m, "price") }, normalize.CodeInvalidPrice},
		{"no timestamp", func(m map[string]any) { delete(m, "ts") }, normalize.CodeInvalidTS},
		{"timestamp without zone", func(m map[string]any) { m["ts"] = "2026-01-05T15:00:00" }, normalize.CodeInvalidTS},
		{"epoch number as timestamp", func(m map[string]any) { m["ts"] = "1767625200" }, normalize.CodeInvalidTS},
		{"absurd year", func(m map[string]any) { m["ts"] = "3026-01-05T15:00:00Z" }, normalize.CodeInvalidTS},
		{"id with a space", func(m map[string]any) { m["order_id"] = "o 1" }, normalize.CodeInvalidField},
		{"id too long", func(m map[string]any) { m["order_id"] = strings.Repeat("x", 65) }, normalize.CodeInvalidField},
	}
	for _, c := range cases {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		c.edit(m)
		b, _ := json.Marshal(m)
		raw, rej := normalize.Decode(b)
		if rej == nil {
			_, rej = n.Normalize(raw, harness.T0)
		}
		if rej == nil || rej.Code != c.code {
			t.Errorf("%s: got %v, want %s", c.name, rej, c.code)
		}
	}
}

func TestDecodeIsStrict(t *testing.T) {
	for _, in := range []string{
		`{"type":"order","acount_id":"A"}`, // misspelt field
		`{"type":"order"} {"type":"order"}`,
		`[1,2]`,
		`not json`,
	} {
		if _, rej := normalize.Decode([]byte(in)); rej == nil || rej.Code != normalize.CodeMalformed {
			t.Errorf("Decode(%s) = %v, want %s", in, rej, normalize.CodeMalformed)
		}
	}
}

func TestCancelMayOmitQtyPriceAndSide(t *testing.T) {
	n := normalizer(t)
	ev, rej := n.Normalize(decode(t, `{"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o",
		"action":"CANCEL","ts":"2026-01-05T15:00:00Z"}`), harness.T0)
	if rej != nil {
		t.Fatal(rej)
	}
	if ev.Qty != 0 || ev.Price != 0 || ev.Side != "" {
		t.Errorf("unexpected cancel %+v", ev)
	}
	// But what it does state must still be valid.
	_, rej = n.Normalize(decode(t, `{"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o",
		"action":"CANCEL","qty":1.5,"ts":"2026-01-05T15:00:00Z"}`), harness.T0)
	if rej == nil || rej.Code != normalize.CodeInvalidQty {
		t.Errorf("got %v, want %s", rej, normalize.CodeInvalidQty)
	}
}

func TestEventIdentity(t *testing.T) {
	n := normalizer(t)
	msg := `{"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o","action":"NEW","side":"BUY","qty":10,"price":"1.00","ts":"2026-01-05T15:00:00Z"}`
	a, _ := n.Normalize(decode(t, msg), harness.T0)
	// The same message received later is the same event...
	b, _ := n.Normalize(decode(t, msg), harness.T0.Add(time.Minute))
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("redelivery changed the ID: %q vs %q", a.ID, b.ID)
	}
	// ...however it was spelt...
	c, _ := n.Normalize(decode(t, strings.NewReplacer(`"XSIM"`, `"sim"`, `"1.00"`, `1.0`).Replace(msg)), harness.T0)
	if c.ID != a.ID {
		t.Errorf("equivalent spelling changed the ID: %q vs %q", c.ID, a.ID)
	}
	// ...and any change of content is a different event.
	d, _ := n.Normalize(decode(t, strings.Replace(msg, `"qty":10`, `"qty":11`, 1)), harness.T0)
	if d.ID == a.ID {
		t.Error("different content produced the same ID")
	}
	// The same source ID on two venues is two events.
	x, _ := n.Normalize(decode(t, strings.Replace(msg, `{`, `{"event_id":"7",`, 1)), harness.T0)
	y, _ := n.Normalize(decode(t, strings.NewReplacer(`{"type"`, `{"event_id":"7","type"`, `"XSIM"`, `"XALT"`).Replace(msg)), harness.T0)
	if x.ID == y.ID {
		t.Errorf("source IDs collided across venues: %q", x.ID)
	}
}

func TestRefDataRejectsAmbiguousAlias(t *testing.T) {
	_, err := normalize.New(normalize.RefData{Accounts: map[string]normalize.AccountRef{
		"ACC-1": {Aliases: []string{"shared"}}, "ACC-2": {Aliases: []string{"SHARED"}}}})
	if err == nil {
		t.Error("an alias claimed by two accounts was accepted")
	}
}
