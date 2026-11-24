// Package scenarios holds the failure-mode and replay tests: what the
// engine does when input is late, duplicated, out of order, wrongly
// stamped or incomplete, and when its own storage refuses writes.
package scenarios

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"surveillance/src/domain"
	"surveillance/src/engine"
	"surveillance/src/journal"
	"surveillance/src/normalize"
	"surveillance/src/rules"
	h "surveillance/tests/harness"
)

func ts(t testing.TB, r normalize.Raw) time.Time {
	t.Helper()
	at, err := normalize.ParseTime(r.TS)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// inOrder merges event lists into event-time order.
func inOrder(t testing.TB, lists ...[]normalize.Raw) []normalize.Raw {
	t.Helper()
	var all []normalize.Raw
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return ts(t, all[i]).Before(ts(t, all[j])) })
	return all
}

// disorder reverses each run of events spanning at most span, so events
// arrive up to span out of order and no further.
func disorder(t testing.TB, evs []normalize.Raw, span time.Duration) []normalize.Raw {
	t.Helper()
	out := make([]normalize.Raw, 0, len(evs))
	for i := 0; i < len(evs); {
		j := i
		for j < len(evs) && ts(t, evs[j]).Sub(ts(t, evs[i])) <= span {
			j++
		}
		for k := j - 1; k >= i; k-- {
			out = append(out, evs[k])
		}
		i = j
	}
	return out
}

// selfTrades builds n trades in which one account is on both sides.
func selfTrades(acct string, start time.Time, n int) []normalize.Raw {
	var evs []normalize.Raw
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * 2 * time.Second)
		b, s := fmt.Sprintf("%s-wb%d", acct, i), fmt.Sprintf("%s-ws%d", acct, i)
		evs = append(evs,
			h.NewOrder(acct, b, "BUY", 300, "50.00", at.Add(-80*time.Millisecond)),
			h.NewOrder(acct, s, "SELL", 300, "50.00", at.Add(-40*time.Millisecond)),
			h.Trade(acct, b, acct, s, 300, "50.00", at))
	}
	return evs
}

// session is a mixed feed that fires every rule once.
func session(t testing.TB) []normalize.Raw {
	return inOrder(t,
		h.Layering("L1", h.At(0), 5, 1000, 200, 5, time.Second),
		h.Burst("Q1", h.At(300), 40, 40),
		selfTrades("W1", h.At(1000), 3),
		h.Burst("OK1", h.At(700), 30, 5), // unremarkable: most orders kept
	)
}

// picture renders alerts in a canonical order for comparison.
func picture(alerts []domain.Alert) string {
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].ID < alerts[j].ID })
	b, _ := json.MarshalIndent(alerts, "", " ")
	return string(b)
}

// clean runs events through a fresh engine with nothing going wrong.
func clean(t *testing.T, evs []normalize.Raw) (*h.Env, string) {
	t.Helper()
	e := h.New(t)
	e.MustAccept(evs...)
	e.Flush()
	got := picture(e.Alerts())
	var fired []string
	for _, a := range e.Alerts() {
		fired = append(fired, a.RuleID+":"+a.Accounts[0])
	}
	sort.Strings(fired)
	want := "[HIGH_CANCEL_RATE:Q1 HIGH_ORDER_TRADE_RATIO:Q1 LAYERING_LIKE:L1 WASH_TRADE_LIKE:W1]"
	if fmt.Sprint(fired) != want {
		t.Fatalf("the reference session fired %v, want %v", fired, want)
	}
	return e, got
}

func equal(t *testing.T, what, want, got string) {
	t.Helper()
	if want != got {
		t.Errorf("%s: alerts differ from the clean run\n--- clean\n%s\n--- got\n%s", what, want, got)
	}
}

func TestOutOfOrderDeliveryWithinLatenessChangesNothing(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)

	for _, span := range []time.Duration{500 * time.Millisecond, 2 * time.Second, 4900 * time.Millisecond} {
		e := h.New(t)
		shuffled := disorder(t, evs, span)
		moved := 0
		for i := range evs {
			if evs[i].EventID != shuffled[i].EventID || evs[i].TradeID != shuffled[i].TradeID {
				moved++
			}
		}
		if moved < len(evs)/2 {
			t.Fatalf("disorder(%v) moved only %d of %d events", span, moved, len(evs))
		}
		res := e.MustAccept(shuffled...)
		if res.Late != 0 {
			t.Fatalf("%v of disorder made %d events late", span, res.Late)
		}
		e.Flush()
		equal(t, fmt.Sprintf("%v of disorder", span), want, picture(e.Alerts()))
	}
}

func TestLateAndStaleEventsAreKeptButNeverEvaluated(t *testing.T) {
	e := h.New(t)
	e.MustAccept(h.Burst("Q1", h.At(0), 30, 30)...)
	// An event at +60s moves the watermark to +55s.
	e.MustAccept(h.NewOrder("X", "x1", "BUY", 100, "10.00", h.At(60000)))
	if st := e.Engine.Stats(); !st.Stream.Watermark.Equal(h.At(55000)) {
		t.Fatalf("watermark %v, want T0+55s", st.Stream.Watermark)
	}

	// The boundary: an event exactly at the watermark is on time, one
	// millisecond behind it is late.
	if res := e.Ingest(h.NewOrder("X", "x2", "BUY", 100, "10.00", h.At(55000))); res.Accepted != 1 || res.Late != 0 {
		t.Errorf("event at the watermark: %+v", res)
	}
	if res := e.Ingest(h.NewOrder("X", "x3", "BUY", 100, "10.00", h.At(54999))); res.Accepted != 0 || res.Late != 1 {
		t.Errorf("event 1ms behind the watermark: %+v", res)
	}

	// A stale burst: fifty-odd seconds old on arrival. Fed on time it
	// would raise two alerts; late, it raises none.
	stale := h.Burst("Q2", h.At(1000), 30, 30)
	res := e.Ingest(stale...)
	if res.Late != len(stale) || res.Accepted != 0 || len(res.Rejected) != 0 {
		t.Fatalf("stale burst: %+v", res)
	}
	e.Flush()
	for _, a := range e.Alerts() {
		if a.Accounts[0] != "Q1" {
			t.Errorf("alert on %v built from late events", a.Accounts)
		}
	}
	if len(e.AlertsOf(rules.HighCancelRate)) != 1 {
		t.Errorf("alerts: %v", e.Alerts())
	}
	for _, fw := range e.Engine.Features("Q2", "", 100) {
		t.Errorf("late events reached a feature window: %+v", fw)
	}

	// Late events are not thrown away: they are journalled, flagged, and
	// counted, and a redelivery is still recognised.
	st := e.Engine.Stats()
	if st.Events.Late != uint64(len(stale))+1 || st.Events.Total != 60+2+1+uint64(len(stale)) {
		t.Errorf("counts: %+v", st.Events)
	}
	raw, _ := os.ReadFile(filepath.Join(e.Dir, "journal.jsonl"))
	if n := bytes.Count(raw, []byte(`"late":true`)); n != len(stale)+1 {
		t.Errorf("%d journal records flagged late, want %d", n, len(stale)+1)
	}
	if res := e.Ingest(stale...); res.Duplicates != len(stale) {
		t.Errorf("redelivered late events: %+v", res)
	}
	// Still late, still out of detection, after a restart.
	e.Restart()
	if st := e.Engine.Stats(); st.Events.Late != uint64(len(stale))+1 || st.Alerts.Total != 2 {
		t.Errorf("after restart: %+v %+v", st.Events, st.Alerts)
	}
}

func TestClockSkew(t *testing.T) {
	e := h.New(t)
	now := h.At(100000)
	e.Clock.Set(now)
	order := func(id string, at time.Time) normalize.Raw { return h.NewOrder("A", id, "BUY", 100, "10.00", at) }

	// A source whose clock runs ahead: tolerated up to max_future_skew_ms
	// (2s), refused beyond it.
	if res := e.Ingest(order("a1", now.Add(2*time.Second))); res.Accepted != 1 {
		t.Errorf("event 2s ahead of the local clock: %+v", res)
	}
	res := e.Ingest(order("a2", now.Add(2001*time.Millisecond)), order("a3", now.Add(24*time.Hour)))
	if len(res.Rejected) != 2 || res.Rejected[0].Code != normalize.CodeFutureTS || res.Rejected[1].Code != normalize.CodeFutureTS {
		t.Fatalf("events from the future: %+v", res)
	}
	// The refused timestamp must not have dragged the watermark forward:
	// had it done so, everything on time would now be late.
	st := e.Engine.Stats()
	if !st.Stream.MaxEventTime.Equal(now.Add(2 * time.Second)) {
		t.Errorf("newest event time %v", st.Stream.MaxEventTime)
	}
	if res := e.Ingest(order("a4", now), order("a5", now.Add(-2*time.Second))); res.Accepted != 2 || res.Late != 0 {
		t.Errorf("on-time events after a future one was refused: %+v", res)
	}

	// A source whose clock runs behind is indistinguishable from delay:
	// inside the allowed lateness (5s behind the newest event) its events
	// count, beyond it they are late.
	if res := e.Ingest(order("b1", now.Add(-3*time.Second))); res.Accepted != 1 {
		t.Errorf("event from a clock 3s behind: %+v", res)
	}
	if res := e.Ingest(order("b2", now.Add(-3001*time.Millisecond))); res.Late != 1 {
		t.Errorf("event from a clock too far behind: %+v", res)
	}

	// The refusals are in the dead-letter log with the clock reading.
	dead, _ := os.ReadFile(filepath.Join(e.Dir, "rejects.jsonl"))
	if bytes.Count(dead, []byte(normalize.CodeFutureTS)) != 2 || !bytes.Contains(dead, []byte("ahead of the local clock")) {
		t.Errorf("dead-letter log: %s", dead)
	}
}

func TestDuplicateDelivery(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)

	e := h.New(t)
	// At-least-once delivery at its worst: every event sent twice in a
	// row, then the whole feed sent again.
	var doubled []normalize.Raw
	for _, ev := range evs {
		doubled = append(doubled, ev, ev)
	}
	res := e.Ingest(doubled...)
	if res.Accepted != len(evs) || res.Duplicates != len(evs) {
		t.Fatalf("doubled feed: %+v", res)
	}
	if res := e.Ingest(evs...); res.Duplicates != len(evs) || res.Accepted != 0 || res.Late != 0 {
		t.Fatalf("replayed feed: %+v", res)
	}
	e.Flush()
	if res := e.Ingest(evs...); res.Duplicates != len(evs) || len(res.AlertsCreated)+len(res.AlertsUpdated) != 0 {
		t.Fatalf("replayed feed after flush: %+v", res)
	}
	equal(t, "duplicate delivery", want, picture(e.Alerts()))
	if st := e.Engine.Stats(); st.Events.Total != uint64(len(evs)) || st.Events.DuplicatesSinceStart != uint64(3*len(evs)) {
		t.Errorf("counts: %+v", st.Events)
	}

	// A source that sends no event IDs is deduplicated on content.
	bare := h.NewOrder("Z", "z1", "BUY", 100, "10.00", h.At(200000))
	bare.EventID = ""
	if res := e.Ingest(bare, bare); res.Accepted != 1 || res.Duplicates != 1 {
		t.Errorf("content-identical events without IDs: %+v", res)
	}
	other := bare
	other.Qty = "101"
	if res := e.Ingest(other); res.Accepted != 1 {
		t.Errorf("a different event without an ID was treated as a duplicate: %+v", res)
	}
}

var errDisk = errors.New("simulated: no space left on device")

func TestJournalWriteFailureRefusesTheEventAndRecoversOnRetry(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)

	e := h.New(t)
	// The journal accepts 50 more records and then refuses writes.
	writes := 0
	e.Engine.SetJournalFault(func() error {
		if writes++; writes > 50 {
			return errDisk
		}
		return nil
	})
	status, raw := e.Do("POST", "/v1/events", h.IngestToken, evs)
	var body struct {
		Error  struct{ Code, Message string }
		Result engine.IngestResult
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusServiceUnavailable || body.Error.Code != "STORAGE_UNAVAILABLE" || !strings.Contains(body.Error.Message, "no space left") {
		t.Fatalf("ingest with a failing journal: %d %s", status, raw)
	}
	// The response says how far it got, and that is exactly what is stored.
	stored := body.Result.Accepted
	if stored == 0 || stored > 50 {
		t.Fatalf("stored %d events before the failure", stored)
	}
	st := e.Engine.Stats()
	if st.Events.Total != uint64(stored) || st.Journal.ErrorsSinceStart != 1 {
		t.Errorf("after the failure: %d events, %d errors", st.Events.Total, st.Journal.ErrorsSinceStart)
	}
	// While the journal is down nothing is accepted "in memory only".
	for i := 0; i < 3; i++ {
		if status, _ := e.Do("POST", "/v1/events", h.IngestToken, evs[stored:]); status != http.StatusServiceUnavailable {
			t.Fatalf("ingest during the outage: %d", status)
		}
		if status, _ := e.Do("POST", "/v1/ingest/flush", h.IngestToken, nil); status != http.StatusServiceUnavailable {
			t.Fatalf("flush during the outage: %d", status)
		}
	}
	if got := e.Engine.Stats().Events.Total; got != uint64(stored) {
		t.Errorf("%d events stored during the outage", got-uint64(stored))
	}
	// Reads keep working.
	if status, _ := e.Do("GET", "/v1/alerts", h.AliceToken, nil); status != 200 {
		t.Errorf("reads during the outage: %d", status)
	}

	// Storage comes back. The client resends the whole batch.
	e.Engine.SetJournalFault(nil)
	var res engine.IngestResult
	e.JSON("POST", "/v1/events", h.IngestToken, evs, 200, &res)
	if res.Duplicates != stored || res.Accepted != len(evs)-stored || res.Late != 0 {
		t.Fatalf("resend: %+v (stored %d of %d before)", res, stored, len(evs))
	}
	e.Flush()
	equal(t, "journal outage and retry", want, picture(e.Alerts()))
	e.Restart()
	equal(t, "journal outage, retry and restart", want, picture(e.Alerts()))
}

// An alert that was computed but could not be written must not be lost,
// and must not be followed by more input until it is safe.
func TestAlertRecordWriteFailureBlocksInputUntilItIsDurable(t *testing.T) {
	e := h.New(t)
	e.MustAccept(h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)...)
	// Flush writes the watermark advance, then the alert. Let the first
	// through and fail the second.
	writes := 0
	e.Engine.SetJournalFault(func() error {
		if writes++; writes > 1 {
			return errDisk
		}
		return nil
	})
	fl, err := e.Engine.Flush()
	if !errors.Is(err, engine.ErrJournal) || len(fl.AlertsCreated) != 1 {
		t.Fatalf("flush: %+v %v", fl, err)
	}
	id := fl.AlertsCreated[0]
	if st := e.Engine.Stats(); !st.Journal.PendingOutput {
		t.Fatal("no pending alert record")
	}
	next := h.NewOrder("A", "a1", "BUY", 100, "10.00", h.At(100000))
	if res, err := e.Engine.Ingest([]normalize.Raw{next}, false); !errors.Is(err, engine.ErrJournal) || res.Accepted != 0 {
		t.Fatalf("ingest while an alert record is pending: %+v %v", res, err)
	}
	if err := e.Engine.Tick(); !errors.Is(err, engine.ErrJournal) {
		t.Errorf("tick while an alert record is pending: %v", err)
	}
	if _, err := e.Engine.ReloadRules(); !errors.Is(err, engine.ErrJournal) {
		t.Errorf("reload while an alert record is pending: %v", err)
	}
	before, _ := e.Engine.Alert(id)

	// Storage comes back: the pending record goes out first.
	e.Engine.SetJournalFault(nil)
	e.MustAccept(next)
	if st := e.Engine.Stats(); st.Journal.PendingOutput {
		t.Error("alert record still pending")
	}
	e.Restart()
	after, err := e.Engine.Alert(id)
	if err != nil {
		t.Fatalf("alert lost: %v", err)
	}
	if picture([]domain.Alert{before.Alert}) != picture([]domain.Alert{after.Alert}) || len(e.Alerts()) != 1 {
		t.Errorf("alert changed:\n before %+v\n after  %+v", before.Alert, after.Alert)
	}
}

// The process dies after journalling an input and before journalling the
// alerts it produced.
func TestCrashBetweenInputAndAlertRecord(t *testing.T) {
	e := h.New(t)
	e.MustAccept(h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)...)
	writes := 0
	e.Engine.SetJournalFault(func() error {
		if writes++; writes > 1 {
			return errDisk
		}
		return nil
	})
	fl, err := e.Engine.Flush()
	if !errors.Is(err, engine.ErrJournal) || len(fl.AlertsCreated) != 1 {
		t.Fatalf("flush: %+v %v", fl, err)
	}
	want, _ := e.Engine.Alert(fl.AlertsCreated[0])

	// "Crash": the pending record dies with the process.
	e.Restart()
	got := e.Alerts()
	if len(got) != 1 {
		t.Fatalf("%d alerts after the crash, want the one that was pending", len(got))
	}
	if picture(got) != picture([]domain.Alert{want.Alert}) {
		t.Errorf("recovered alert differs:\n before %+v\n after  %+v", want.Alert, got[0])
	}
	// It was written during recovery, so a second restart finds it in
	// the journal and recomputes nothing.
	kinds := func() string {
		var ks []string
		raw, _ := os.ReadFile(filepath.Join(e.Dir, "journal.jsonl"))
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var rec journal.Record
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			ks = append(ks, rec.Kind)
		}
		return strings.Join(ks[len(ks)-2:], ",")
	}
	if got := kinds(); got != "advance,out" {
		t.Errorf("journal ends with %s, want advance,out", got)
	}
	size := e.Engine.Stats().Journal.Bytes
	e.Restart()
	if picture(e.Alerts()) != picture(got) || e.Engine.Stats().Journal.Bytes != size {
		t.Error("a second restart changed the alert or the journal")
	}
}

func TestTornJournalWrite(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)

	e := h.New(t)
	half := len(evs) / 2
	e.MustAccept(evs[:half]...)
	e.Restart() // close the file cleanly before damaging it
	path := filepath.Join(e.Dir, "journal.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Power is lost part-way through a record.
	f.WriteString(`{"k":"event","seq":9999,"at":"2026-01-05T16:00:00Z","event":{"event_id":"ord:XSIM:torn","ki`)
	f.Close()
	e.Restart()
	if st := e.Engine.Stats(); st.Events.Total != uint64(half) {
		t.Fatalf("%d events after recovering from a torn write, want %d", st.Events.Total, half)
	}
	e.MustAccept(evs[half:]...)
	e.Flush()
	equal(t, "torn write", want, picture(e.Alerts()))
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("torn")) {
		t.Error("the torn record is still in the journal")
	}

	// Damage in the middle is not a torn tail: refuse to start rather
	// than run on a history with a hole in it.
	e.Engine.Close()
	raw, _ = os.ReadFile(path)
	lines := bytes.SplitAfter(raw, []byte("\n"))
	lines[len(lines)/2] = []byte("{not json}\n")
	os.WriteFile(path, bytes.Join(lines, nil), 0o600)
	_, err = engine.Open(engine.Config{DataDir: e.Dir, RulesPath: e.RulesPath, RefDataPath: e.RefPath, Clock: e.Clock.Now})
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("engine started on a corrupt journal: %v", err)
	}
	// Put it back so the harness can shut down normally.
	os.WriteFile(path, raw, 0o600)
	e.Engine = nil
	e.Restart()
}

func TestConnectionLossMidBatch(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)
	e := h.New(t)

	// The connection drops while the body is in flight: the server sees
	// a truncated request. Nothing in it is stored.
	body, _ := json.Marshal(evs)
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.Server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /v1/events HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", h.IngestToken, len(body))
	conn.Write(body[:len(body)/2])
	conn.Close()
	// Give the handler a moment to see the connection close.
	time.Sleep(200 * time.Millisecond)
	if st := e.Engine.Stats(); st.Events.Total != 0 {
		t.Fatalf("%d events stored from a truncated request", st.Events.Total)
	}

	// The connection drops after the server stored the batch but before
	// the client saw the reply. The client cannot tell and resends: the
	// first two thirds twice, in effect.
	cut := len(evs) * 2 / 3
	e.JSON("POST", "/v1/events", h.IngestToken, evs[:cut], 200, nil)
	var res engine.IngestResult
	e.JSON("POST", "/v1/events", h.IngestToken, evs, 200, &res)
	if res.Duplicates != cut || res.Accepted != len(evs)-cut || res.Late != 0 {
		t.Fatalf("resend after a lost reply: %+v", res)
	}
	e.Flush()
	equal(t, "connection loss and resend", want, picture(e.Alerts()))
}

func TestMissingData(t *testing.T) {
	e := h.New(t)

	// Cancels and amends for orders whose entry was never received are
	// accepted and counted; they are part of the record.
	e.MustAccept(h.Cancel("M1", "never-seen", h.At(0)),
		h.Order("M1", "never-seen-2", "AMEND", "BUY", 100, "10.00", h.At(100)))

	// A layering pattern whose cancels were lost in transit. The engine
	// does not guess that the orders were pulled.
	full := h.Layering("L1", h.At(1000), 5, 1000, 200, 5, time.Second)
	var noCancels []normalize.Raw
	for _, ev := range full {
		if ev.Action != "CANCEL" {
			noCancels = append(noCancels, ev)
		}
	}
	e.MustAccept(noCancels...)

	// A layering pattern whose order entries were lost: cancels for
	// orders of unknown side, size and price prove nothing.
	full = h.Layering("L2", h.At(2000), 5, 1000, 200, 5, time.Second)
	var noEntries []normalize.Raw
	for _, ev := range full {
		if !(ev.Action == "NEW" && ev.Side == "SELL" && ev.AccountID == "L2") {
			noEntries = append(noEntries, ev)
		}
	}
	e.MustAccept(inOrder(t, noEntries)...)

	// A trade report with no order IDs still counts as a trade.
	tr := h.Trade("M2", "", "M3", "", 100, "10.00", h.At(3000))
	e.MustAccept(tr)
	e.Flush()
	if got := e.AlertsOf(rules.LayeringLike); len(got) != 0 {
		t.Errorf("layering alert raised on incomplete data: %+v", got)
	}
	fws := e.Engine.Features("M1", "", 10)
	if len(fws) == 0 || fws[0].Cancels != 1 || fws[0].Amends != 1 || fws[0].NewOrders != 0 || fws[0].CancelRate != nil {
		t.Errorf("features for orphan cancels: %+v", fws)
	}
	if fws := e.Engine.Features("M2", "", 10); len(fws) == 0 || fws[0].Trades != 1 {
		t.Errorf("features for a trade without order IDs: %+v", fws)
	}

	// The complete pattern, for contrast, does fire.
	e.MustAccept(h.Layering("L3", h.At(200000), 5, 1000, 200, 5, time.Second)...)
	e.Flush()
	if got := e.AlertsOf(rules.LayeringLike); len(got) != 1 || got[0].Accounts[0] != "L3" {
		t.Errorf("complete pattern: %+v", got)
	}

	// Events with a required field missing are refused one by one, with
	// the field named, and nothing else in the batch is held up.
	mk := func(edit func(*normalize.Raw)) normalize.Raw {
		r := h.NewOrder("M4", "m4", "BUY", 100, "10.00", h.At(300000))
		edit(&r)
		return r
	}
	res := e.Ingest(
		mk(func(r *normalize.Raw) { r.AccountID = "" }),
		mk(func(r *normalize.Raw) { r.TS = "" }),
		mk(func(r *normalize.Raw) { r.Price = "" }),
		mk(func(r *normalize.Raw) { r.Side = "" }),
		mk(func(r *normalize.Raw) { r.Symbol = "" }),
		mk(func(r *normalize.Raw) { r.TS = "2026-01-05 15:00:00" }),
		mk(func(r *normalize.Raw) {}),
	)
	if res.Accepted != 1 || len(res.Rejected) != 6 {
		t.Fatalf("batch with gaps: %+v", res)
	}
	for i, field := range []string{"account_id", "ts", "price", "side", "symbol", "offset"} {
		if !strings.Contains(res.Rejected[i].Detail, field) {
			t.Errorf("rejection %d does not name %s: %+v", i, field, res.Rejected[i])
		}
	}
}

func TestIdleStreamStillGetsEvaluated(t *testing.T) {
	e := h.New(t)
	// A live source: event time tracks the local clock.
	evs := h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)
	last := ts(t, evs[len(evs)-1])
	e.Clock.Set(last)
	e.MustAccept(evs...)
	// Then the source goes quiet. The pattern is complete, but the
	// watermark is 5s behind it and the cancel window has not closed.
	if len(e.Alerts()) != 0 {
		t.Fatal("alert before the watermark reached the end of the cancel window")
	}
	wm := func() time.Time { return *e.Engine.Stats().Stream.Watermark }
	if !wm().Equal(last.Add(-5 * time.Second)) {
		t.Fatalf("watermark %v", wm())
	}

	// Under the idle timeout (3s) nothing moves.
	e.Clock.Advance(2999 * time.Millisecond)
	e.Engine.Tick()
	if !wm().Equal(last.Add(-5 * time.Second)) {
		t.Errorf("watermark moved before the idle timeout: %v", wm())
	}
	// From then on the watermark follows the local clock, still held
	// back by the allowed lateness.
	e.Clock.Advance(time.Millisecond)
	e.Engine.Tick()
	if !wm().Equal(last.Add(-2 * time.Second)) {
		t.Errorf("watermark after 3s idle: %v, want newest - 5s + 3s", wm())
	}
	e.Clock.Advance(12 * time.Second)
	e.Engine.Tick()
	if !wm().Equal(last.Add(10 * time.Second)) {
		t.Errorf("watermark after 15s idle: %v", wm())
	}
	got := e.AlertsOf(rules.LayeringLike)
	if len(got) != 1 {
		t.Fatalf("no layering alert after the idle timer passed the cancel window: %v", e.Alerts())
	}
	// Event-to-alert latency was honest about the wait.
	if !got[0].FirstDetectedAt.Equal(last.Add(15 * time.Second)) {
		t.Errorf("detected at %v", got[0].FirstDetectedAt)
	}

	// The source resumes, stamping events with the current time. They
	// are on time: the idle advance never ran ahead of the clock.
	if res := e.Ingest(h.NewOrder("A", "a1", "BUY", 100, "10.00", e.Clock.Now())); res.Accepted != 1 {
		t.Errorf("first event after the quiet period: %+v", res)
	}
	// So is one that was in flight for 4s. One from before the quiet
	// period is late.
	if res := e.Ingest(h.NewOrder("A", "a2", "BUY", 100, "10.00", e.Clock.Now().Add(-4*time.Second))); res.Accepted != 1 {
		t.Errorf("event delayed 4s: %+v", res)
	}
	if res := e.Ingest(h.NewOrder("A", "a3", "BUY", 100, "10.00", last)); res.Late != 1 {
		t.Errorf("event from before the quiet period: %+v", res)
	}

	// A long silence: the watermark stops one window past the newest
	// event, and ticking after that writes nothing.
	e.Clock.Advance(time.Hour)
	e.Engine.Tick()
	newest := *e.Engine.Stats().Stream.MaxEventTime
	if !wm().Equal(newest.Add(60 * time.Second)) {
		t.Errorf("watermark after an hour idle: %v, newest event %v", wm(), newest)
	}
	size := e.Engine.Stats().Journal.Bytes
	for i := 0; i < 5; i++ {
		e.Clock.Advance(time.Minute)
		e.Engine.Tick()
	}
	if e.Engine.Stats().Journal.Bytes != size {
		t.Error("idle ticks keep writing to the journal")
	}

	// The idle advances were journalled, so a restart lands on the same
	// watermark and the same alerts.
	before := picture(e.Alerts())
	e.Restart()
	if !wm().Equal(newest.Add(60*time.Second)) || picture(e.Alerts()) != before {
		t.Errorf("after restart: watermark %v", wm())
	}
}

func TestRestartDoesNotCountAsIdleTime(t *testing.T) {
	e := h.New(t)
	evs := h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)
	last := ts(t, evs[len(evs)-1])
	e.Clock.Set(last)
	e.MustAccept(evs...)
	// The process is down for ten minutes. On the way up it must not
	// treat the outage as the source having gone quiet: the source may
	// be about to deliver what it queued meanwhile.
	e.Clock.Advance(10 * time.Minute)
	e.Restart()
	e.Engine.Tick()
	if wm := *e.Engine.Stats().Stream.Watermark; !wm.Equal(last.Add(-5 * time.Second)) {
		t.Fatalf("watermark jumped to %v on restart", wm)
	}
	// Queued events stamped during the first seconds of the outage are
	// still on time.
	if res := e.Ingest(h.Cancel("L1", "x", last.Add(time.Second))); res.Accepted != 1 {
		t.Errorf("event queued during the outage: %+v", res)
	}
}

func TestRestartMidStream(t *testing.T) {
	evs := session(t)
	_, want := clean(t, evs)
	shuffled := disorder(t, evs, 2*time.Second)

	// Restart at several points, including in the middle of each pattern
	// and with events still buffered behind the watermark.
	e := h.New(t)
	step := len(shuffled) / 7
	for i := 0; i < len(shuffled); i += step {
		e.MustAccept(shuffled[i:min(i+step, len(shuffled))]...)
		buffered := e.Engine.Stats().Stream.Buffered
		e.Restart()
		if got := e.Engine.Stats().Stream.Buffered; got != buffered {
			t.Fatalf("%d events buffered before restart, %d after", buffered, got)
		}
	}
	e.Flush()
	equal(t, "restarts mid-stream", want, picture(e.Alerts()))
}

func TestCaseLogWriteFailure(t *testing.T) {
	e := h.New(t)
	e.MustAccept(h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)...)
	e.Flush()
	id := e.Alerts()[0].ID

	e.Engine.SetCaseLogFault(func() error { return errDisk })
	status, raw := e.Do("POST", "/v1/alerts/"+id+"/cases", h.AliceToken, map[string]string{"note": "n"})
	if status != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte("STORAGE_UNAVAILABLE")) {
		t.Fatalf("opening a case with a failing log: %d %s", status, raw)
	}
	// Nothing happened: no case, and the alert is still in the queue.
	if len(e.Engine.Cases("")) != 0 || e.Alerts()[0].Status != domain.AlertOpen {
		t.Fatalf("state changed without a durable record: %v", e.Engine.Cases(""))
	}

	e.Engine.SetCaseLogFault(nil)
	var c domain.Case
	e.JSON("POST", "/v1/alerts/"+id+"/cases", h.AliceToken, map[string]string{"note": "n"}, 201, &c)
	if c.ID != "case-000001" {
		t.Errorf("case ID %s: the failed attempt consumed a number", c.ID)
	}
	e.JSON("POST", "/v1/cases/"+c.ID+"/triage", h.AliceToken, map[string]string{"priority": "P2"}, 200, nil)

	// An analyst action that cannot be recorded does not happen.
	e.Engine.SetCaseLogFault(func() error { return errDisk })
	for _, call := range []struct {
		path string
		body map[string]string
	}{
		{"/notes", map[string]string{"text": "lost?"}},
		{"/resolve", map[string]string{"disposition": "FALSE_POSITIVE", "summary": "s"}},
		{"/escalate", map[string]string{"reason": "r"}},
	} {
		if status, _ := e.Do("POST", "/v1/cases/"+c.ID+call.path, h.BobToken, call.body); status != http.StatusServiceUnavailable {
			t.Errorf("%s with a failing log: %d", call.path, status)
		}
	}
	// Nor can evidence leave without the export being on the record.
	if status, _ := e.Do("GET", "/v1/cases/"+c.ID+"/evidence", h.BobToken, nil); status != http.StatusServiceUnavailable {
		t.Errorf("export with a failing log: %d", status)
	}
	e.Engine.SetCaseLogFault(nil)
	got, _ := e.Engine.Case(c.ID)
	if got.Status != domain.CaseInReview || len(got.Notes) != 1 || len(got.AuditEvents) != 2 {
		t.Errorf("case changed without a durable record: %+v", got)
	}
	if st := e.Engine.AuditVerify(); !st.Verified || st.Events != 2 {
		t.Errorf("chain: %+v", st)
	}
	e.Restart()
	after, _ := e.Engine.Case(c.ID)
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Errorf("case differs after restart:\n%s\n%s", a, b)
	}
}

func TestTamperedCaseLogStopsTheEngine(t *testing.T) {
	e := h.New(t)
	e.MustAccept(h.Layering("L1", h.At(0), 4, 1000, 200, 4, time.Second)...)
	e.Flush()
	c, err := e.Engine.OpenCase("alice", e.Alerts()[0].ID, "", "first impression: stacked offers")
	if err != nil {
		t.Fatal(err)
	}
	e.Engine.Triage("alice", c.ID, "P1", "alice")
	e.Engine.Resolve("alice", c.ID, domain.DispReferred, "passing on")

	path := filepath.Join(e.Dir, "cases.jsonl")
	orig, _ := os.ReadFile(path)
	// Someone edits the disposition on disk.
	edited := bytes.Replace(orig, []byte(domain.DispReferred), []byte(domain.DispFalsePositive), 1)
	if bytes.Equal(orig, edited) {
		t.Fatal("nothing to tamper with")
	}
	os.WriteFile(path, edited, 0o600)

	// The running engine notices on verification...
	if st := e.Engine.AuditVerify(); st.Verified || st.Error == "" {
		t.Errorf("verification passed on an edited log: %+v", st)
	}
	var chain struct {
		Verified bool
		Error    string
	}
	e.JSON("GET", "/v1/audit/verify", h.BobToken, nil, 200, &chain)
	if chain.Verified {
		t.Error("the API reports an edited log as verified")
	}
	// ...and a restart refuses to load it.
	e.Engine.Close()
	_, err = engine.Open(engine.Config{DataDir: e.Dir, RulesPath: e.RulesPath, RefDataPath: e.RefPath, Clock: e.Clock.Now})
	if err == nil {
		t.Fatal("engine started on a tampered case log")
	}
	os.WriteFile(path, orig, 0o600)
	e.Engine = nil
	e.Restart()
	if st := e.Engine.AuditVerify(); !st.Verified {
		t.Errorf("restored log: %+v", st)
	}
}
