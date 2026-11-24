package scenarios

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"surveillance/src/domain"
	"surveillance/src/engine"
	"surveillance/src/evaluation"
	"surveillance/src/rules"
	h "surveillance/tests/harness"
)

var update = flag.Bool("update", false, "re-record testdata/session.golden")

const (
	fixture = "testdata/session.jsonl"
	golden  = "testdata/session.golden"
)

// The fixture is generated, not hand-written:
//
//	go run ./src/cmd/simgen -seed 7 -n 21 -out tests/scenarios/testdata/session.jsonl \
//	    -labels tests/scenarios/testdata/session.labels.json
func loadFixture(t *testing.T) []json.RawMessage {
	t.Helper()
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var items []json.RawMessage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		items = append(items, json.RawMessage(append([]byte(nil), sc.Bytes()...)))
	}
	if err := sc.Err(); err != nil || len(items) == 0 {
		t.Fatalf("fixture: %d events, %v", len(items), err)
	}
	return items
}

// replay feeds the fixture through an engine in batches of the given size.
func replay(t *testing.T, e *h.Env, items []json.RawMessage, batch int, restartEvery int) {
	t.Helper()
	for i, n := 0, 0; i < len(items); i, n = i+batch, n+1 {
		res, err := e.Engine.IngestJSON(items[i:min(i+batch, len(items))])
		if err != nil || len(res.Rejected) != 0 || res.Duplicates != 0 {
			t.Fatalf("fixture batch at %d: %+v %v", i, res, err)
		}
		if restartEvery > 0 && n%restartEvery == restartEvery-1 {
			e.Restart()
		}
	}
	e.Flush()
}

func keys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// transcript renders what an analyst would be shown, in a stable order.
func transcript(e *h.Env) string {
	st := e.Engine.Stats()
	alerts := e.Alerts()
	sort.SliceStable(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if !a.WindowStart.Equal(b.WindowStart) {
			return a.WindowStart.Before(b.WindowStart)
		}
		return a.ID < b.ID
	})
	var b strings.Builder
	fmt.Fprintf(&b, "ruleset %s\nevents %d (orders %d, trades %d, late %d)\nalerts %d from %d detections (%d folded)\n",
		st.Ruleset.Version, st.Events.Total, st.Events.Orders, st.Events.Trades, st.Events.Late,
		st.Alerts.Total, st.Alerts.Detections, st.Alerts.Suppressed)
	const clock = "15:04:05.000"
	for _, a := range alerts {
		refs := sha256.Sum256([]byte(strings.Join(a.EventRefs, "\n")))
		fmt.Fprintf(&b, "\n%s %s v%d %s\n  accounts %s in %s, %s..%s, occurrences %d\n  evidence %d events (sha256 %s), trigger %s\n",
			a.ID, a.RuleID, a.RuleVersion, a.Severity, strings.Join(a.Accounts, ","), a.Symbol,
			a.WindowStart.Format(clock), a.WindowEnd.Format(clock), a.Occurrences,
			len(a.EventRefs), hex.EncodeToString(refs[:6]), a.TriggerEventID)
		b.WriteString("  observed  ")
		for _, k := range keys(a.Observed) {
			fmt.Fprintf(&b, " %s=%v", k, a.Observed[k])
		}
		b.WriteString("\n  thresholds")
		for _, k := range keys(a.Thresholds) {
			fmt.Fprintf(&b, " %s=%v", k, a.Thresholds[k])
		}
		fmt.Fprintf(&b, "\n  %s\n", a.Explanation)
	}
	return b.String()
}

// The same recorded session must always produce the same alerts, to the
// byte: same IDs, same evidence, same numbers, same wording.
func TestRecordedSessionReplaysIdentically(t *testing.T) {
	items := loadFixture(t)
	e := h.New(t)
	replay(t, e, items, 100, 0)
	got := transcript(e)

	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("re-recorded %s", golden)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (record it with: make golden)", err)
	}
	if got != string(want) {
		t.Fatalf("replay differs from %s. If the rule change is intended, run `make golden` and review the diff.\n--- got\n%s", golden, got)
	}

	// Batch boundaries and restarts are not inputs: any way of feeding
	// the same events gives the same alerts.
	full := picture(e.Alerts())
	for _, tc := range []struct{ batch, restartEvery int }{{1, 0}, {977, 0}, {37, 3}} {
		other := h.New(t)
		replay(t, other, items, tc.batch, tc.restartEvery)
		if picture(other.Alerts()) != full {
			t.Errorf("batch size %d, restart every %d batches: alerts differ", tc.batch, tc.restartEvery)
		}
	}
	// And the engine's own journal replays to the same place.
	e.Restart()
	if transcript(e) != got {
		t.Error("the transcript changed across a restart")
	}
}

// Definition of done: every alert carries evidence event IDs that resolve,
// window bounds that contain them, the rule version and the thresholds.
func TestEveryAlertCarriesItsEvidence(t *testing.T) {
	scs, err := evaluation.Build(evaluation.Options{Seed: 11, Scenarios: 105})
	if err != nil {
		t.Fatal(err)
	}
	e := h.New(t)
	stream := evaluation.Stream(scs, 11, 25)
	e.MustAccept(stream...)
	e.Flush()

	alerts := e.Alerts()
	seen := map[string]bool{}
	byRule := map[string]int{}
	cfg := h.DefaultRules()
	versions := map[string]int{rules.HighCancelRate: cfg.Rules.HighCancelRate.Version, rules.HighOrderTradeRatio: cfg.Rules.HighOrderTradeRatio.Version,
		rules.LayeringLike: cfg.Rules.LayeringLike.Version, rules.WashTradeLike: cfg.Rules.WashTradeLike.Version}
	for _, a := range alerts {
		byRule[a.RuleID]++
		if seen[a.ID] || !strings.HasPrefix(a.ID, "alr-") {
			t.Errorf("alert ID %q repeated or malformed", a.ID)
		}
		seen[a.ID] = true
		if a.RuleVersion != versions[a.RuleID] || a.RulesetVersion != "test-1" || a.RulesetHash != cfg.Hash() {
			t.Errorf("%s: rule version %d, ruleset %s %s", a.ID, a.RuleVersion, a.RulesetVersion, a.RulesetHash)
		}
		if len(a.Thresholds) == 0 || len(a.Observed) == 0 || a.Explanation == "" || a.Disclaimer != domain.Disclaimer {
			t.Errorf("%s: thresholds %v observed %v explanation %q", a.ID, a.Thresholds, a.Observed, a.Explanation)
		}
		if domain.SeverityRank(a.Severity) == 0 || a.Occurrences < 1 || a.DedupKey == "" || len(a.Accounts) == 0 {
			t.Errorf("%s: severity %q occurrences %d key %q accounts %v", a.ID, a.Severity, a.Occurrences, a.DedupKey, a.Accounts)
		}
		if !a.WindowStart.Before(a.WindowEnd) && !a.WindowStart.Equal(a.WindowEnd) {
			t.Errorf("%s: window %v..%v", a.ID, a.WindowStart, a.WindowEnd)
		}
		d, err := e.Engine.Alert(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.EventRefs) == 0 || len(d.Events) != len(a.EventRefs) || len(d.MissingEventRefs) != 0 || a.EvidenceTruncated {
			t.Errorf("%s: %d refs, %d resolved, missing %v, truncated %v", a.ID, len(a.EventRefs), len(d.Events), d.MissingEventRefs, a.EvidenceTruncated)
		}
		trigger := false
		for _, ev := range d.Events {
			trigger = trigger || ev.ID == a.TriggerEventID
			if ev.EventTime.Before(a.WindowStart) || ev.EventTime.After(a.WindowEnd) {
				t.Errorf("%s: evidence event %s at %v is outside the window %v..%v", a.ID, ev.ID, ev.EventTime, a.WindowStart, a.WindowEnd)
			}
			if ev.Symbol != a.Symbol || ev.Late {
				t.Errorf("%s: evidence event %s is in %s (late: %v)", a.ID, ev.ID, ev.Symbol, ev.Late)
			}
			involved := false
			for _, acct := range a.Accounts {
				involved = involved || ev.AccountID == acct || ev.BuyAccount == acct || ev.SellAccount == acct
			}
			if !involved {
				t.Errorf("%s: evidence event %s involves none of %v", a.ID, ev.ID, a.Accounts)
			}
		}
		if !trigger {
			t.Errorf("%s: trigger event %s is not among the evidence", a.ID, a.TriggerEventID)
		}
		// No alert wording asserts intent or wrongdoing.
		low := strings.ToLower(a.Explanation)
		for _, word := range []string{"manipulat", "spoof", "fraud", "illegal", "guilty", "abuse", "violation"} {
			if strings.Contains(low, word) {
				t.Errorf("%s: explanation uses %q: %s", a.ID, word, a.Explanation)
			}
		}
	}
	for _, id := range rules.RuleIDs {
		if byRule[id] < 5 {
			t.Errorf("only %d %s alerts in the set; the check above proves little about that rule", byRule[id], id)
		}
	}
	if st := e.Engine.Stats(); st.Events.Late != 0 || st.Alerts.Suppressed == 0 {
		t.Errorf("late %d, suppressed %d", st.Events.Late, st.Alerts.Suppressed)
	}
}

// Non-goal: a single unusual order or trade is never enough for an alert.
func TestOneUnusualEventIsNeverEnough(t *testing.T) {
	e := h.New(t)
	// One trade with the same account on both sides.
	e.MustAccept(selfTrades("S1", h.At(0), 1)...)
	// One trade between two accounts of the same beneficial owner.
	e.MustAccept(h.Trade("ACC-1001", "", "ACC-1002", "", 500, "20.00", h.At(1000)))
	// One enormous order, cancelled at once.
	e.MustAccept(h.NewOrder("S2", "big", "SELL", 900000000, "20.00", h.At(2000)), h.Cancel("S2", "big", h.At(2001)))
	// One large resting order pulled right after a small opposite fill.
	e.MustAccept(h.Layering("S3", h.At(3000), 1, 50000, 100, 1, 100*1e6)...)
	// One position passed across and straight back.
	e.MustAccept(
		h.Trade("S4", "", "S5", "", 400, "20.00", h.At(4000)),
		h.Trade("S5", "", "S4", "", 400, "20.00", h.At(5000)))
	e.Flush()
	if got := e.Alerts(); len(got) != 0 {
		t.Errorf("alerts raised on single events: %+v", got)
	}

	// And no ruleset that would allow it is accepted.
	for name, edit := range map[string]func(*rules.Config){
		"one self-trade":   func(c *rules.Config) { c.Rules.WashTradeLike.MinSelfTrades = 1 },
		"one paired trade": func(c *rules.Config) { c.Rules.WashTradeLike.MinPairTrades = 1 },
		"one layer": func(c *rules.Config) {
			c.Rules.LayeringLike.MinLayerOrders = 1
			c.Rules.LayeringLike.MinPriceLevels = 1
		},
		"one order":   func(c *rules.Config) { c.Rules.HighCancelRate.MinNewOrders = 1 },
		"one message": func(c *rules.Config) { c.Rules.HighOrderTradeRatio.MinMessages = 1 },
	} {
		c := h.DefaultRules()
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("a ruleset firing on %s was accepted", name)
		}
	}
}

// The synthetic controls give a repeatable false-positive estimate, and
// that estimate comes from the same engine everything else uses.
func TestSyntheticControlsEstimateFalsePositives(t *testing.T) {
	o := evaluation.Options{Seed: 42, Scenarios: 210, RulesPath: "../../examples/rules.json", DisorderPct: 20}
	rep, err := evaluation.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Negatives != 90 || rep.HardNegatives+rep.OrdinaryNegatives != 90 || rep.HardNegatives == 0 || rep.OrdinaryNegatives == 0 {
		t.Fatalf("negative controls: %d (%d hard, %d ordinary)", rep.Negatives, rep.HardNegatives, rep.OrdinaryNegatives)
	}
	flagged := 0
	for _, out := range rep.Outcomes {
		if out.Label == "negative" && len(out.Fired) > 0 {
			flagged++
		}
		if (out.Verdict == "FP") != (out.Label == "negative" && len(out.Fired) > 0) {
			t.Errorf("%s seed %d: verdict %s with fired %v", out.Name, out.Seed, out.Verdict, out.Fired)
		}
	}
	if flagged != rep.NegativesFlagged || rep.FalsePositiveRate != float64(flagged)/90 {
		t.Errorf("flagged %d, reported %d, rate %v", flagged, rep.NegativesFlagged, rep.FalsePositiveRate)
	}
	// The hard negatives are there to be hard. If none trips a rule the
	// controls are too easy to estimate anything; if the ordinary ones
	// trip often, a rule is broken.
	if rep.HardFlagged == 0 {
		t.Error("no hard negative raised an alert: the controls are not testing the rules")
	}
	if rep.OrdinaryFlagged*5 > rep.OrdinaryNegatives {
		t.Errorf("%d of %d ordinary negatives raised an alert", rep.OrdinaryFlagged, rep.OrdinaryNegatives)
	}
	// The report never goes out without its caveats.
	var text bytes.Buffer
	rep.WriteText(&text)
	for _, want := range []string{"synthetic", "Limitations", "not ground truth"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the text report does not mention %q", want)
		}
	}
	js, _ := json.Marshal(rep)
	if !bytes.Contains(js, []byte(`"limitations":["Every event is synthetic`)) {
		t.Error("the JSON report does not carry the limitations")
	}
	_ = engine.ErrJournal
}
