package unit

import (
	"reflect"
	"strings"
	"testing"

	"mdp/src/domain"
	"mdp/src/refdata"
	"mdp/tests/harness"
)

func TestExampleReferenceData(t *testing.T) {
	reg := harness.Registry(t)
	alpha, ok := reg.Provider("alpha")
	if !ok || alpha.Venue != "venueA" || alpha.SeqScope != "symbol" || alpha.TradeSide != "aggressor" || alpha.SeqStart != 1 {
		t.Errorf("alpha: %+v", alpha)
	}
	beta, ok := reg.Provider("beta")
	if !ok || beta.Venue != "venueB" || beta.SeqScope != "feed" || beta.TradeSide != "resting" {
		t.Errorf("beta: %+v", beta)
	}
	if _, ok := reg.Provider("gamma"); ok {
		t.Error("unknown provider found")
	}
	if got := reg.Symbols("alpha"); !reflect.DeepEqual(got, []string{"ABC", "QRS", "XYZ"}) {
		t.Errorf("alpha symbols %v", got)
	}
	if got := reg.InVenue("venueB"); !reflect.DeepEqual(got, []string{"venueB:ABC.B", "venueB:LMN.B", "venueB:XYZ.B"}) {
		t.Errorf("venueB instruments %v", got)
	}
	in, ok := reg.Lookup("venueB", "XYZ.B")
	if !ok || in.ID != "venueB:XYZ.B" || in.TickSize != "0.005" || in.Tick.String() != "0.005" || in.Lot.String() != "1" {
		t.Errorf("lookup: %+v", in)
	}
	if _, ok := reg.Lookup("venueA", "XYZ.B"); ok {
		t.Error("a native symbol resolves only within its own venue")
	}
	if got, ok := reg.Get("venueA:QRS"); !ok || got.LotSize != "0.001" {
		t.Errorf("get: %+v", got)
	}
	all := reg.All()
	if len(all) != 6 || all[0].ID != "venueA:ABC" || all[5].ID != "venueB:XYZ.B" {
		t.Errorf("all: %v", all)
	}
	all[0].ID = "mutated"
	if reg.All()[0].ID != "venueA:ABC" {
		t.Error("All must return a copy")
	}
	if got := reg.Search("xyz"); len(got) != 2 {
		t.Errorf("search xyz: %v", got)
	}
	if got := reg.Search("VENUEB:"); len(got) != 3 {
		t.Errorf("search is case-insensitive: %v", got)
	}
	if got := reg.Search("zzz"); got == nil || len(got) != 0 {
		t.Errorf("search without matches should be empty, not nil: %v", got)
	}
}

func TestReferenceDataIsValidated(t *testing.T) {
	prov := refdata.Provider{Provider: "p", Venue: "v", SeqScope: "symbol", TradeSide: "aggressor"}
	inst := domain.Instrument{ID: "v:S", Venue: "v", NativeSymbol: "S", TickSize: "0.01", LotSize: "1", Currency: "USD"}
	with := func(f func(*refdata.Provider, *domain.Instrument)) refdata.File {
		p, i := prov, inst
		f(&p, &i)
		return refdata.File{Providers: []refdata.Provider{p}, Instruments: []domain.Instrument{i}}
	}
	if _, err := refdata.New(with(func(*refdata.Provider, *domain.Instrument) {})); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	cases := map[string]refdata.File{
		"needs provider and venue":    with(func(p *refdata.Provider, _ *domain.Instrument) { p.Venue = "" }),
		"seq_scope must be":           with(func(p *refdata.Provider, _ *domain.Instrument) { p.SeqScope = "venue" }),
		"trade_side must be":          with(func(p *refdata.Provider, _ *domain.Instrument) { p.TradeSide = "" }),
		"must be venue:native_symbol": with(func(_ *refdata.Provider, i *domain.Instrument) { i.ID = "S" }),
		"no provider serves venue":    with(func(_ *refdata.Provider, i *domain.Instrument) { i.ID, i.Venue = "w:S", "w" }),
		"tick_size":                   with(func(_ *refdata.Provider, i *domain.Instrument) { i.TickSize = "0" }),
		"lot_size":                    with(func(_ *refdata.Provider, i *domain.Instrument) { i.LotSize = "one" }),
		"duplicate provider":          {Providers: []refdata.Provider{prov, prov}},
		"duplicate instrument":        {Providers: []refdata.Provider{prov}, Instruments: []domain.Instrument{inst, inst}},
	}
	for want, f := range cases {
		if _, err := refdata.New(f); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	if _, err := refdata.Load("does-not-exist.json"); err == nil {
		t.Error("loading a missing file should fail")
	}
}
