package unit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"surveillance/src/cases"
	"surveillance/src/domain"
	"surveillance/src/journal"
	"surveillance/src/rules"
	"surveillance/tests/harness"
)

// ---- ruleset ----

func TestExampleRulesetLoads(t *testing.T) {
	c, err := rules.Load("../../examples/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	// The shipped example and the test defaults are the same thresholds.
	want := harness.DefaultRules()
	want.Version = c.Version
	if c != want {
		t.Errorf("examples/rules.json drifted from the test defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestRulesetIsStrict(t *testing.T) {
	good, _ := json.Marshal(harness.DefaultRules())
	if _, err := rules.Parse(good); err != nil {
		t.Fatalf("the default ruleset does not parse: %v", err)
	}
	bad := map[string]func(*rules.Config){
		"no version":                   func(c *rules.Config) { c.Version = "" },
		"window not a multiple of hop": func(c *rules.Config) { c.Window.SizeS, c.Window.HopS = 60, 7 },
		"zero hop":                     func(c *rules.Config) { c.Window.HopS = 0 },
		"negative lateness":            func(c *rules.Config) { c.Window.AllowedLatenessMs = -1 },
		"cancel pct over 100":          func(c *rules.Config) { c.Rules.LayeringLike.MinCancelPct = 101 },
		"zero min orders":              func(c *rules.Config) { c.Rules.HighCancelRate.MinNewOrders = 0 },
		"rule version zero":            func(c *rules.Config) { c.Rules.WashTradeLike.Version = 0 },
		"severity steps out of order":  func(c *rules.Config) { c.Rules.HighOrderTradeRatio.HighMinRatio = 30 },
		"more levels than orders":      func(c *rules.Config) { c.Rules.LayeringLike.MinPriceLevels = 9 },
		// A single unusual trade must never be enough to raise an alert.
		"one self trade is enough": func(c *rules.Config) { c.Rules.WashTradeLike.MinSelfTrades = 1 },
		"one layer is enough":      func(c *rules.Config) { c.Rules.LayeringLike.MinLayerOrders = 1 },
		"too few evidence refs":    func(c *rules.Config) { c.Suppression.MaxEvidenceRefs = 0 },
	}
	for name, edit := range bad {
		c := harness.DefaultRules()
		edit(&c)
		b, _ := json.Marshal(c)
		if _, err := rules.Parse(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A misspelt key must fail the file, not silently fall back to zero.
	typo := strings.Replace(string(good), `"min_cancel_rate_pct"`, `"min_cancel_rate_pc"`, 1)
	if _, err := rules.Parse([]byte(typo)); err == nil {
		t.Error("a ruleset with an unknown field was accepted")
	}
	if _, err := rules.Parse(append(good, []byte(` {}`)...)); err == nil {
		t.Error("trailing data after the ruleset was accepted")
	}
}

func TestRulesetHashFollowsValuesNotFormatting(t *testing.T) {
	c := harness.DefaultRules()
	compact, _ := json.Marshal(c)
	pretty, _ := json.MarshalIndent(c, "    ", "\t")
	a, _ := rules.Parse(compact)
	b, _ := rules.Parse(pretty)
	if a.Hash() != b.Hash() || len(a.Hash()) != 64 {
		t.Errorf("formatting changed the hash: %s vs %s", a.Hash(), b.Hash())
	}
	c.Rules.HighCancelRate.MinCancelRatePct = 91
	if c.Hash() == a.Hash() {
		t.Error("changing a threshold did not change the hash")
	}
}

// ---- journal ----

func openJournal(t *testing.T, path string) (*journal.Journal, []journal.Record) {
	t.Helper()
	j, err := journal.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	var recs []journal.Record
	if err := j.Scan(func(_ int64, r journal.Record) error { recs = append(recs, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return j, recs
}

func eventRecord(n uint64) journal.Record {
	ev := order("A", "o", domain.ActionNew, int(n)*1000)
	ev.Seq = n
	return journal.Record{Kind: journal.KindEvent, Seq: n, At: harness.T0, Event: &ev}
}

func TestJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j, recs := openJournal(t, path)
	if len(recs) != 0 {
		t.Fatalf("new journal has %d records", len(recs))
	}
	var offs []int64
	for n := uint64(1); n <= 3; n++ {
		off, err := j.Append(eventRecord(n))
		if err != nil {
			t.Fatal(err)
		}
		offs = append(offs, off)
	}
	// Random access by offset returns the record written there.
	for i, off := range offs {
		rec, err := j.ReadAt(off)
		if err != nil || rec.Seq != uint64(i+1) || rec.Event == nil || rec.Event.Seq != uint64(i+1) {
			t.Errorf("ReadAt(%d) = %+v, %v", off, rec, err)
		}
	}
	if _, err := j.ReadAt(j.Size()); err == nil {
		t.Error("reading past the end succeeded")
	}
	j.Close()

	_, recs = openJournal(t, path)
	if len(recs) != 3 || recs[2].Seq != 3 || !recs[0].Event.EventTime.Equal(harness.At(1000)) {
		t.Errorf("reopened journal holds %+v", recs)
	}
}

func TestJournalDropsATornTailButRefusesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j, _ := openJournal(t, path)
	for n := uint64(1); n <= 2; n++ {
		if _, err := j.Append(eventRecord(n)); err != nil {
			t.Fatal(err)
		}
	}
	whole := j.Size()
	j.Close()

	// A crash mid-write leaves a line with no newline. It was never
	// acknowledged, so it is cut off and the journal carries on.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"k":"event","seq":3,"at":"2026-01-05T15:00:00Z","event":{"event_id":"half`)
	f.Close()
	j, recs := openJournal(t, path)
	if len(recs) != 2 || j.Size() != whole {
		t.Fatalf("torn tail not dropped: %d records, size %d want %d", len(recs), j.Size(), whole)
	}
	if _, err := j.Append(eventRecord(3)); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if _, recs = openJournal(t, path); len(recs) != 3 {
		t.Fatalf("journal did not continue cleanly after a torn tail: %d records", len(recs))
	}

	// A complete line that does not parse is corruption. Skipping it
	// could silently lose evidence, so the journal refuses to load.
	b, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(b, []byte("\n"))
	lines[1] = []byte("{garbage}\n")
	os.WriteFile(path, bytes.Join(lines, nil), 0o600)
	j2, err := journal.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if err := j2.Scan(func(int64, journal.Record) error { return nil }); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("corrupt journal loaded: %v", err)
	}
}

func TestJournalFailedAppendLeavesNoTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j, _ := openJournal(t, path)
	if _, err := j.Append(eventRecord(1)); err != nil {
		t.Fatal(err)
	}
	size := j.Size()
	boom := errors.New("disk full")
	j.Fail = func() error { return boom }
	if _, err := j.Append(eventRecord(2)); !errors.Is(err, boom) {
		t.Fatalf("append error = %v", err)
	}
	if st, _ := os.Stat(path); st.Size() != size || j.Size() != size {
		t.Errorf("a failed append changed the file: %d -> %d", size, st.Size())
	}
	j.Fail = nil
	if _, err := j.Append(eventRecord(2)); err != nil {
		t.Fatalf("append after the fault cleared: %v", err)
	}
}

// ---- cases ----

func snapshot(alertID string) domain.Snapshot {
	ev := order("A", "o1", domain.ActionNew, 1000)
	return domain.Snapshot{TakenAt: harness.T0, Events: []domain.Event{ev},
		Alert: domain.Alert{ID: alertID, RuleID: rules.HighCancelRate, RuleVersion: 1, Severity: domain.SeverityLow,
			Status: domain.AlertOpen, Symbol: "SIMA", Accounts: []string{"A"}, EventRefs: []string{ev.ID},
			Thresholds: map[string]float64{"min_new_orders": 20}, Observed: map[string]float64{"new_orders": 25.5},
			WindowStart: harness.At(0), WindowEnd: harness.At(60000), Explanation: "why", Disclaimer: domain.Disclaimer}}
}

func openCases(t *testing.T, path string) *cases.Service {
	t.Helper()
	s, err := cases.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCaseWorkflow(t *testing.T) {
	s := openCases(t, filepath.Join(t.TempDir(), "cases.jsonl"))
	now := harness.T0
	tick := func() time.Time { now = now.Add(time.Minute); return now }

	c, err := s.Create("alice", tick(), "", snapshot("alr-1"), "first look")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "case-000001" || c.Status != domain.CaseOpen || c.CreatedBy != "alice" || len(c.Notes) != 1 {
		t.Fatalf("new case: %+v", c)
	}
	if c.Title != "HIGH_CANCEL_RATE: A in SIMA" {
		t.Errorf("default title = %q", c.Title)
	}

	// The workflow is enforced: a case nobody triaged cannot be resolved
	// or escalated.
	if _, err := s.Resolve("alice", tick(), c.ID, domain.DispFalsePositive, "noise"); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("resolving an untriaged case: %v", err)
	}
	if _, err := s.Escalate("alice", tick(), c.ID, "worrying"); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("escalating an untriaged case: %v", err)
	}
	if _, err := s.Reopen("alice", tick(), c.ID, "again"); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("reopening an open case: %v", err)
	}

	c, err = s.Triage("alice", tick(), c.ID, "P2", "bob")
	if err != nil || c.Status != domain.CaseInReview || c.Priority != "P2" || c.Assignee != "bob" {
		t.Fatalf("triage: %+v %v", c, err)
	}
	// Re-triage changes priority and owner, not status.
	c, _ = s.Triage("bob", tick(), c.ID, "P1", "bob")
	if c.Status != domain.CaseInReview || c.Priority != "P1" {
		t.Errorf("re-triage: %+v", c)
	}
	c, _ = s.AddNote("bob", tick(), c.ID, "  checked the tape  ")
	if len(c.Notes) != 2 || c.Notes[1].Text != "checked the tape" || c.Notes[1].Author != "bob" {
		t.Errorf("notes: %+v", c.Notes)
	}
	c, err = s.Escalate("bob", tick(), c.ID, "needs a second pair of eyes")
	if err != nil || c.Status != domain.CaseEscalated {
		t.Fatalf("escalate: %+v %v", c, err)
	}
	if _, err := s.Escalate("bob", tick(), c.ID, "again"); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("escalating twice: %v", err)
	}
	c, err = s.Resolve("alice", tick(), c.ID, domain.DispBenignExplained, "market maker re-quoting")
	if err != nil || c.Status != domain.CaseResolved || c.ResolvedAt == nil || c.Disposition != domain.DispBenignExplained {
		t.Fatalf("resolve: %+v %v", c, err)
	}

	// Resolved cases are closed to everything but notes and reopening.
	if _, err := s.Triage("bob", tick(), c.ID, "P3", "bob"); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("triaging a resolved case: %v", err)
	}
	if _, err := s.Attach("bob", tick(), c.ID, snapshot("alr-2")); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("attaching to a resolved case: %v", err)
	}
	if _, err := s.AddNote("bob", tick(), c.ID, "afterthought"); err != nil {
		t.Errorf("a note on a resolved case was refused: %v", err)
	}
	c, err = s.Reopen("alice", tick(), c.ID, "new information")
	if err != nil || c.Status != domain.CaseInReview || c.ResolvedAt != nil || c.Disposition != "" {
		t.Fatalf("reopen: %+v %v", c, err)
	}
	c, err = s.Attach("alice", tick(), c.ID, snapshot("alr-2"))
	if err != nil || len(c.AlertIDs) != 2 {
		t.Fatalf("attach: %+v %v", c, err)
	}
	if _, err := s.Attach("alice", tick(), c.ID, snapshot("alr-2")); !errors.Is(err, cases.ErrConflict) {
		t.Errorf("attaching the same alert twice: %v", err)
	}

	// Every change above left exactly one audit event, in order, by the
	// person who made it.
	var got []string
	for _, ev := range c.AuditEvents {
		got = append(got, ev.Action+":"+ev.Actor)
	}
	want := "CASE_OPENED:alice TRIAGED:alice TRIAGED:bob NOTE_ADDED:bob ESCALATED:bob RESOLVED:alice NOTE_ADDED:bob REOPENED:alice ALERT_ATTACHED:alice"
	if strings.Join(got, " ") != want {
		t.Errorf("audit trail:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if fmtIDs(s.CasesOf("alr-2")) != "case-000001" || s.Status(c.ID) != domain.CaseInReview {
		t.Errorf("alert index wrong: %v", s.CasesOf("alr-2"))
	}
}

func fmtIDs(ids []string) string { return strings.Join(ids, ",") }

func TestCaseValidation(t *testing.T) {
	s := openCases(t, filepath.Join(t.TempDir(), "cases.jsonl"))
	c, _ := s.Create("alice", harness.T0, "t", snapshot("alr-1"), "")
	s.Triage("alice", harness.T0, c.ID, "P2", "alice")

	invalid := map[string]func() error{
		"no actor":            func() error { _, err := s.AddNote(" ", harness.T0, c.ID, "x"); return err },
		"empty note":          func() error { _, err := s.AddNote("alice", harness.T0, c.ID, "  "); return err },
		"oversized note":      func() error { _, err := s.AddNote("alice", harness.T0, c.ID, strings.Repeat("x", 10001)); return err },
		"bad priority":        func() error { _, err := s.Triage("alice", harness.T0, c.ID, "P0", "bob"); return err },
		"no assignee":         func() error { _, err := s.Triage("alice", harness.T0, c.ID, "P1", ""); return err },
		"no escalate reason":  func() error { _, err := s.Escalate("alice", harness.T0, c.ID, ""); return err },
		"no resolve summary":  func() error { _, err := s.Resolve("alice", harness.T0, c.ID, domain.DispInconclusive, ""); return err },
		"unknown disposition": func() error { _, err := s.Resolve("alice", harness.T0, c.ID, "GUILTY", "x"); return err },
		"oversized title": func() error {
			_, err := s.Create("alice", harness.T0, strings.Repeat("x", 201), snapshot("alr-9"), "")
			return err
		},
	}
	for name, fn := range invalid {
		if err := fn(); !errors.Is(err, cases.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.AddNote("alice", harness.T0, "case-999999", "x"); !errors.Is(err, cases.ErrNotFound) {
		t.Errorf("unknown case: %v", err)
	}
	// None of the refused requests left an audit event.
	got, _ := s.Get(c.ID)
	if len(got.AuditEvents) != 2 || s.Head().Events != 2 {
		t.Errorf("refused requests reached the audit log: %d events", s.Head().Events)
	}
	// No disposition asserts wrongdoing.
	for _, d := range []string{"CONFIRMED_MANIPULATION", "GUILTY", "MISCONDUCT"} {
		if domain.ValidDisposition(d) {
			t.Errorf("disposition %s is accepted", d)
		}
	}
}

func TestCaseLogSurvivesRestartExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	s := openCases(t, path)
	c, _ := s.Create("alice", harness.T0, "one", snapshot("alr-1"), "n1")
	s.Triage("alice", harness.T0.Add(time.Minute), c.ID, "P1", "bob")
	s.Resolve("bob", harness.T0.Add(time.Hour), c.ID, domain.DispFalsePositive, "noise")
	s.Create("bob", harness.T0.Add(2*time.Hour), "two", snapshot("alr-2"), "")
	before, _ := json.Marshal([]any{s.List(""), s.Head(), s.Summary(), s.CasesOf("alr-1")})
	a, _ := s.Get(c.ID)
	s.Close()

	s2 := openCases(t, path)
	after, _ := json.Marshal([]any{s2.List(""), s2.Head(), s2.Summary(), s2.CasesOf("alr-1")})
	if !bytes.Equal(before, after) {
		t.Errorf("state changed across a restart:\n before %s\n after  %s", before, after)
	}
	b, _ := s2.Get(c.ID)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Errorf("case changed across a restart:\n before %s\n after  %s", ja, jb)
	}
	if st := s2.Verify(); !st.Verified || st.Events != 4 {
		t.Errorf("chain after restart: %+v", st)
	}
	sm := s2.Summary()
	if sm.Total != 2 || sm.ByStatus[domain.CaseResolved] != 1 || sm.ResolutionRate != 0.5 ||
		sm.MeanResolveSecs != 3600 || sm.FalsePositiveRate != 1 {
		t.Errorf("summary: %+v", sm)
	}
	// New cases continue the numbering.
	c3, _ := s2.Create("alice", harness.T0, "three", snapshot("alr-3"), "")
	if c3.ID != "case-000003" {
		t.Errorf("next case ID = %s", c3.ID)
	}
}

// tamper rewrites the case log through fn and reports what Open and
// Verify make of it.
func tamper(t *testing.T, fn func(lines [][]byte) [][]byte) (openErr error, verify cases.ChainStatus) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	s := openCases(t, path)
	c, _ := s.Create("alice", harness.T0, "one", snapshot("alr-1"), "note")
	s.Triage("alice", harness.T0, c.ID, "P1", "bob")
	s.AddNote("bob", harness.T0, c.ID, "looks benign")
	s.Resolve("bob", harness.T0, c.ID, domain.DispBenignExplained, "explained")

	b, _ := os.ReadFile(path)
	lines := bytes.SplitAfter(bytes.TrimRight(b, "\n"), []byte("\n"))
	lines[len(lines)-1] = append(lines[len(lines)-1], '\n')
	if err := os.WriteFile(path, bytes.Join(fn(lines), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	verify = s.Verify() // the running service re-reads the disk
	s2, err := cases.Open(path)
	if err == nil {
		s2.Close()
	}
	return err, verify
}

func TestAuditChainDetectsTampering(t *testing.T) {
	edits := map[string]func([][]byte) [][]byte{
		"a note reworded": func(l [][]byte) [][]byte {
			l[2] = bytes.Replace(l[2], []byte("looks benign"), []byte("looks guilty"), 1)
			return l
		},
		"an actor changed": func(l [][]byte) [][]byte {
			l[1] = bytes.Replace(l[1], []byte(`"actor":"alice"`), []byte(`"actor":"mallory"`), 1)
			return l
		},
		"an event deleted":   func(l [][]byte) [][]byte { return append(l[:1:1], l[2:]...) },
		"events reordered":   func(l [][]byte) [][]byte { l[1], l[2] = l[2], l[1]; return l },
		"the tail truncated": func(l [][]byte) [][]byte { return l[:3] },
		"evidence altered": func(l [][]byte) [][]byte {
			// Change the frozen evidence but leave the audit event itself alone.
			l[0] = bytes.Replace(l[0], []byte(`"new_orders":25.5`), []byte(`"new_orders":99`), 1)
			return l
		},
		// The edited event is given a fresh, valid hash of its own. Only
		// the link from the next event gives it away.
		"a note reworded and re-hashed": func(l [][]byte) [][]byte {
			l[2] = rehash(t, bytes.Replace(l[2], []byte("looks benign"), []byte("looks guilty"), 1), "")
			return l
		},
		"a disposition flipped": func(l [][]byte) [][]byte {
			l[3] = bytes.Replace(l[3], []byte(domain.DispBenignExplained), []byte(domain.DispReferred), 1)
			return l
		},
	}
	for name, edit := range edits {
		openErr, verify := tamper(t, edit)
		if verify.Verified {
			t.Errorf("%s: Verify still reports the chain intact", name)
		}
		// Truncating whole events from the end leaves a shorter but
		// internally consistent chain: only the running service, which
		// remembers the head, can tell. Everything else must also stop
		// a fresh start.
		if name == "the tail truncated" {
			continue
		}
		if openErr == nil {
			t.Errorf("%s: the service started on a tampered log", name)
		}
	}
	// The limit of a hash chain: someone who can rewrite the file can
	// re-hash everything after their edit. A fresh start then sees a
	// consistent chain. The running service still notices, because the
	// head it remembers no longer matches; so does anyone who kept a
	// copy of the head hash (GET /v1/audit/head) somewhere else.
	openErr, verify := tamper(t, func(l [][]byte) [][]byte {
		l[2] = rehash(t, bytes.Replace(l[2], []byte("looks benign"), []byte("looks guilty"), 1), "")
		l[3] = rehash(t, l[3], hashOf(t, l[2]))
		return l
	})
	if openErr != nil {
		t.Errorf("a fully re-hashed tail is expected to load (documented limit), got %v", openErr)
	}
	if verify.Verified {
		t.Error("the running service did not notice a fully re-hashed tail")
	}

	// And an untouched log passes.
	openErr, verify = tamper(t, func(l [][]byte) [][]byte { return l })
	if openErr != nil || !verify.Verified || verify.Events != 4 {
		t.Errorf("untouched log: open=%v verify=%+v", openErr, verify)
	}
}

func hashOf(t *testing.T, line []byte) string {
	t.Helper()
	var ev domain.AuditEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Hash
}

// rehash gives an audit event (one without a snapshot body) a valid hash
// of its own, optionally re-pointing it at a new predecessor.
func rehash(t *testing.T, line []byte, prev string) []byte {
	t.Helper()
	var ev domain.AuditEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	if prev != "" {
		ev.PrevHash = prev
	}
	ev.Hash = ""
	b, _ := json.Marshal(ev)
	sum := sha256.Sum256(b)
	ev.Hash = hex.EncodeToString(sum[:])
	out, _ := json.Marshal(ev)
	return append(out, '\n')
}

func TestCaseWriteFailureChangesNothing(t *testing.T) {
	s := openCases(t, filepath.Join(t.TempDir(), "cases.jsonl"))
	c, _ := s.Create("alice", harness.T0, "one", snapshot("alr-1"), "")
	s.Fail = func() error { return errors.New("disk full") }
	if _, err := s.Triage("alice", harness.T0, c.ID, "P1", "bob"); !errors.Is(err, cases.ErrUnavailable) {
		t.Fatalf("triage with a failing log: %v", err)
	}
	if _, err := s.Create("alice", harness.T0, "two", snapshot("alr-2"), ""); !errors.Is(err, cases.ErrUnavailable) {
		t.Fatalf("create with a failing log: %v", err)
	}
	// An export that cannot be recorded is not handed out.
	if _, err := s.Export("alice", harness.T0, c.ID); !errors.Is(err, cases.ErrUnavailable) {
		t.Fatalf("export with a failing log: %v", err)
	}
	got, _ := s.Get(c.ID)
	if got.Status != domain.CaseOpen || len(s.List("")) != 1 || s.Head().Events != 1 {
		t.Errorf("a failed write changed state: %+v", got)
	}
	s.Fail = nil
	if c, err := s.Triage("alice", harness.T0, c.ID, "P1", "bob"); err != nil || c.Status != domain.CaseInReview {
		t.Errorf("triage after recovery: %+v %v", c, err)
	}
	if st := s.Verify(); !st.Verified {
		t.Errorf("chain after a failed write: %+v", st)
	}
}

func TestEvidenceExport(t *testing.T) {
	s := openCases(t, filepath.Join(t.TempDir(), "cases.jsonl"))
	c, _ := s.Create("alice", harness.T0, "one", snapshot("alr-1"), "note")
	s.Triage("alice", harness.T0, c.ID, "P1", "bob")

	b, err := s.Export("bob", harness.T0.Add(time.Hour), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Disclaimer != domain.Disclaimer || b.ExportedBy != "bob" || !b.AuditChain.Verified || len(b.BundleHash) != 64 {
		t.Errorf("bundle header: %+v", b.AuditChain)
	}
	if len(b.Evidence) != 1 || len(b.Evidence[0].Events) != 1 || b.Evidence[0].Alert.ID != "alr-1" {
		t.Fatalf("bundle evidence: %+v", b.Evidence)
	}
	// The snapshot's hash is recomputable from its contents and is the
	// one the audit trail committed to when the case was opened.
	if cases.SnapshotHash(b.Evidence[0]) != b.Evidence[0].Hash {
		t.Error("snapshot hash does not match its contents")
	}
	var opened cases.Details
	json.Unmarshal(b.AuditTrail[0].Details, &opened)
	if opened.SnapshotHash != b.Evidence[0].Hash || opened.EventCount != 1 {
		t.Errorf("audit trail does not commit to the snapshot: %+v", opened)
	}
	if len(b.AuditTrail) != 2 {
		t.Errorf("bundle has %d audit events, want the 2 before the export", len(b.AuditTrail))
	}

	// Taking the export is itself on the record, naming the bundle.
	got, _ := s.Get(c.ID)
	last := got.AuditEvents[len(got.AuditEvents)-1]
	var d cases.Details
	json.Unmarshal(last.Details, &d)
	if last.Action != cases.ActExported || last.Actor != "bob" || d.BundleHash != b.BundleHash {
		t.Errorf("export not recorded: %+v %+v", last, d)
	}
}
