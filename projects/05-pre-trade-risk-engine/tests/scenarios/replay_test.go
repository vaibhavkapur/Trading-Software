package scenarios

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"riskengine/src/audit"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/tests/harness"
)

var update = flag.Bool("update", false, "rewrite testdata/session.golden from the current behaviour")

type step struct {
	Op        string             `json:"op"`
	Ms        int64              `json:"ms"`
	Symbol    string             `json:"symbol"`
	Bid       string             `json:"bid"`
	Ask       string             `json:"ask"`
	Order     domain.RiskRequest `json:"order"`
	OrderID   string             `json:"order_id"`
	FillID    string             `json:"fill_id"`
	Qty       int64              `json:"qty"`
	Price     string             `json:"price"`
	By        string             `json:"by"`
	Scope     domain.KillScope   `json:"scope"`
	Target    string             `json:"target"`
	Enabled   bool               `json:"enabled"`
	AccountID string             `json:"account_id"`
	Positions map[string]int64   `json:"positions"`
}

func outcome(res domain.RiskReservation, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("%s remaining=%d", res.Status, res.Remaining)
}

// runSession feeds the recorded session to a fresh engine and returns a
// transcript of every result plus the final audit head hash.
func runSession(t *testing.T) (transcript []string, head string) {
	t.Helper()
	e := harness.New(t)
	f, err := os.Open("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		var s step
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("session.jsonl:%d: %v", line, err)
		}
		eng := e.App.Engine
		var out string
		switch s.Op {
		case "advance":
			e.Clock.Advance(time.Duration(s.Ms) * time.Millisecond)
			continue
		case "quote":
			e.Quote(s.Symbol, s.Bid, s.Ask)
			continue
		case "restart":
			e.Restart()
			out = fmt.Sprintf("replayed %d audit records", e.App.Recovered)
		case "evaluate":
			d := eng.Evaluate(s.Order)
			out = fmt.Sprintf("%q %s %v", d.OrderID, d.Decision, d.ReasonCodes)
			if d.Replayed {
				out += " (replayed)"
			}
		case "commit":
			out = s.OrderID + " " + outcome(eng.Commit(s.OrderID))
		case "approve":
			out = s.OrderID + " " + outcome(eng.Approve(s.OrderID, s.By))
		case "release":
			out = s.OrderID + " " + outcome(eng.Release(s.OrderID, ""))
		case "fill":
			res, err := eng.Fill(s.OrderID, s.FillID, s.Qty, fixed.MustParse(s.Price))
			if err != nil {
				t.Fatalf("session.jsonl:%d: %v", line, err)
			}
			st := eng.AccountState(eng.OrderAccount(s.OrderID))
			out = fmt.Sprintf("%s/%s position=%d realized_pnl=%s unreserved=%v", s.OrderID, s.FillID, res.Position.Qty, st.RealizedPnLToday, res.Unreserved)
		case "kill":
			_, _, _, err := eng.SetKillSwitch(s.Scope, s.Target, s.Enabled, s.By, "")
			out = fmt.Sprintf("%s:%s enabled=%v err=%v", s.Scope, s.Target, s.Enabled, err)
		case "sweep":
			out = fmt.Sprintf("released %d expired", eng.Sweep())
		case "reconcile":
			rec, _ := eng.Reconcile(s.AccountID, s.Positions)
			out = fmt.Sprintf("%s abs_drift=%d %v", s.AccountID, rec.AbsDrift, rec.Drift)
		default:
			t.Fatalf("session.jsonl:%d: unknown op %q", line, s.Op)
		}
		transcript = append(transcript, fmt.Sprintf("%02d %-9s %s", line, s.Op, out))
	}
	n, head, err := audit.Verify(e.AuditPath(), harness.Key().Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return append(transcript, fmt.Sprintf("audit records=%d head=%s", n, head)), head
}

// Replay: a fixed session, policy, clock and signing key must produce the
// same decisions and a bit-identical audit log every time. The golden file
// pins both, so any behavioural change to a rule shows up as a diff here.
func TestRecordedSessionReplaysIdentically(t *testing.T) {
	first, head1 := runSession(t)
	_, head2 := runSession(t)
	if head1 != head2 {
		t.Fatalf("two runs of the same session produced different audit logs: %s vs %s", head1, head2)
	}
	got := strings.Join(first, "\n") + "\n"
	const golden = "testdata/session.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `make golden` to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("session no longer replays to the recorded outcome.\n--- got\n%s--- want\n%s"+
			"If the change is intended, review the diff and run `make golden`.", got, want)
	}
}
