// demo walks through the whole system on simulated data, over real HTTP,
// including the ways it is expected to fail: late, duplicated and
// future-stamped events, a storage outage, a restart, and an edited audit
// log. It checks every step and exits non-zero if one does not hold.
//
// It listens on a loopback port chosen by the OS, writes to a temporary
// directory that it removes, and generates its own throwaway tokens. It
// runs on a simulated clock, so the output is the same every time.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"surveillance/src/api"
	"surveillance/src/cases"
	"surveillance/src/domain"
	"surveillance/src/engine"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type demo struct {
	dir, rules, refdata string
	clock               *clock
	eng                 *engine.Engine
	srv                 *http.Server
	url                 string
	ingest, admin       string
	alice, bob          string
	failed              int
}

func token() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (d *demo) start() error {
	eng, err := engine.Open(engine.Config{DataDir: d.dir, RulesPath: d.rules, RefDataPath: d.refdata, Clock: d.clock.Now})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		eng.Close()
		return err
	}
	d.eng = eng
	d.url = "http://" + ln.Addr().String()
	d.srv = &http.Server{Handler: api.New(api.Config{Engine: eng, IngestToken: d.ingest, AdminToken: d.admin,
		Analysts: map[string]string{d.alice: "alice", d.bob: "bob"}, EnableSim: true})}
	go d.srv.Serve(ln)
	return nil
}

func (d *demo) stop() {
	if d.srv != nil {
		d.srv.Close()
		d.srv = nil
	}
	if d.eng != nil {
		d.eng.Close()
		d.eng = nil
	}
}

// call sends a request and decodes the JSON reply into out (if not nil).
func (d *demo) call(method, path, tok string, body, out any) int {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.url+path, rd)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("   request failed:", err)
		d.failed++
		return 0
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			fmt.Printf("   could not decode the reply to %s %s: %v\n", method, path, err)
			d.failed++
		}
	}
	return res.StatusCode
}

func step(n int, title string) { fmt.Printf("\n%d. %s\n", n, title) }

func say(format string, args ...any) { fmt.Printf("   "+format+"\n", args...) }

// check prints a line and records a failure if the condition is false.
func (d *demo) check(ok bool, format string, args ...any) {
	mark := "ok  "
	if !ok {
		mark = "FAIL"
		d.failed++
	}
	fmt.Printf("   [%s] %s\n", mark, fmt.Sprintf(format, args...))
}

type simReply struct {
	Events   int `json:"events"`
	Scenario struct {
		Name      string   `json:"name"`
		Label     string   `json:"label"`
		Intensity string   `json:"intensity"`
		Variant   string   `json:"variant"`
		Accounts  []string `json:"accounts"`
	} `json:"scenario"`
	Ingest engine.IngestResult `json:"ingest"`
	Flush  engine.FlushResult  `json:"flush"`
}

func (r simReply) created() []string { return append(r.Ingest.AlertsCreated, r.Flush.AlertsCreated...) }

func (d *demo) scenario(name string, seed int) simReply {
	var r simReply
	status := d.call("POST", "/v1/sim/scenarios/"+name, d.admin, map[string]any{"seed": seed}, &r)
	what := r.Scenario.Label + ", " + r.Scenario.Intensity
	if r.Scenario.Variant != "" {
		what += ", " + r.Scenario.Variant
	}
	say("POST /v1/sim/scenarios/%s {seed: %d} -> %d: %d events (%s), %d alert(s)", name, seed, status, r.Events, what, len(r.created()))
	// Let simulated time move on, so the next scenario starts after this one.
	d.clock.Advance(10 * time.Minute)
	return r
}

func (d *demo) alert(id string) engine.AlertDetail {
	var a engine.AlertDetail
	d.call("GET", "/v1/alerts/"+id, d.alice, nil, &a)
	return a
}

func (d *demo) rulesOf(ids []string) string {
	var rs []string
	for _, id := range ids {
		rs = append(rs, d.alert(id).Alert.RuleID)
	}
	sort.Strings(rs)
	return strings.Join(rs, ", ")
}

func sorted(m map[string]float64) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

type event map[string]any

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func order(id, acct, orderID, action, side string, at time.Time) event {
	e := event{"type": "order", "event_id": id, "venue": "XSIM", "symbol": "SIMB", "ts": stamp(at),
		"account_id": acct, "order_id": orderID, "action": action}
	if action != "CANCEL" {
		e["side"], e["qty"], e["price"] = side, 100, "25.10"
	}
	return e
}

func main() {
	rulesPath := flag.String("rules", "examples/rules.json", "ruleset file")
	refPath := flag.String("refdata", "examples/refdata.json", "reference data file")
	flag.Parse()

	dir, err := os.MkdirTemp("", "surv-demo-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	d := &demo{dir: dir, rules: *rulesPath, refdata: *refPath,
		clock:  &clock{t: time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)},
		ingest: token(), admin: token(), alice: token(), bob: token()}
	if err := d.start(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	defer d.stop()

	fmt.Println("Market surveillance engine: demo on SIMULATED data with a simulated clock.")
	fmt.Println(domain.Disclaimer)
	d.run()

	fmt.Println()
	if d.failed > 0 {
		fmt.Printf("%d check(s) FAILED\n", d.failed)
		d.stop()
		os.RemoveAll(dir)
		os.Exit(1)
	}
	fmt.Println("All checks passed.")
}

func (d *demo) run() {
	// ---- detection ----
	step(1, "A layering-like scenario raises an alert that explains itself")
	lay := d.scenario("layering_like", 1)
	var layID string
	for _, id := range lay.created() {
		if d.alert(id).Alert.RuleID == "LAYERING_LIKE" {
			layID = id
			break
		}
	}
	d.check(layID != "", "a LAYERING_LIKE alert was raised")
	if layID == "" {
		return
	}
	a := d.alert(layID)
	say("alert      %s  %s v%d  severity %s  status %s", a.Alert.ID, a.Alert.RuleID, a.Alert.RuleVersion, a.Alert.Severity, a.Alert.Status)
	say("ruleset    %s (sha256 %s...)", a.Alert.RulesetVersion, a.Alert.RulesetHash[:12])
	say("window     %s .. %s (%d occurrence(s) folded into this alert)", stamp(a.Alert.WindowStart), stamp(a.Alert.WindowEnd), a.Alert.Occurrences)
	say("observed   %s", sorted(a.Alert.Observed))
	say("thresholds %s", sorted(a.Alert.Thresholds))
	say("evidence   %d event IDs, e.g. %s", len(a.Alert.EventRefs), strings.Join(a.Alert.EventRefs[:3], ", "))
	say("why        %s", a.Alert.Explanation)
	d.check(len(a.Events) == len(a.Alert.EventRefs) && len(a.Alert.Thresholds) > 0 && a.Alert.RuleVersion > 0,
		"every evidence ID resolves to a stored event; rule version and thresholds are attached")

	var list struct {
		Alerts []domain.Alert `json:"alerts"`
		Count  int            `json:"count"`
	}
	d.call("GET", "/v1/alerts?symbol=SIMA&status=open", d.alice, nil, &list)
	d.check(list.Count >= 1, "GET /v1/alerts?symbol=SIMA&status=open lists %d open alert(s)", list.Count)

	step(2, "Other patterns, and benign flow that must stay quiet")
	wash := d.scenario("wash_trade_like", 2)
	d.check(strings.Contains(d.rulesOf(wash.created()), "WASH_TRADE_LIKE"), "wash-trade-like scenario -> %s", d.rulesOf(wash.created()))
	burst := d.scenario("high_cancel_rate", 3)
	d.check(strings.Contains(d.rulesOf(burst.created()), "HIGH_CANCEL_RATE"), "cancel burst -> %s", d.rulesOf(burst.created()))
	flicker := d.scenario("quote_flicker_like", 4)
	d.check(strings.Contains(d.rulesOf(flicker.created()), "HIGH_ORDER_TRADE_RATIO"), "quote flicker -> %s", d.rulesOf(flicker.created()))
	normal := d.scenario("normal_flow", 5)
	d.check(len(normal.created()) == 0, "ordinary two-way flow among %d accounts -> no alert", len(normal.Scenario.Accounts))

	step(3, "A false positive, shown on purpose")
	say("A market maker who cancels and replaces a ladder of quotes after each fill is")
	say("doing nothing unusual, and looks a lot like the patterns above.")
	mm := d.scenario("benign_market_maker", 2)
	d.check(mm.Scenario.Variant == "cancel_replace" && len(mm.created()) > 0,
		"benign market maker (%s) -> %s: a false positive, kept in the evaluation set as such", mm.Scenario.Variant, d.rulesOf(mm.created()))
	var fpID string
	if ids := mm.created(); len(ids) > 0 {
		fpID = ids[0]
	}

	// ---- stream failure modes ----
	step(4, "Out-of-order, duplicate, late and future-stamped events")
	now := d.clock.Now()
	var batch []event
	for i := 0; i < 30; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		id := fmt.Sprintf("demo-o%d", i)
		batch = append(batch, order(fmt.Sprintf("demo-n%d", i), "ACC-2001", id, "NEW", "BUY", at),
			order(fmt.Sprintf("demo-c%d", i), "ACC-2001", id, "CANCEL", "", at.Add(40*time.Millisecond)))
	}
	d.clock.Advance(5 * time.Second)
	// Deliver it backwards: every event arrives before the ones that preceded it.
	reversed := make([]event, len(batch))
	for i, e := range batch {
		reversed[len(batch)-1-i] = e
	}
	var res engine.IngestResult
	status := d.call("POST", "/v1/events", d.ingest, reversed, &res)
	d.check(status == 200 && res.Accepted == 60 && res.Late == 0,
		"60 events delivered in reverse order (3s of disorder, 5s allowed): %d accepted, %d late", res.Accepted, res.Late)
	status = d.call("POST", "/v1/events", d.ingest, batch, &res)
	d.check(status == 200 && res.Duplicates == 60 && res.Accepted == 0, "the same 60 delivered again: %d duplicates, %d accepted", res.Duplicates, res.Accepted)

	d.clock.Advance(time.Minute)
	fresh := order("demo-fresh", "ACC-2001", "demo-x1", "NEW", "BUY", d.clock.Now())
	stale := order("demo-stale", "ACC-2001", "demo-x2", "NEW", "BUY", d.clock.Now().Add(-30*time.Second))
	future := order("demo-future", "ACC-2001", "demo-x3", "NEW", "BUY", d.clock.Now().Add(time.Hour))
	broken := event{"type": "order", "venue": "XSIM", "symbol": "SIMB", "ts": "yesterday"}
	status = d.call("POST", "/v1/events", d.ingest, []event{fresh, stale, future, broken}, &res)
	codes := []string{}
	for _, r := range res.Rejected {
		codes = append(codes, r.Code)
	}
	d.check(status == 200 && res.Accepted == 1 && res.Late == 1 && strings.Join(codes, ",") == "FUTURE_TIMESTAMP,INVALID_TIMESTAMP",
		"one on-time, one 30s stale, one stamped an hour ahead, one malformed: %d accepted, %d late (stored, not evaluated), rejected %v",
		res.Accepted, res.Late, codes)
	// The on-time event moved the watermark past the burst, which is
	// when its windows closed and were evaluated.
	var fl engine.FlushResult
	d.call("POST", "/v1/ingest/flush", d.ingest, nil, &fl)
	raised := d.rulesOf(append(res.AlertsCreated, fl.AlertsCreated...))
	d.check(strings.Contains(raised, "HIGH_CANCEL_RATE"), "the reversed burst was evaluated in event-time order -> %s", raised)
	d.clock.Advance(10 * time.Minute)

	// ---- storage outage ----
	step(5, "The journal refuses writes (simulated disk failure)")
	now = d.clock.Now()
	batch = batch[:0]
	for i := 0; i < 20; i++ {
		batch = append(batch, order(fmt.Sprintf("demo-j%d", i), "ACC-2001", fmt.Sprintf("demo-j%d", i), "NEW", "SELL", now.Add(time.Duration(i)*10*time.Millisecond)))
	}
	d.clock.Advance(time.Second)
	writes := 0
	d.eng.SetJournalFault(func() error {
		if writes++; writes > 8 {
			return errors.New("simulated: no space left on device")
		}
		return nil
	})
	var outage struct {
		Error  struct{ Code, Message string }
		Result engine.IngestResult
	}
	status = d.call("POST", "/v1/events", d.ingest, batch, &outage)
	d.check(status == 503 && outage.Error.Code == "STORAGE_UNAVAILABLE" && outage.Result.Accepted == 8,
		"POST /v1/events -> %d %s after storing %d of 20: nothing is accepted without being journalled", status, outage.Error.Code, outage.Result.Accepted)
	status = d.call("GET", "/v1/alerts", d.alice, nil, nil)
	d.check(status == 200, "reads still work during the outage (GET /v1/alerts -> %d)", status)
	d.eng.SetJournalFault(nil)
	status = d.call("POST", "/v1/events", d.ingest, batch, &res)
	d.check(status == 200 && res.Duplicates == 8 && res.Accepted == 12,
		"storage back, client resends the whole batch: %d recognised as already stored, %d accepted", res.Duplicates, res.Accepted)

	// ---- cases ----
	step(6, "An analyst works the layering alert as a case")
	var c domain.Case
	status = d.call("POST", "/v1/alerts/"+layID+"/cases", d.alice, map[string]string{"note": "Picked up from the open queue."}, &c)
	d.check(status == 201, "alice: POST /v1/alerts/%s/cases -> %d, %s (%s)", layID, status, c.ID, c.Title)
	status = d.call("POST", "/v1/alerts/"+layID+"/cases", d.bob, nil, nil)
	d.check(status == 409, "bob tries to open a second case on the same alert -> %d", status)
	d.clock.Advance(4 * time.Minute)
	d.call("POST", "/v1/cases/"+c.ID+"/triage", d.alice, map[string]string{"priority": "P2", "assignee": "bob"}, &c)
	d.clock.Advance(11 * time.Minute)
	d.call("POST", "/v1/cases/"+c.ID+"/notes", d.bob, map[string]string{
		"text": "Sell orders sat 2-6 ticks above the touch and were pulled within 3s of the buy fill. Same shape in two of three cycles."}, &c)
	d.clock.Advance(20 * time.Minute)
	status = d.call("POST", "/v1/cases/"+c.ID+"/resolve", d.bob, map[string]string{
		"disposition": "REFERRED_FOR_REVIEW", "summary": "Repeated pattern; referring for human compliance review. No conclusion drawn here."}, &c)
	d.check(status == 200 && c.Status == domain.CaseResolved, "triaged (P2, bob), annotated, resolved as %s", c.Disposition)
	say("dispositions available: FALSE_POSITIVE, BENIGN_EXPLAINED, INCONCLUSIVE, REFERRED_FOR_REVIEW. None asserts wrongdoing.")

	if fpID != "" {
		var fp domain.Case
		d.call("POST", "/v1/alerts/"+fpID+"/cases", d.alice, nil, &fp)
		d.call("POST", "/v1/cases/"+fp.ID+"/triage", d.alice, map[string]string{"priority": "P3"}, nil)
		d.clock.Advance(6 * time.Minute)
		status = d.call("POST", "/v1/cases/"+fp.ID+"/resolve", d.alice, map[string]string{
			"disposition": "FALSE_POSITIVE", "summary": "Registered market maker re-quoting after fills."}, &fp)
		d.check(status == 200, "the market-maker alert from step 3 -> %s, resolved %s", fp.ID, fp.Disposition)
	}

	step(7, "Evidence export and audit trail")
	d.clock.Advance(time.Minute)
	var b cases.Bundle
	status = d.call("GET", "/v1/cases/"+c.ID+"/evidence", d.bob, nil, &b)
	d.check(status == 200 && len(b.Evidence) == 1 && b.AuditChain.Verified, "GET /v1/cases/%s/evidence -> %d", c.ID, status)
	if len(b.Evidence) == 1 {
		sn := b.Evidence[0]
		say("snapshot   alert %s as it stood at %s, with %d events in full (sha256 %s...)",
			sn.Alert.ID, stamp(sn.TakenAt), len(sn.Events), sn.Hash[:12])
	}
	say("audit trail:")
	for _, ev := range b.AuditTrail {
		say("  #%d %s  %-17s by %-5s  hash %s...", ev.Seq, stamp(ev.Time), ev.Action, ev.Actor, ev.Hash[:12])
	}
	d.call("GET", "/v1/cases/"+c.ID, d.alice, nil, &c)
	last := c.AuditEvents[len(c.AuditEvents)-1]
	d.check(last.Action == cases.ActExported && last.Actor == "bob", "the export itself is on the record: #%d %s by %s", last.Seq, last.Action, last.Actor)

	// ---- restart ----
	step(8, "Restart: everything is rebuilt from the journal and the case log")
	before := d.summary()
	d.stop()
	d.clock.Advance(3 * time.Minute)
	if err := d.start(); err != nil {
		d.check(false, "restart: %v", err)
		return
	}
	after := d.summary()
	d.check(before == after, "same alerts (IDs, evidence, status) and cases after restart: %s", after)
	var chain cases.ChainStatus
	d.call("GET", "/v1/audit/verify", d.alice, nil, &chain)
	d.check(chain.Verified, "audit chain verifies: %d events, head %s...", chain.Events, chain.Head[:12])

	step(9, "Someone edits the case log on disk")
	path := filepath.Join(d.dir, "cases.jsonl")
	orig, _ := os.ReadFile(path)
	edited := bytes.Replace(orig, []byte("REFERRED_FOR_REVIEW"), []byte("FALSE_POSITIVE"), 1)
	_ = os.WriteFile(path, edited, 0o600)
	d.call("GET", "/v1/audit/verify", d.alice, nil, &chain)
	d.check(!chain.Verified, "a disposition changed in the file -> verified=%v: %s", chain.Verified, chain.Error)
	d.stop()
	err := d.start()
	d.check(err != nil, "and the server refuses to start on it: %s", strings.ReplaceAll(fmt.Sprint(err), d.dir, "<data>"))
	_ = os.WriteFile(path, orig, 0o600)
	if err == nil {
		d.stop()
	}
	if err := d.start(); err != nil {
		d.check(false, "restart on the restored log: %v", err)
		return
	}
	d.call("GET", "/v1/audit/verify", d.alice, nil, &chain)
	d.check(chain.Verified, "original file restored -> verified=%v", chain.Verified)

	step(10, "Counters")
	var st engine.Stats
	d.call("GET", "/v1/stats", d.alice, nil, &st)
	say("events %d (late %d) | alerts %d from %d detections, %d folded into an open alert (%.0f%%)",
		st.Events.Total, st.Events.Late, st.Alerts.Total, st.Alerts.Detections, st.Alerts.Suppressed, 100*st.Alerts.DuplicateRate)
	say("alerts per million events %.0f (meaningless at this size and on a feed built to trigger alerts)", st.Alerts.PerMillionEvents)
	say("cases %d, resolved %.0f%%, mean time to resolve %.0f min (simulated clock)",
		st.Cases.Total, 100*st.Cases.ResolutionRate, st.Cases.MeanResolveSecs/60)
	d.check(st.Events.Late == 1 && st.Cases.Total >= 1, "one late event on record; cases counted")
}

// summary is a fingerprint of alerts and cases, for before/after comparison.
func (d *demo) summary() string {
	alerts := d.eng.Alerts(engine.AlertFilter{})
	a, _ := json.Marshal(alerts)
	c, _ := json.Marshal(d.eng.Cases(""))
	sum := func(b []byte) string {
		h := uint64(14695981039346656037)
		for _, x := range b {
			h = (h ^ uint64(x)) * 1099511628211
		}
		return fmt.Sprintf("%016x", h)
	}
	return fmt.Sprintf("%d alerts (%s), %d cases (%s)", len(alerts), sum(a), len(d.eng.Cases("")), sum(c))
}
