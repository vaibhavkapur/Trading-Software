package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"surveillance/src/api"
	"surveillance/src/cases"
	"surveillance/src/domain"
	"surveillance/src/engine"
	"surveillance/src/normalize"
	"surveillance/src/rules"
	h "surveillance/tests/harness"
)

type apiErr struct {
	Error struct{ Code, Message string }
}

func errCode(t *testing.T, raw []byte) string {
	t.Helper()
	var e apiErr
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("not an error body: %s", raw)
	}
	return e.Error.Code
}

type alertList struct {
	Alerts []domain.Alert
	Count  int
}

// layeringAlert ingests one layering pattern over HTTP and returns the alert.
func layeringAlert(t *testing.T, e *h.Env, acct string, start time.Time) domain.Alert {
	t.Helper()
	evs := h.Layering(acct, start, 4, 1000, 200, 4, time.Second)
	var res engine.IngestResult
	e.JSON("POST", "/v1/events", h.IngestToken, evs, 200, &res)
	if res.Accepted != len(evs) || len(res.Rejected) != 0 {
		t.Fatalf("ingest: %+v", res)
	}
	var fl engine.FlushResult
	e.JSON("POST", "/v1/ingest/flush", h.IngestToken, nil, 200, &fl)
	if len(fl.AlertsCreated) != 1 {
		t.Fatalf("flush created %v, want one alert", fl.AlertsCreated)
	}
	var d engine.AlertDetail
	e.JSON("GET", "/v1/alerts/"+fl.AlertsCreated[0], h.AliceToken, nil, 200, &d)
	return d.Alert
}

func TestAuthentication(t *testing.T) {
	e := h.New(t)
	type call struct {
		method, path, token string
		want                int
	}
	calls := []call{
		// Ingest routes take the ingest token and nothing else.
		{"POST", "/v1/events", "", 401}, {"POST", "/v1/events", h.AliceToken, 401}, {"POST", "/v1/events", h.AdminToken, 401},
		{"POST", "/v1/ingest/flush", "", 401}, {"POST", "/v1/ingest/flush", h.IngestToken, 200},
		// Analyst routes take an analyst token and nothing else.
		{"GET", "/v1/alerts", "", 401}, {"GET", "/v1/alerts", h.IngestToken, 401}, {"GET", "/v1/alerts", h.AdminToken, 401},
		{"GET", "/v1/alerts", "wrong-token-wrong-token", 401}, {"GET", "/v1/alerts", h.AliceToken, 200}, {"GET", "/v1/alerts", h.BobToken, 200},
		{"GET", "/v1/cases", "", 401}, {"GET", "/v1/stats", "", 401}, {"GET", "/v1/features", "", 401}, {"GET", "/v1/rules", "", 401},
		{"GET", "/v1/audit/verify", "", 401}, {"GET", "/v1/audit/head", h.AliceToken, 200},
		{"GET", "/v1/cases/case-000001/evidence", "", 401}, {"POST", "/v1/cases/case-000001/notes", h.IngestToken, 401},
		// Admin routes take the admin token and nothing else.
		{"POST", "/v1/rules/reload", "", 401}, {"POST", "/v1/rules/reload", h.AliceToken, 401}, {"POST", "/v1/rules/reload", h.AdminToken, 200},
		// The simulator is off unless the server was started with it.
		{"POST", "/v1/sim/scenarios/layering_like", h.AdminToken, 403}, {"POST", "/v1/sim/scenarios/layering_like", "", 401},
		// Health and metrics carry no account data and are open.
		{"GET", "/healthz", "", 200}, {"GET", "/metrics", "", 200},
		{"GET", "/v1/nope", h.AliceToken, 404}, {"DELETE", "/v1/alerts", h.AliceToken, 405},
	}
	for _, c := range calls {
		if status, raw := e.Do(c.method, c.path, c.token, nil); status != c.want {
			t.Errorf("%s %s with token %q: %d, want %d (%s)", c.method, c.path, c.token, status, c.want, raw)
		}
	}
	var who map[string]string
	e.JSON("GET", "/v1/whoami", h.BobToken, nil, 200, &who)
	if who["analyst"] != "bob" {
		t.Errorf("whoami = %v", who)
	}
}

func TestRoutesWithoutACredentialAreDisabledNotOpen(t *testing.T) {
	e := h.New(t)
	bare := httptest.NewServer(api.New(api.Config{Engine: e.Engine}))
	defer bare.Close()
	for _, c := range [][2]string{{"POST", "/v1/events"}, {"POST", "/v1/ingest/flush"}, {"POST", "/v1/rules/reload"},
		{"GET", "/v1/alerts"}, {"GET", "/v1/cases"}, {"GET", "/v1/stats"}, {"POST", "/v1/sim/scenarios/normal_flow"}} {
		for _, token := range []string{"", h.AliceToken, h.IngestToken} {
			req, _ := http.NewRequest(c[0], bare.URL+c[1], nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with no credential configured: %d, want 403", c[0], c[1], res.StatusCode)
			}
		}
	}
}

func TestAnalystTokenConfiguration(t *testing.T) {
	got, err := api.ParseAnalysts(" alice:aaaaaaaaaaaaaaaa , bob:bbbbbbbbbbbbbbbb ")
	if err != nil || len(got) != 2 || got["aaaaaaaaaaaaaaaa"] != "alice" {
		t.Errorf("ParseAnalysts = %v, %v", got, err)
	}
	if got, err := api.ParseAnalysts(""); err != nil || len(got) != 0 {
		t.Errorf("empty configuration: %v, %v", got, err)
	}
	for name, in := range map[string]string{
		"short token":  "alice:short",
		"no name":      ":aaaaaaaaaaaaaaaa",
		"no separator": "aaaaaaaaaaaaaaaaaaaa",
		"shared token": "alice:aaaaaaaaaaaaaaaa,bob:aaaaaaaaaaaaaaaa",
		"same name":    "alice:aaaaaaaaaaaaaaaa,alice:bbbbbbbbbbbbbbbb",
	} {
		if _, err := api.ParseAnalysts(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIngestToEvidenceOverHTTP(t *testing.T) {
	e := h.New(t)
	a := layeringAlert(t, e, "L1", h.At(0))

	// The alert says what fired, under which rules, on what evidence.
	if a.RuleID != rules.LayeringLike || a.RuleVersion != 1 || a.RulesetVersion != "test-1" || len(a.RulesetHash) != 64 {
		t.Errorf("rule identity: %+v", a)
	}
	if a.Status != domain.AlertOpen || a.Symbol != "SIMA" || fmt.Sprint(a.Accounts) != "[L1]" || a.Disclaimer != domain.Disclaimer {
		t.Errorf("alert: %+v", a)
	}
	if a.Thresholds["min_cancel_pct"] != 80 || a.Observed["cancel_pct"] != 100 || len(a.EventRefs) != 9 || a.Explanation == "" {
		t.Errorf("explanation data: %+v", a)
	}
	if !a.WindowStart.Equal(h.At(0)) || !a.WindowEnd.After(a.WindowStart) {
		t.Errorf("window bounds: %v..%v", a.WindowStart, a.WindowEnd)
	}

	// Filters, case-insensitively.
	var list alertList
	e.JSON("GET", "/v1/alerts?symbol=sima&status=open&rule_id=layering_like&account=l1&severity=high", h.AliceToken, nil, 200, &list)
	if list.Count != 1 || list.Alerts[0].ID != a.ID {
		t.Fatalf("filtered list: %+v", list)
	}
	for _, q := range []string{"symbol=OTHER", "status=closed", "rule_id=WASH_TRADE_LIKE", "account=CP", "severity=low"} {
		e.JSON("GET", "/v1/alerts?"+q, h.AliceToken, nil, 200, &list)
		if list.Count != 0 {
			t.Errorf("filter %s matched %d alerts", q, list.Count)
		}
	}

	// The evidence events come back in full, event time and receive time apart.
	var d engine.AlertDetail
	e.JSON("GET", "/v1/alerts/"+a.ID, h.AliceToken, nil, 200, &d)
	if len(d.Events) != 9 || len(d.MissingEventRefs) != 0 {
		t.Fatalf("evidence: %d events, missing %v", len(d.Events), d.MissingEventRefs)
	}
	for i, ev := range d.Events {
		if !contains(a.EventRefs, ev.ID) {
			t.Errorf("event %s is not one the alert cites", ev.ID)
		}
		if !ev.ReceivedAt.Equal(e.Clock.Now()) || ev.EventTime.Equal(ev.ReceivedAt) {
			t.Errorf("event %s: event time %v, received %v", ev.ID, ev.EventTime, ev.ReceivedAt)
		}
		if i > 0 && ev.EventTime.Before(d.Events[i-1].EventTime) {
			t.Error("evidence is not in event-time order")
		}
	}
	if status, _ := e.Do("GET", "/v1/alerts/alr-missing", h.AliceToken, nil); status != 404 {
		t.Errorf("unknown alert: %d", status)
	}

	// Alice opens a case. The alert leaves the open queue.
	e.Clock.Advance(time.Minute)
	var c domain.Case
	e.JSON("POST", "/v1/alerts/"+a.ID+"/cases", h.AliceToken, map[string]string{"title": "Stacked offers in SIMA", "note": "picked up from the queue"}, 201, &c)
	if c.ID != "case-000001" || c.Status != domain.CaseOpen || c.CreatedBy != "alice" || fmt.Sprint(c.AlertIDs) != "["+a.ID+"]" {
		t.Fatalf("case: %+v", c)
	}
	status, raw := e.Do("POST", "/v1/alerts/"+a.ID+"/cases", h.BobToken, nil)
	if status != 409 || errCode(t, raw) != "CONFLICT" {
		t.Errorf("opening a second case on an alert under review: %d %s", status, raw)
	}
	e.JSON("GET", "/v1/alerts?status=open", h.AliceToken, nil, 200, &list)
	if list.Count != 0 {
		t.Errorf("%d alerts still open", list.Count)
	}
	e.JSON("GET", "/v1/alerts?status=in_case", h.AliceToken, nil, 200, &list)
	if list.Count != 1 || fmt.Sprint(list.Alerts[0].CaseIDs) != "[case-000001]" {
		t.Errorf("in-case list: %+v", list)
	}

	// The analyst is whoever holds the token. A request cannot name
	// someone else: there is no such field.
	status, raw = e.Do("POST", "/v1/cases/"+c.ID+"/notes", h.BobToken, map[string]string{"text": "x", "actor": "alice"})
	if status != 400 || errCode(t, raw) != "MALFORMED_REQUEST" {
		t.Errorf("a note naming its own actor: %d %s", status, raw)
	}

	// Workflow errors map to distinct statuses.
	status, _ = e.Do("POST", "/v1/cases/"+c.ID+"/resolve", h.BobToken, map[string]string{"disposition": "FALSE_POSITIVE", "summary": "s"})
	if status != 409 {
		t.Errorf("resolving before triage: %d", status)
	}
	status, _ = e.Do("POST", "/v1/cases/"+c.ID+"/triage", h.AliceToken, map[string]string{"priority": "urgent"})
	if status != 422 {
		t.Errorf("invalid priority: %d", status)
	}
	if status, _ = e.Do("POST", "/v1/cases/case-000099/notes", h.BobToken, map[string]string{"text": "x"}); status != 404 {
		t.Errorf("note on an unknown case: %d", status)
	}
	if status, _ = e.Do("POST", "/v1/cases/"+c.ID+"/notes", h.BobToken, nil); status != 400 {
		t.Errorf("note with no body: %d", status)
	}

	e.Clock.Advance(time.Minute)
	e.JSON("POST", "/v1/cases/"+c.ID+"/triage", h.AliceToken, map[string]string{"priority": "p2"}, 200, &c)
	if c.Status != domain.CaseInReview || c.Assignee != "alice" || c.Priority != "P2" {
		t.Errorf("triage: %+v", c)
	}
	e.Clock.Advance(time.Minute)
	e.JSON("POST", "/v1/cases/"+c.ID+"/notes", h.BobToken, map[string]string{"text": "Sells were 10-13 ticks away; never at risk of trading."}, 200, &c)
	e.Clock.Advance(time.Minute)
	e.JSON("POST", "/v1/cases/"+c.ID+"/resolve", h.BobToken, map[string]string{"disposition": "referred_for_review", "summary": "Pattern repeats; passing to compliance."}, 200, &c)
	if c.Status != domain.CaseResolved || c.Disposition != domain.DispReferred || len(c.Notes) != 2 {
		t.Errorf("resolved case: %+v", c)
	}
	e.JSON("GET", "/v1/alerts/"+a.ID, h.AliceToken, nil, 200, &d)
	if d.Alert.Status != domain.AlertClosed {
		t.Errorf("alert status after its case resolved: %s", d.Alert.Status)
	}

	// Evidence export.
	e.Clock.Advance(time.Minute)
	req, _ := http.NewRequest("GET", e.Server.URL+"/v1/cases/"+c.ID+"/evidence", nil)
	req.Header.Set("Authorization", "Bearer "+h.BobToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), "case-000001-evidence.json") {
		t.Fatalf("export: %d %v", res.StatusCode, res.Header)
	}
	var b cases.Bundle
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.ExportedBy != "bob" || b.Disclaimer != domain.Disclaimer || !b.AuditChain.Verified || b.BundleHash == "" {
		t.Errorf("bundle: by %s chain %+v", b.ExportedBy, b.AuditChain)
	}
	// The snapshot is the alert as it stood when the case was opened,
	// with every cited event in full, and its hash checks out.
	if len(b.Evidence) != 1 || len(b.Evidence[0].Events) != 9 || b.Evidence[0].Alert.Status != domain.AlertOpen {
		t.Fatalf("snapshot: %+v", b.Evidence)
	}
	if cases.SnapshotHash(b.Evidence[0]) != b.Evidence[0].Hash {
		t.Error("exported snapshot does not match its hash")
	}
	var trail []string
	for _, ev := range b.AuditTrail {
		trail = append(trail, ev.Action+":"+ev.Actor)
	}
	if got := strings.Join(trail, " "); got != "CASE_OPENED:alice TRIAGED:alice NOTE_ADDED:bob RESOLVED:bob" {
		t.Errorf("audit trail: %s", got)
	}
	// Audit timestamps are the times the actions were taken.
	if !b.AuditTrail[0].Time.Equal(h.T0.Add(time.Hour+time.Minute)) || !b.AuditTrail[3].Time.Equal(h.T0.Add(time.Hour+4*time.Minute)) {
		t.Errorf("audit times: %v .. %v", b.AuditTrail[0].Time, b.AuditTrail[3].Time)
	}

	// The export is on the record too, and the chain verifies end to end.
	e.JSON("GET", "/v1/cases/"+c.ID, h.AliceToken, nil, 200, &c)
	if last := c.AuditEvents[len(c.AuditEvents)-1]; last.Action != cases.ActExported || last.Actor != "bob" {
		t.Errorf("last audit event: %+v", last)
	}
	var chain cases.ChainStatus
	e.JSON("GET", "/v1/audit/verify", h.AliceToken, nil, 200, &chain)
	if !chain.Verified || chain.Events != 5 {
		t.Errorf("chain: %+v", chain)
	}

	var st engine.Stats
	e.JSON("GET", "/v1/stats", h.AliceToken, nil, 200, &st)
	if st.Cases.Total != 1 || st.Cases.ResolutionRate != 1 || st.Cases.MeanResolveSecs != 180 || st.Alerts.ByStatus[domain.AlertClosed] != 1 {
		t.Errorf("stats: %+v %+v", st.Cases, st.Alerts)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestBatchIsJudgedEventByEvent(t *testing.T) {
	e := h.New(t)
	future := e.Clock.Now().Add(time.Hour).Format(time.RFC3339)
	body := `[
	  {"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o1","action":"NEW","side":"BUY","qty":100,"price":"10.00","ts":"2026-01-05T15:00:00Z"},
	  {"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o2","action":"NEW","side":"BUY","price":"10.00","ts":"2026-01-05T15:00:01Z"},
	  "not an object",
	  {"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o3","action":"NEW","side":"BUY","qty":100,"price":"10.00","ts":"2026-01-05T15:00:02Z","colour":"red"},
	  {"type":"trade","venue":"NOWHERE","symbol":"SIMA","trade_id":"t1","buy_account":"A","sell_account":"B","qty":1,"price":"10.00","ts":"2026-01-05T15:00:03Z"},
	  {"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o4","action":"NEW","side":"BUY","qty":100,"price":"10.00","ts":"` + future + `"},
	  {"type":"trade","venue":"XSIM","symbol":"SIMA","trade_id":"t2","buy_account":"A","sell_account":"B","qty":1,"price":10.5,"ts":"2026-01-05T15:00:04Z"}
	]`
	var res engine.IngestResult
	e.JSON("POST", "/v1/events", h.IngestToken, body, 200, &res)
	if res.Accepted != 2 {
		t.Errorf("accepted %d, want the first and the last", res.Accepted)
	}
	got := map[int]string{}
	for _, r := range res.Rejected {
		got[r.Index] = r.Code
	}
	want := map[int]string{1: normalize.CodeInvalidQty, 2: normalize.CodeMalformed, 3: normalize.CodeMalformed,
		4: normalize.CodeUnknownVenue, 5: normalize.CodeFutureTS}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rejections %v, want %v", got, want)
	}

	// Refused events go to the dead-letter log with the reason and the text.
	dead, err := os.ReadFile(filepath.Join(e.Dir, "rejects.jsonl"))
	if err != nil || bytes.Count(dead, []byte("\n")) != 5 || !bytes.Contains(dead, []byte(`colour`)) || !bytes.Contains(dead, []byte(normalize.CodeFutureTS)) {
		t.Errorf("dead-letter log: %v\n%s", err, dead)
	}

	// A single object is a batch of one.
	e.JSON("POST", "/v1/events", h.IngestToken,
		`{"type":"order","venue":"XSIM","symbol":"SIMA","account_id":"A","order_id":"o9","action":"NEW","side":"BUY","qty":1,"price":"10.00","ts":"2026-01-05T15:00:05Z"}`, 200, &res)
	if res.Accepted != 1 {
		t.Errorf("single object: %+v", res)
	}

	// Bodies that are not a batch at all are a 400, not a pile of rejects.
	for _, bad := range []string{``, `"x"`, `[{"type":"order"}`, `{"type":`, `42`} {
		if status, raw := e.Do("POST", "/v1/events", h.IngestToken, bad); status != 400 || errCode(t, raw) != "MALFORMED_REQUEST" {
			t.Errorf("body %q: %d %s", bad, status, raw)
		}
	}
	big := "[" + strings.Repeat("{},", 5000) + "{}]"
	if status, raw := e.Do("POST", "/v1/events", h.IngestToken, big); status != 413 || errCode(t, raw) != "BATCH_TOO_LARGE" {
		t.Errorf("oversized batch: %d %s", status, raw)
	}

	var st engine.Stats
	e.JSON("GET", "/v1/stats", h.AliceToken, nil, 200, &st)
	if st.Events.Total != 3 || st.Events.RejectedSinceStart[normalize.CodeMalformed] != 2 {
		t.Errorf("stats: %+v", st.Events)
	}
}

// view is the part of an alert that must not change across restarts.
func view(alerts []domain.Alert) string {
	b, _ := json.MarshalIndent(alerts, "", " ")
	return string(b)
}

func TestRestartRecoversEverything(t *testing.T) {
	e := h.New(t)
	evs := append(h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second), h.Burst("Q1", h.At(500), 30, 30)...)
	e.MustAccept(evs...)
	e.Flush()
	if len(e.AlertsOf(rules.LayeringLike)) != 1 || len(e.AlertsOf(rules.HighCancelRate)) != 1 {
		t.Fatalf("setup: alerts %v", e.Alerts())
	}
	lay := e.AlertsOf(rules.LayeringLike)[0]
	c, err := e.Engine.OpenCase("alice", lay.ID, "", "n")
	if err != nil {
		t.Fatal(err)
	}
	e.Engine.Triage("alice", c.ID, "P1", "bob")

	snap := func() (string, string, engine.Stats) {
		cs, _ := json.Marshal(e.Engine.Cases(""))
		return view(e.Alerts()), string(cs), e.Engine.Stats()
	}
	alerts, cs, st := snap()
	features := len(e.Engine.Features("", "", 1000))

	e.Restart()
	alerts2, cs2, st2 := snap()
	if alerts2 != alerts {
		t.Errorf("alerts changed across a restart:\n before %s\n after  %s", alerts, alerts2)
	}
	if cs2 != cs {
		t.Errorf("cases changed across a restart:\n before %s\n after  %s", cs, cs2)
	}
	for name, pair := range map[string][2]any{"events": {st.Events, st2.Events}, "alerts": {st.Alerts, st2.Alerts}, "stream": {st.Stream, st2.Stream}} {
		before, _ := json.Marshal(pair[0])
		after, _ := json.Marshal(pair[1])
		if string(before) != string(after) {
			t.Errorf("%s stats changed across a restart:\n before %s\n after  %s", name, before, after)
		}
	}
	if got := len(e.Engine.Features("", "", 1000)); got != features || got == 0 {
		t.Errorf("feature history: %d windows before, %d after", features, got)
	}
	// The evidence is still readable from the journal.
	d, err := e.Engine.Alert(lay.ID)
	if err != nil || len(d.Events) != 9 || d.Alert.Status != domain.AlertInCase {
		t.Errorf("alert detail after restart: %+v %v", d.Alert, err)
	}

	// Redelivery after a restart is still recognised.
	res := e.Ingest(evs...)
	if res.Duplicates != len(evs) || res.Accepted != 0 {
		t.Errorf("redelivery after restart: %+v", res)
	}

	// Suppression state survived: a repeat burst by Q1 inside the
	// cooldown folds into the alert raised before the restart.
	before := e.AlertsOf(rules.HighCancelRate)[0]
	e.MustAccept(h.Burst("Q1", h.At(120000), 30, 30)...)
	e.Flush()
	after := e.AlertsOf(rules.HighCancelRate)
	if len(after) != 1 || after[0].ID != before.ID || after[0].Occurrences <= before.Occurrences {
		t.Errorf("repeat after restart: %d alerts, occurrences %d -> %d", len(after), before.Occurrences, after[0].Occurrences)
	}
	// And the case workflow carries on where it stopped.
	if _, err := e.Engine.Resolve("bob", c.ID, domain.DispInconclusive, "not enough to go on"); err != nil {
		t.Errorf("resolve after restart: %v", err)
	}
	if st := e.Engine.AuditVerify(); !st.Verified || st.Events != 3 {
		t.Errorf("chain after restart: %+v", st)
	}
}

func TestRulesReload(t *testing.T) {
	e := h.New(t)
	old := layeringAlert(t, e, "L1", h.At(0))

	// A broken file is refused and the old ruleset stays in force.
	os.WriteFile(e.RulesPath, []byte(`{"version":"broken"`), 0o600)
	status, raw := e.Do("POST", "/v1/rules/reload", h.AdminToken, nil)
	if status != 422 || errCode(t, raw) != "INVALID_RULESET" {
		t.Errorf("reload of a broken file: %d %s", status, raw)
	}
	bad := h.DefaultRules()
	bad.Rules.WashTradeLike.MinSelfTrades = 1
	e.WriteRules(bad)
	if status, _ = e.Do("POST", "/v1/rules/reload", h.AdminToken, nil); status != 422 {
		t.Errorf("reload of an invalid ruleset: %d", status)
	}
	var info engine.RulesetInfo
	e.JSON("GET", "/v1/rules", h.AliceToken, nil, 200, &info)
	if info.Version != "test-1" || info.Hash != old.RulesetHash {
		t.Errorf("ruleset after failed reloads: %s %s", info.Version, info.Hash)
	}

	// A valid new version: the layering rule now needs a 30x imbalance.
	next := h.DefaultRules()
	next.Version = "test-2"
	next.Rules.LayeringLike.Version = 2
	next.Rules.LayeringLike.MinImbalanceRatio = 30
	next.Rules.LayeringLike.MediumMinImbalanceRatio = 40
	next.Rules.LayeringLike.HighMinImbalanceRatio = 50
	e.WriteRules(next)
	e.JSON("POST", "/v1/rules/reload", h.AdminToken, nil, 200, &info)
	if !info.Changed || info.Version != "test-2" || info.Hash == old.RulesetHash {
		t.Errorf("reload: %+v", info)
	}
	e.JSON("POST", "/v1/rules/reload", h.AdminToken, nil, 200, &info)
	if info.Changed {
		t.Error("reloading an unchanged file reported a change")
	}

	// The same 20x pattern no longer fires; a 40x one does, and its
	// alert names the new rule version and ruleset.
	e.MustAccept(h.Layering("L2", h.At(200000), 4, 1000, 200, 4, time.Second)...)
	e.MustAccept(h.Layering("L3", h.At(200000), 4, 2000, 200, 4, time.Second)...)
	e.Flush()
	got := e.AlertsOf(rules.LayeringLike)
	if len(got) != 2 || got[1].Accounts[0] != "L3" {
		t.Fatalf("alerts after reload: %+v", got)
	}
	if got[1].RuleVersion != 2 || got[1].RulesetVersion != "test-2" || got[1].Thresholds["min_imbalance_ratio"] != 30 || got[1].RulesetHash != info.Hash {
		t.Errorf("new alert: %+v", got[1])
	}
	// The earlier alert is untouched: it still says what it was raised under.
	if got[0].ID != old.ID || got[0].RuleVersion != 1 || got[0].RulesetVersion != "test-1" || got[0].Thresholds["min_imbalance_ratio"] != 4 {
		t.Errorf("old alert rewritten: %+v", got[0])
	}

	// Across a restart each alert keeps its own ruleset, and replay does
	// not re-judge the L2 pattern under different thresholds.
	before := view(e.Alerts())
	e.Restart()
	if after := view(e.Alerts()); after != before {
		t.Errorf("alerts changed across a restart after a reload:\n before %s\n after  %s", before, after)
	}
	if rs := e.Engine.Ruleset(); rs.Version != "test-2" {
		t.Errorf("ruleset after restart: %s", rs.Version)
	}

	// Starting with a different ruleset file activates it for new input
	// only; history is not re-decided under it.
	e.WriteRules(h.DefaultRules())
	e.Restart()
	if rs := e.Engine.Ruleset(); rs.Version != "test-1" {
		t.Errorf("ruleset after restart with the old file: %s", rs.Version)
	}
	if after := view(e.Alerts()); after != before {
		t.Errorf("a ruleset change at startup rewrote history:\n before %s\n after  %s", before, after)
	}
}

func TestSimulatorRoute(t *testing.T) {
	e := h.New(t, h.WithSim())
	// Simulated time may run ahead of the wall clock: put the clock at
	// "now" so that is exercised.
	var names struct{ Scenarios []string }
	e.JSON("GET", "/v1/sim/scenarios", h.AdminToken, nil, 200, &names)
	if len(names.Scenarios) != 7 {
		t.Errorf("scenarios: %v", names.Scenarios)
	}
	type simRes struct {
		Simulated bool
		Events    int
		Scenario  struct {
			Name, Label string
			Accounts    []string
		}
		Ingest engine.IngestResult
		Flush  engine.FlushResult
	}
	want := map[string]string{"layering_like": rules.LayeringLike, "wash_trade_like": rules.WashTradeLike,
		"high_cancel_rate": rules.HighCancelRate, "quote_flicker_like": rules.HighOrderTradeRatio}
	// Back to back, with no waiting in between: nothing may be late.
	for _, name := range []string{"layering_like", "wash_trade_like", "high_cancel_rate", "quote_flicker_like", "normal_flow"} {
		var res simRes
		e.JSON("POST", "/v1/sim/scenarios/"+name, h.AdminToken, map[string]any{"seed": 3}, 200, &res)
		if !res.Simulated || res.Events == 0 || res.Ingest.Accepted != res.Events || res.Ingest.Late != 0 || len(res.Ingest.Rejected) != 0 {
			t.Fatalf("%s: %+v", name, res)
		}
		created := append(res.Ingest.AlertsCreated, res.Flush.AlertsCreated...)
		rule, positive := want[name]
		found := false
		for _, id := range created {
			d, err := e.Engine.Alert(id)
			if err != nil {
				t.Fatal(err)
			}
			found = found || d.Alert.RuleID == rule
			if !contains(res.Scenario.Accounts, d.Alert.Accounts[0]) {
				t.Errorf("%s raised an alert on %v, not one of its accounts", name, d.Alert.Accounts)
			}
		}
		if positive && !found {
			t.Errorf("%s: no %s alert among %v", name, rule, created)
		}
	}
	// An empty body is fine; an unknown scenario is a 404.
	e.JSON("POST", "/v1/sim/scenarios/benign_market_maker", h.AdminToken, nil, 200, nil)
	if status, raw := e.Do("POST", "/v1/sim/scenarios/spoofing", h.AdminToken, nil); status != 404 || errCode(t, raw) != "UNKNOWN_SCENARIO" {
		t.Errorf("unknown scenario: %d %s", status, raw)
	}
	if st := e.Engine.Stats(); st.Events.Late != 0 {
		t.Errorf("%d simulated events were late", st.Events.Late)
	}
}

func TestMetricsAndDashboard(t *testing.T) {
	e := h.New(t)
	a := layeringAlert(t, e, "L1", h.At(0))
	c, _ := e.Engine.OpenCase("alice", a.ID, "", "")
	e.Engine.Triage("alice", c.ID, "P1", "alice")
	e.Clock.Advance(10 * time.Minute)
	e.Engine.Resolve("alice", c.ID, domain.DispFalsePositive, "noise")
	e.Do("POST", "/v1/events", h.IngestToken, `{"type":"quote"}`)

	_, raw := e.Do("GET", "/metrics", "", nil)
	text := string(raw)
	for _, want := range []string{
		"surv_events_total 11\n",
		"surv_events_late_total 0\n",
		`surv_events_rejected_total{code="UNKNOWN_EVENT_TYPE"} 1` + "\n",
		`surv_alerts_total{rule="LAYERING_LIKE"} 1` + "\n",
		`surv_alerts_by_status{status="CLOSED"} 1` + "\n",
		"surv_alerts_per_million_events 90909.09",
		"surv_duplicate_alert_ratio 0\n",
		"surv_event_to_alert_seconds_count 1\n",
		`surv_cases{status="RESOLVED"} 1` + "\n",
		`surv_case_resolutions{disposition="FALSE_POSITIVE"} 1` + "\n",
		"surv_case_resolution_ratio 1\n",
		`surv_case_resolution_seconds_bucket{le="900"} 1` + "\n",
		`surv_case_resolution_seconds_bucket{le="300"} 0` + "\n",
		"surv_watermark_lag_seconds", "surv_buffered_events 0\n", "surv_journal_errors_total 0\n",
		`surv_ruleset_info{version="test-1"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// The dashboard is static, self-contained and locked down.
	for path, kind := range map[string]string{"/": "text/html", "/app.js": "text/javascript", "/app.css": "text/css"} {
		res, err := http.Get(e.Server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), kind) ||
			!strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: %d %s csp=%q", path, res.StatusCode, res.Header.Get("Content-Type"), csp)
		}
	}
	_, page := e.Do("GET", "/", "", nil)
	if !bytes.Contains(page, []byte("Simulated data")) || !bytes.Contains(page, []byte("investigation leads")) {
		t.Error("the dashboard does not label itself as simulated and as leads only")
	}
	// No token, and no way to build markup from data, in the script.
	_, js := e.Do("GET", "/app.js", "", nil)
	for _, banned := range []string{"innerHTML", "outerHTML", "document.write", "localStorage", "sessionStorage", "eval("} {
		if bytes.Contains(js, []byte(banned)) {
			t.Errorf("app.js uses %s", banned)
		}
	}
}

func TestConcurrentIngestAndRead(t *testing.T) {
	e := h.New(t)
	const writers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers hammer the read side while writers ingest.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					e.Engine.Alerts(engine.AlertFilter{})
					e.Engine.Stats()
					e.Engine.Features("", "", 50)
					e.Do("GET", "/metrics", "", nil)
				}
			}
		}()
	}
	var ww sync.WaitGroup
	for w := 0; w < writers; w++ {
		ww.Add(1)
		go func(w int) {
			defer ww.Done()
			evs := h.Burst(fmt.Sprintf("Q%d", w), h.At(0), 30, 30)
			for len(evs) > 0 {
				n := min(7, len(evs))
				status, raw := e.Do("POST", "/v1/events", h.IngestToken, evs[:n])
				if status != 200 {
					t.Errorf("writer %d: %d %s", w, status, raw)
					return
				}
				evs = evs[n:]
			}
		}(w)
	}
	ww.Wait()
	e.Flush()
	close(stop)
	wg.Wait()

	st := e.Engine.Stats()
	if st.Events.Total != writers*60 || st.Events.Late != 0 {
		t.Errorf("events: %+v", st.Events)
	}
	// Whatever the interleaving, each account gets exactly one alert per
	// rule, with every one of its orders counted.
	for _, rule := range []string{rules.HighCancelRate, rules.HighOrderTradeRatio} {
		got := e.AlertsOf(rule)
		if len(got) != writers {
			t.Errorf("%s: %d alerts, want %d", rule, len(got), writers)
		}
		for _, a := range got {
			if a.Observed["new_orders"] != 30 || a.Observed["cancels"] != 30 || len(a.EventRefs) != 60 {
				t.Errorf("%s on %v: observed %v, %d refs", rule, a.Accounts, a.Observed, len(a.EventRefs))
			}
		}
	}
}
