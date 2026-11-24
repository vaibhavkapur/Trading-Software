package unit

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"mdp/src/domain"
	"mdp/src/ingestors"
)

func parseOne(t *testing.T, p ingestors.Parser, frame string) ingestors.Event {
	t.Helper()
	evs, err := p.Parse([]byte(frame))
	if err != nil || len(evs) != 1 {
		t.Fatalf("Parse(%s) = %d events, %v", frame, len(evs), err)
	}
	return evs[0]
}

func TestAlphaParser(t *testing.T) {
	p := &ingestors.AlphaParser{}
	if evs, err := p.Parse([]byte(`{"op":"welcome","session":"A1"}`)); err != nil || evs != nil {
		t.Fatalf("welcome: %v %v", evs, err)
	}
	if evs, err := p.Parse([]byte(`{"op":"hb","ts":"2026-01-05T09:30:00Z"}`)); err != nil || evs != nil {
		t.Fatalf("heartbeat: %v %v", evs, err)
	}

	got := parseOne(t, p, `{"ch":"trade","sym":"XYZ","seq":42,"ts":"2026-01-05T09:30:00.000123456Z","px":"101.25","qty":"100","side":"B","tid":"A-7781"}`)
	want := ingestors.Event{Provider: "alpha", Session: "A1", Domain: "XYZ", Seq: 42, Kind: domain.KindTrade, Symbol: "XYZ",
		Price: "101.25", Qty: "100", TradeID: "A-7781", Side: "buy",
		EventTime: time.Date(2026, 1, 5, 9, 30, 0, 123456, time.UTC)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trade\n got %+v\nwant %+v", got, want)
	}

	// A timestamp with an offset is the same instant, reported in UTC.
	got = parseOne(t, p, `{"ch":"quote","sym":"ABC","seq":1,"ts":"2026-01-05T04:30:00-05:00","bid":"10.01","bsz":"300","ask":"10.02","asz":"200"}`)
	want = ingestors.Event{Provider: "alpha", Session: "A1", Domain: "ABC", Seq: 1, Kind: domain.KindQuote, Symbol: "ABC",
		Bid: "10.01", BidQty: "300", Ask: "10.02", AskQty: "200", EventTime: time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("quote\n got %+v\nwant %+v", got, want)
	}
	if got.EventTime.Location() != time.UTC {
		t.Error("event time should be UTC")
	}
	if s := parseOne(t, p, `{"ch":"trade","sym":"XYZ","seq":43,"ts":"2026-01-05T09:30:00Z","px":"1","qty":"1","side":"S","tid":"t"}`).Side; s != "sell" {
		t.Errorf("side S = %q", s)
	}
}

func TestAlphaParserRejectsMalformedFrames(t *testing.T) {
	welcome := `{"op":"welcome","session":"A1"}`
	cases := map[string]string{
		"not JSON":             `{"ch":"trade","sym":garbled`,
		"price as JSON number": `{"ch":"trade","sym":"XYZ","seq":1,"ts":"2026-01-05T09:30:00Z","px":101.25,"qty":"100","tid":"t"}`,
		"missing seq":          `{"ch":"trade","sym":"XYZ","ts":"2026-01-05T09:30:00Z","px":"1","qty":"1","tid":"t"}`,
		"missing symbol":       `{"ch":"trade","seq":1,"ts":"2026-01-05T09:30:00Z","px":"1","qty":"1","tid":"t"}`,
		"negative seq":         `{"ch":"trade","sym":"XYZ","seq":-1,"ts":"2026-01-05T09:30:00Z","px":"1","qty":"1","tid":"t"}`,
		"bad timestamp":        `{"ch":"trade","sym":"XYZ","seq":1,"ts":"09:30","px":"1","qty":"1","tid":"t"}`,
		"unknown channel":      `{"ch":"candle","sym":"XYZ","seq":1,"ts":"2026-01-05T09:30:00Z"}`,
		"unknown side":         `{"ch":"trade","sym":"XYZ","seq":1,"ts":"2026-01-05T09:30:00Z","px":"1","qty":"1","side":"X","tid":"t"}`,
		"unknown op":           `{"op":"logout"}`,
		"welcome sans session": `{"op":"welcome"}`,
		"empty":                ``,
	}
	for name, frame := range cases {
		p := &ingestors.AlphaParser{}
		p.Parse([]byte(welcome))
		if evs, err := p.Parse([]byte(frame)); !errors.Is(err, ingestors.ErrParse) {
			t.Errorf("%s: got %v, %v; want ErrParse", name, evs, err)
		}
	}
	// Without a session, sequence numbers cannot be interpreted.
	fresh := &ingestors.AlphaParser{}
	if _, err := fresh.Parse([]byte(`{"ch":"quote","sym":"XYZ","seq":1,"ts":"2026-01-05T09:30:00Z","bid":"1","bsz":"1"}`)); !errors.Is(err, ingestors.ErrParse) {
		t.Errorf("data before welcome: %v", err)
	}
}

func TestBetaParser(t *testing.T) {
	p := &ingestors.BetaParser{}
	for _, control := range []string{"ACK|B7|1", "H|0|1767605400000000"} {
		if evs, err := p.Parse([]byte(control)); err != nil || evs != nil {
			t.Fatalf("%s: %v %v", control, evs, err)
		}
	}
	at := time.Date(2026, 1, 5, 9, 30, 0, 123000, time.UTC)
	us := "1767605400000123"
	if at.UnixMicro() != 1767605400000123 {
		t.Fatalf("test setup: %d", at.UnixMicro())
	}

	// Prices are integers scaled by 10^4; side 2 is a resting sell.
	got := parseOne(t, p, "T|17|"+us+"|XYZ.B|750150|100|2|991")
	want := ingestors.Event{Provider: "beta", Session: "B7", Domain: "main", Seq: 17, Kind: domain.KindTrade, Symbol: "XYZ.B",
		Price: "75.015", Qty: "100", TradeID: "991", Side: "sell", EventTime: at}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trade\n got %+v\nwant %+v", got, want)
	}

	got = parseOne(t, p, "Q|18|"+us+"|LMN.B|10874|500|10876|700\r")
	want = ingestors.Event{Provider: "beta", Session: "B7", Domain: "main", Seq: 18, Kind: domain.KindQuote, Symbol: "LMN.B",
		Bid: "1.0874", BidQty: "500", Ask: "1.0876", AskQty: "700", EventTime: at}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("quote\n got %+v\nwant %+v", got, want)
	}

	// An empty price means there is no order on that side.
	got = parseOne(t, p, "Q|19|"+us+"|LMN.B||0|10876|700")
	if got.Bid != "" || got.Ask != "1.0876" {
		t.Errorf("one-sided quote: bid %q ask %q", got.Bid, got.Ask)
	}

	got = parseOne(t, p, "D|20|"+us+"|ABC.B|100100@50,100000@70|100200@10,100300@20,100400@30")
	if !reflect.DeepEqual(got.Bids, [][2]string{{"10.01", "50"}, {"10", "70"}}) ||
		!reflect.DeepEqual(got.Asks, [][2]string{{"10.02", "10"}, {"10.03", "20"}, {"10.04", "30"}}) {
		t.Errorf("depth: bids %v asks %v", got.Bids, got.Asks)
	}
	got = parseOne(t, p, "D|21|"+us+"|ABC.B||100200@10")
	if got.Bids != nil || len(got.Asks) != 1 {
		t.Errorf("one-sided depth: bids %v asks %v", got.Bids, got.Asks)
	}
}

func TestBetaParserRejectsMalformedFrames(t *testing.T) {
	cases := map[string]string{
		"unknown record":  "X|1|2|3",
		"too few fields":  "T|17|1767605400000123|XYZ.B|750150|100|2",
		"too many fields": "Q|18|1767605400000123|LMN.B|1|2|3|4|5",
		"bad seq":         "T|x|1767605400000123|XYZ.B|750150|100|2|991",
		"negative seq":    "T|-4|1767605400000123|XYZ.B|750150|100|2|991",
		"bad timestamp":   "T|17|yesterday|XYZ.B|750150|100|2|991",
		"missing symbol":  "T|17|1767605400000123||750150|100|2|991",
		"decimal price":   "T|17|1767605400000123|XYZ.B|75.015|100|2|991",
		"unknown side":    "T|17|1767605400000123|XYZ.B|750150|100|9|991",
		"bad depth level": "D|20|1767605400000123|ABC.B|100100x50|100200@10",
		"bad depth price": "D|20|1767605400000123|ABC.B|abc@50|100200@10",
		"bad ACK":         "ACK||1",
		"empty":           "",
		"binary":          "\x00\x01\x02",
	}
	for name, frame := range cases {
		p := &ingestors.BetaParser{}
		p.Parse([]byte("ACK|B7|1"))
		if evs, err := p.Parse([]byte(frame)); !errors.Is(err, ingestors.ErrParse) {
			t.Errorf("%s: got %v, %v; want ErrParse", name, evs, err)
		}
	}
	fresh := &ingestors.BetaParser{}
	if _, err := fresh.Parse([]byte("T|17|1767605400000123|XYZ.B|750150|100|2|991")); !errors.Is(err, ingestors.ErrParse) {
		t.Errorf("data before ACK: %v", err)
	}
}

func TestResumeTokens(t *testing.T) {
	for _, c := range []struct {
		session string
		seq     uint64
	}{{"A1", 41}, {"B-20260105T093000", 0}, {"with@at", 7}} {
		s, n, err := ingestors.ParseToken(ingestors.Token(c.session, c.seq))
		if err != nil || s != c.session || n != c.seq {
			t.Errorf("round trip of %s@%d gave %s@%d, %v", c.session, c.seq, s, n, err)
		}
	}
	for _, bad := range []string{"", "nosep", "S@", "S@x", "S@-1"} {
		if _, _, err := ingestors.ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) should fail", bad)
		}
	}
}

func FuzzParsersNeverPanic(f *testing.F) {
	f.Add([]byte(`{"ch":"trade","sym":"XYZ","seq":42,"ts":"2026-01-05T09:30:00Z","px":"101.25","qty":"100","side":"B","tid":"t"}`))
	f.Add([]byte("T|17|1767605400000123|XYZ.B|750150|100|2|991"))
	f.Add([]byte("D|20|1767605400000123|ABC.B|100100@50,|@"))
	f.Add([]byte("|||||||"))
	f.Fuzz(func(t *testing.T, frame []byte) {
		a := &ingestors.AlphaParser{}
		a.Parse([]byte(`{"op":"welcome","session":"A1"}`))
		if evs, err := a.Parse(frame); err != nil && !errors.Is(err, ingestors.ErrParse) || err != nil && evs != nil {
			t.Fatalf("alpha: %v %v", evs, err)
		}
		b := &ingestors.BetaParser{}
		b.Parse([]byte("ACK|B7|1"))
		if evs, err := b.Parse(frame); err != nil && !errors.Is(err, ingestors.ErrParse) || err != nil && evs != nil {
			t.Fatalf("beta: %v %v", evs, err)
		}
	})
}
