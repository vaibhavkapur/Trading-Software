package unit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mdp/src/app"
	"mdp/src/simulator"
	"mdp/tests/harness"
)

func TestMarketIsDeterministicAndWellFormed(t *testing.T) {
	now := time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)
	a, b, c := simulator.NewMarket(7, 3, true), simulator.NewMarket(7, 3, true), simulator.NewMarket(8, 3, true)
	same, kinds := true, map[string]int{}
	var lastTrade uint64
	for i := 0; i < 5000; i++ {
		ta, tb, tc := a.Next(now), b.Next(now), c.Next(now)
		if !reflect.DeepEqual(ta, tb) {
			t.Fatalf("tick %d differs between two markets with the same seed", i)
		}
		if !reflect.DeepEqual(ta, tc) {
			same = false
		}
		kinds[ta.Kind]++
		if ta.Sym < 0 || ta.Sym >= 3 {
			t.Fatalf("symbol index %d", ta.Sym)
		}
		switch ta.Kind {
		case "trade":
			if ta.PriceTicks <= 0 || ta.Lots <= 0 || ta.TradeNo != lastTrade+1 {
				t.Fatalf("trade %+v after trade number %d", ta, lastTrade)
			}
			lastTrade = ta.TradeNo
		case "quote":
			if ta.BidTicks <= 0 || ta.BidTicks >= ta.AskTicks || ta.BidLots <= 0 || ta.AskLots <= 0 {
				t.Fatalf("quote %+v", ta)
			}
		case "depth":
			if len(ta.Bids) != 5 || len(ta.Asks) != 5 || ta.Bids[0].Ticks >= ta.Asks[0].Ticks {
				t.Fatalf("depth %+v", ta)
			}
			for k := 1; k < 5; k++ {
				if ta.Bids[k].Ticks >= ta.Bids[k-1].Ticks || ta.Asks[k].Ticks <= ta.Asks[k-1].Ticks {
					t.Fatalf("depth levels out of order: %+v", ta)
				}
			}
		default:
			t.Fatalf("kind %q", ta.Kind)
		}
	}
	if same {
		t.Error("different seeds produced the same market")
	}
	if kinds["trade"] == 0 || kinds["quote"] == 0 || kinds["depth"] == 0 {
		t.Errorf("kinds %v", kinds)
	}
	noDepth := simulator.NewMarket(7, 3, false)
	for i := 0; i < 2000; i++ {
		if noDepth.Next(now).Kind == "depth" {
			t.Fatal("a market without depth produced a depth event")
		}
	}
}

func TestFixtureGenerationIsDeterministic(t *testing.T) {
	reg := harness.Registry(t)
	start := time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)
	gen := func(seed uint64, faults []simulator.FaultAt) ([]byte, int) {
		var buf bytes.Buffer
		frames, err := simulator.WriteFixture(reg, seed, 300, start, faults, &buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf.Bytes(), frames
	}
	a, framesA := gen(3, simulator.DefaultFaults()[:2])
	b, _ := gen(3, simulator.DefaultFaults()[:2])
	if !bytes.Equal(a, b) {
		t.Error("the same arguments must generate the same bytes")
	}
	if c, _ := gen(4, simulator.DefaultFaults()[:2]); bytes.Equal(a, c) {
		t.Error("a different seed generated the same capture")
	}
	clean, frames := gen(3, nil)
	if frames != 2+2*300 {
		t.Errorf("a clean capture has a login reply per venue plus one frame per event: got %d", frames)
	}
	if framesA != frames { // duplicate faults at steps 500 and 900 are beyond 300 steps
		t.Errorf("faults scheduled after the last step changed the capture: %d vs %d frames", framesA, frames)
	}

	if _, err := simulator.WriteFixture(reg, 1, 10, start, []simulator.FaultAt{{Step: 1, Venue: "gamma", Fault: "drop"}}, &bytes.Buffer{}); err == nil {
		t.Error("a fault on an unknown venue should be an error")
	}
	if _, err := simulator.WriteFixture(reg, 1, 10, start, []simulator.FaultAt{{Step: 1, Venue: "alpha", Fault: "explode"}}, &bytes.Buffer{}); err == nil {
		t.Error("an unknown fault should be an error")
	}

	// A clean capture replays to exactly its events: nothing missing, repeated or rejected.
	path := filepath.Join(t.TempDir(), "clean.ndjson")
	if err := os.WriteFile(path, clean, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _, err := app.Replay(ctx, reg, path, harness.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if res.RawEvents != 600 || res.Trades+res.Quotes+res.Depth != 600 || res.Gaps != 0 || res.Duplicates != 0 || len(res.Rejects) != 0 {
		t.Errorf("clean replay: %+v", res)
	}
}
