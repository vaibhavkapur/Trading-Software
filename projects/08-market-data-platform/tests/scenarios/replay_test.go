package scenarios

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mdp/src/app"
	"mdp/src/simulator"
	"mdp/tests/harness"
)

// fixtureManifest mirrors examples/fixtures/session.manifest.json.
type fixtureManifest struct {
	Seed          uint64              `json:"seed"`
	Steps         int                 `json:"steps"`
	Start         time.Time           `json:"start"`
	Faults        []simulator.FaultAt `json:"faults"`
	Frames        int                 `json:"frames"`
	CaptureSHA256 string              `json:"capture_sha256"`
	Expected      app.ReplayResult    `json:"expected"`
}

func fixture(t *testing.T) (capture string, m fixtureManifest) {
	t.Helper()
	dir := filepath.Join(harness.Root(), "examples", "fixtures")
	b, err := os.ReadFile(filepath.Join(dir, "session.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "session.ndjson"), m
}

func TestFixtureRegeneratesBitForBit(t *testing.T) {
	capture, m := fixture(t)
	var buf bytes.Buffer
	frames, err := simulator.WriteFixture(harness.Registry(t), m.Seed, m.Steps, m.Start, m.Faults, &buf)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if got := hex.EncodeToString(sum[:]); got != m.CaptureSHA256 {
		t.Errorf("regenerated capture sha256 = %s, manifest pins %s", got, m.CaptureSHA256)
	}
	if frames != m.Frames {
		t.Errorf("regenerated %d frames, manifest says %d", frames, m.Frames)
	}
	committed, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, buf.Bytes()) {
		t.Error("the committed capture differs from what its manifest's seed and fault plan generate")
	}
}

func TestReplayIsReproducible(t *testing.T) {
	capture, m := fixture(t)
	reg := harness.Registry(t)
	for run := 1; run <= 3; run++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, store, err := app.Replay(ctx, reg, capture, harness.Logger())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res, m.Expected) {
			t.Fatalf("run %d: replay result differs from the manifest\n got: %+v\nwant: %+v", run, res, m.Expected)
		}
		// One stored row per canonical event: nothing duplicated, nothing lost.
		if got := store.Rows("trades"); got != res.Trades {
			t.Errorf("run %d: %d trades stored, %d published", run, got, res.Trades)
		}
		if got := store.Rows("quotes"); got != res.Quotes {
			t.Errorf("run %d: %d quotes stored, %d published", run, got, res.Quotes)
		}
		if got := store.Rows("depth"); got != res.Depth {
			t.Errorf("run %d: %d depth snapshots stored, %d published", run, got, res.Depth)
		}
		if got := store.Rows("gaps"); got != res.Gaps {
			t.Errorf("run %d: %d gaps stored, %d declared", run, got, res.Gaps)
		}
	}
}

// The expected output is not just pinned; it follows from the fault plan.
func TestFixtureOutputReconcilesWithItsFaultPlan(t *testing.T) {
	_, m := fixture(t)
	plan := map[string]int{}
	for _, f := range m.Faults {
		plan[f.Fault] += f.N
	}
	published := 2 * m.Steps // one event per venue per step
	exp := m.Expected

	// Frames on the wire: two login replies, every event that was neither
	// lost nor dropped, each duplicate twice, plus the garbage frames.
	if want := 2 + published - plan["lose"] - plan["drop"] + plan["duplicate"] + plan["garbage"]; m.Frames != want {
		t.Errorf("frames = %d, fault plan implies %d", m.Frames, want)
	}
	// A replay cannot ask the venue to resend, so dropped events are gaps too.
	if want := published - plan["lose"] - plan["drop"]; exp.RawEvents != want {
		t.Errorf("raw events = %d, fault plan implies %d", exp.RawEvents, want)
	}
	if want := uint64(plan["lose"] + plan["drop"]); exp.MissingSeq != want {
		t.Errorf("missing sequence numbers = %d, fault plan implies %d", exp.MissingSeq, want)
	}
	if want := uint64(plan["duplicate"]); exp.Duplicates != want {
		t.Errorf("duplicates = %d, fault plan implies %d", exp.Duplicates, want)
	}
	if exp.Rejects["UNPARSEABLE_FRAME"] != plan["garbage"] || exp.Rejects["PRICE_NOT_ON_TICK"] != plan["offtick"] {
		t.Errorf("rejects = %v, fault plan implies %d unparseable and %d off-tick", exp.Rejects, plan["garbage"], plan["offtick"])
	}
	// Every accepted event is either canonical or dead-lettered by the normalizer.
	if got, want := exp.Trades+exp.Quotes+exp.Depth, exp.RawEvents-plan["offtick"]; got != want {
		t.Errorf("canonical events = %d, want %d", got, want)
	}
}
