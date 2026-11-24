// Package engine wires the stages together and owns their state.
//
//	raw event -> normalize -> journal -> reorder (watermark) -> detectors
//	          -> suppression -> alert -> journal
//
// It is a single writer: one mutex guards everything, so each input is
// journalled and fully processed before the next one starts. That is what
// makes the journal an exact, replayable history.
//
// Durability rules:
//
//   - An event is journalled before any detector sees it. If the journal
//     cannot be written the event is refused, never processed "in memory
//     only": an alert whose evidence was not stored would be worthless.
//   - Alerts are journalled right after the input that produced them. If
//     that write fails, nothing more is accepted until it succeeds.
//   - On restart, inputs are replayed to rebuild detector state, and
//     alerts are restored exactly as journalled. Replay never re-decides
//     an old alert, so a later change to rules or code cannot rewrite
//     history. The only input whose alerts are computed on restart is the
//     very last one, if the process died before its alerts were written.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"surveillance/src/cases"
	"surveillance/src/domain"
	"surveillance/src/journal"
	"surveillance/src/metrics"
	"surveillance/src/normalize"
	"surveillance/src/pipeline"
	"surveillance/src/rules"
)

var (
	// ErrJournal means the input could not be made durable. The caller
	// should retry; redelivery is safe because duplicates are recognised.
	ErrJournal  = errors.New("journal unavailable")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Config struct {
	DataDir     string
	RulesPath   string
	RefDataPath string // optional
	Clock       func() time.Time
	// SyncJournal fsyncs every journal record before acknowledging it.
	SyncJournal bool
}

type counters struct {
	events, orders, trades, late uint64 // rebuilt from the journal
	duplicates                   uint64 // since process start
	rejected                     map[string]uint64
	journalErrors                uint64
	idleAdvances                 uint64
}

type Engine struct {
	mu    sync.Mutex
	cfg   Config
	clock func() time.Time
	norm  *normalize.Normalizer
	jr    *journal.Journal
	rej   *os.File
	cases *cases.Service

	rs     rules.Config
	rsHash string
	active bool

	reorder *pipeline.Reorder
	windows *pipeline.Windower
	det     *rules.Detectors
	book    *rules.Book

	seq        uint64
	offsets    map[string]int64 // event ID -> journal offset
	pendingOut *journal.Record
	lastRecv   time.Time
	features   []domain.FeatureWindow
	st         counters

	// AlertLatency is receive-to-alert time for newly raised alerts, in seconds.
	AlertLatency *metrics.Histogram
	// ResolveTime is case open-to-resolve time, in seconds.
	ResolveTime *metrics.Histogram
}

const maxFeatures = 5000

// Open loads configuration, replays the journal and the case log, and
// returns an engine ready for input. It refuses to start on a ruleset that
// does not validate or a log that does not verify.
func Open(cfg Config) (*Engine, error) {
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	e := &Engine{cfg: cfg, clock: cfg.Clock, offsets: map[string]int64{},
		st:           counters{rejected: map[string]uint64{}},
		AlertLatency: metrics.NewHistogram(0.001, 0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300),
		ResolveTime:  metrics.NewHistogram(60, 300, 900, 3600, 14400, 86400, 604800),
	}

	var rd normalize.RefData
	if cfg.RefDataPath != "" {
		var err error
		if rd, err = normalize.LoadRefData(cfg.RefDataPath); err != nil {
			return nil, err
		}
	}
	norm, err := normalize.New(rd)
	if err != nil {
		return nil, err
	}
	e.norm = norm

	want, err := rules.Load(cfg.RulesPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if e.jr, err = journal.Open(filepath.Join(cfg.DataDir, "journal.jsonl"), cfg.SyncJournal); err != nil {
		return nil, err
	}
	if e.cases, err = cases.Open(filepath.Join(cfg.DataDir, "cases.jsonl")); err != nil {
		e.jr.Close()
		return nil, err
	}
	fail := func(err error) (*Engine, error) {
		e.jr.Close()
		e.cases.Close()
		return nil, err
	}
	tail, tailAt, err := e.rebuild()
	if err != nil {
		return fail(err)
	}
	if e.active {
		for _, a := range e.book.All() {
			e.syncAlert(a.ID)
		}
	}
	if len(tail) > 0 {
		// The process stopped between journalling the last input and
		// journalling its alerts. Raise them now.
		if _, _, err := e.emit(tail, tailAt); err != nil {
			return fail(fmt.Errorf("recovering the alerts of the last journalled input: %w", err))
		}
	}
	if !e.active || e.rsHash != want.Hash() {
		if err := e.activate(want, e.clock()); err != nil {
			return fail(err)
		}
	}
	if e.rej, err = os.OpenFile(filepath.Join(cfg.DataDir, "rejects.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
		return fail(err)
	}
	// A restart counts as activity: the idle timer starts now, so the
	// watermark does not leap ahead of a source that is about to resume.
	e.lastRecv = e.clock()
	return e, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return errors.Join(e.jr.Close(), e.cases.Close(), e.rej.Close())
}

// rebuild replays the journal. Detections computed along the way are
// thrown away, because the journalled alerts are the record of what was
// raised. The exception is a final input with no output record after it:
// its detections are returned for the caller to raise.
func (e *Engine) rebuild() (tail []rules.Detection, tailAt time.Time, err error) {
	err = e.jr.Scan(func(off int64, rec journal.Record) error {
		if rec.Seq > e.seq {
			e.seq = rec.Seq
		}
		if rec.Kind == journal.KindOut {
			if e.book == nil {
				return fmt.Errorf("journal: alerts recorded at offset %d before any ruleset", off)
			}
			for _, a := range rec.Alerts {
				e.book.Restore(a)
			}
			tail = nil
			return nil
		}
		dets, err := e.applyInput(rec, off)
		if err != nil {
			return fmt.Errorf("journal: offset %d: %w", off, err)
		}
		tail, tailAt = dets, rec.At
		return nil
	})
	return tail, tailAt, err
}

// activate journals a ruleset and puts it in force.
func (e *Engine) activate(c rules.Config, now time.Time) error {
	rec := journal.Record{Kind: journal.KindRuleset, Seq: e.seq + 1, At: now, Ruleset: c.Canonical()}
	off, err := e.jr.Append(rec)
	if err != nil {
		e.st.journalErrors++
		return fmt.Errorf("%w: %v", ErrJournal, err)
	}
	e.seq++
	dets, err := e.applyInput(rec, off)
	if err != nil {
		return err
	}
	_, _, err = e.emit(dets, now)
	return err
}

// applyInput feeds one journalled input through the pipeline and returns
// what the detectors found. It is the single path for live input and for
// replay, which is why replay reproduces the same state.
func (e *Engine) applyInput(rec journal.Record, off int64) ([]rules.Detection, error) {
	switch rec.Kind {
	case journal.KindRuleset:
		c, err := rules.Parse(rec.Ruleset)
		if err != nil {
			return nil, err
		}
		e.setRuleset(c)
	case journal.KindEvent:
		if !e.active || rec.Event == nil {
			return nil, errors.New("event record without an active ruleset or without an event")
		}
		ev := *rec.Event
		e.offsets[ev.ID] = off
		e.st.events++
		if ev.Kind == domain.KindTrade {
			e.st.trades++
		} else {
			e.st.orders++
		}
		if ev.Late {
			e.st.late++
			return nil, nil
		}
		e.reorder.Push(ev)
	case journal.KindAdvance:
		if !e.active || rec.AdvanceTo == nil {
			return nil, errors.New("advance record without an active ruleset or without a target")
		}
		e.reorder.AdvanceTo(rec.AdvanceTo.UnixNano())
	default:
		return nil, fmt.Errorf("unknown record kind %q", rec.Kind)
	}
	return e.drain(), nil
}

func (e *Engine) setRuleset(c rules.Config) {
	w := c.Window
	if !e.active {
		e.reorder = pipeline.NewReorder(w.Lateness())
		e.windows = pipeline.NewWindower(w.Size(), w.Hop(), c.Suppression.MaxEvidenceRefs)
		e.det = rules.NewDetectors(c)
		e.book = rules.NewBook(c.Suppression)
	} else {
		e.reorder.SetLateness(w.Lateness())
		if w.Size() != e.windows.Size() || w.Hop() != e.windows.Hop() {
			// New window geometry: windows still open under the old one
			// are dropped rather than emitted with mixed boundaries.
			e.windows = pipeline.NewWindower(w.Size(), w.Hop(), c.Suppression.MaxEvidenceRefs)
		}
		e.det.SetConfig(c)
		e.book.SetConfig(c.Suppression)
	}
	e.rs, e.rsHash, e.active = c, c.Hash(), true
}

// drain hands everything the watermark has passed to the detectors, in
// event-time order, then closes the windows it has passed.
func (e *Engine) drain() []rules.Detection {
	wm, ok := e.reorder.Watermark()
	if !ok {
		return nil
	}
	var dets []rules.Detection
	for _, ev := range e.reorder.Release() {
		dets = append(dets, e.det.OnWatermark(ev.TS())...)
		dets = append(dets, e.det.OnEvent(ev)...)
		e.windows.Add(ev)
	}
	dets = append(dets, e.det.OnWatermark(wm)...)
	for _, fw := range e.windows.Close(wm) {
		dets = append(dets, e.det.OnWindow(fw)...)
		fw.EventRefs = nil // the alert keeps the evidence; the history keeps the numbers
		e.features = append(e.features, fw)
	}
	if len(e.features) > 2*maxFeatures {
		e.features = append(e.features[:0], e.features[len(e.features)-maxFeatures:]...)
	}
	return dets
}

// emit runs detections through suppression and journals the result.
func (e *Engine) emit(dets []rules.Detection, now time.Time) (created, updated []string, err error) {
	if len(dets) == 0 {
		return nil, nil, nil
	}
	meta := rules.Meta{RulesetVersion: e.rs.Version, RulesetHash: e.rsHash}
	final := map[string]domain.Alert{}
	var order []string
	isNew := map[string]bool{}
	for _, d := range dets {
		a, fresh := e.book.Apply(d, meta, now)
		if _, seen := final[a.ID]; !seen {
			order = append(order, a.ID)
		}
		final[a.ID] = a
		if fresh {
			isNew[a.ID] = true
			if !a.TriggerReceivedAt.IsZero() {
				e.AlertLatency.Observe(now.Sub(a.TriggerReceivedAt).Seconds())
			}
		}
	}
	rec := journal.Record{Kind: journal.KindOut, Seq: e.seq, At: now}
	for _, id := range order {
		rec.Alerts = append(rec.Alerts, final[id])
		if isNew[id] {
			created = append(created, id)
		} else {
			updated = append(updated, id)
		}
	}
	if _, werr := e.jr.Append(rec); werr != nil {
		e.st.journalErrors++
		e.pendingOut = &rec
		return created, updated, fmt.Errorf("%w: %v", ErrJournal, werr)
	}
	return created, updated, nil
}

// flushPending retries an alert record that could not be written earlier.
// Until it is on disk the engine accepts nothing new.
func (e *Engine) flushPending() error {
	if e.pendingOut == nil {
		return nil
	}
	if _, err := e.jr.Append(*e.pendingOut); err != nil {
		e.st.journalErrors++
		return fmt.Errorf("%w: %v", ErrJournal, err)
	}
	e.pendingOut = nil
	return nil
}

type Rejected struct {
	Index  int    `json:"index"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type IngestResult struct {
	Accepted      int        `json:"accepted"`
	Duplicates    int        `json:"duplicates"`
	Late          int        `json:"late"`
	Rejected      []Rejected `json:"rejected"`
	AlertsCreated []string   `json:"alerts_created"`
	AlertsUpdated []string   `json:"alerts_updated"`
}

func newResult() IngestResult {
	return IngestResult{Rejected: []Rejected{}, AlertsCreated: []string{}, AlertsUpdated: []string{}}
}

func (r *IngestResult) alerts(created, updated []string) {
	for _, id := range created {
		r.AlertsCreated = append(r.AlertsCreated, id)
	}
	for _, id := range updated {
		if !has(r.AlertsCreated, id) && !has(r.AlertsUpdated, id) {
			r.AlertsUpdated = append(r.AlertsUpdated, id)
		}
	}
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// IngestJSON accepts events as sent on the wire. Each is judged on its
// own: one bad event does not sink the batch. If the journal fails, the
// result says how far it got and the error is ErrJournal.
func (e *Engine) IngestJSON(items []json.RawMessage) (IngestResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := newResult()
	if err := e.flushPending(); err != nil {
		return res, err
	}
	for i, item := range items {
		raw, rej := normalize.Decode(item)
		if rej != nil {
			e.refuse(&res, i, rej, item)
			continue
		}
		if err := e.ingestOne(raw, false, &res, i, item); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Ingest accepts already-decoded events. trusted is for the built-in
// simulator only: it skips the future-timestamp guard, because simulated
// time is allowed to run ahead of the wall clock.
func (e *Engine) Ingest(raws []normalize.Raw, trusted bool) (IngestResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := newResult()
	if err := e.flushPending(); err != nil {
		return res, err
	}
	for i, raw := range raws {
		if err := e.ingestOne(raw, trusted, &res, i, nil); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (e *Engine) ingestOne(raw normalize.Raw, trusted bool, res *IngestResult, idx int, text []byte) error {
	now := e.clock()
	ev, rej := e.norm.Normalize(raw, now)
	if rej == nil && !trusted {
		if skew := e.rs.Window.FutureSkew(); ev.EventTime.After(now.Add(skew)) {
			rej = &normalize.Reject{Code: normalize.CodeFutureTS, Detail: fmt.Sprintf(
				"event time %s is more than %s ahead of the local clock (%s)",
				ev.EventTime.Format(time.RFC3339Nano), skew, now.Format(time.RFC3339Nano))}
		}
	}
	if rej != nil {
		if text == nil {
			text, _ = json.Marshal(raw)
		}
		e.refuse(res, idx, rej, text)
		return nil
	}
	if _, dup := e.offsets[ev.ID]; dup {
		res.Duplicates++
		e.st.duplicates++
		return nil
	}
	ev.Seq = e.seq + 1
	ev.Late = e.reorder.IsLate(ev.TS())
	rec := journal.Record{Kind: journal.KindEvent, Seq: ev.Seq, At: now, Event: &ev}
	off, err := e.jr.Append(rec)
	if err != nil {
		e.st.journalErrors++
		return fmt.Errorf("%w: %v", ErrJournal, err)
	}
	e.seq++
	e.lastRecv = now
	dets, err := e.applyInput(rec, off)
	if err != nil {
		return err
	}
	if ev.Late {
		res.Late++
	} else {
		res.Accepted++
	}
	created, updated, err := e.emit(dets, now)
	res.alerts(created, updated)
	return err
}

const maxRejectText = 2048

// refuse records a rejected event in the dead-letter log. That log is
// best effort: failing to write it does not fail the request.
func (e *Engine) refuse(res *IngestResult, idx int, rej *normalize.Reject, text []byte) {
	res.Rejected = append(res.Rejected, Rejected{Index: idx, Code: rej.Code, Detail: rej.Detail})
	e.st.rejected[rej.Code]++
	if len(text) > maxRejectText {
		text = text[:maxRejectText]
	}
	line, err := json.Marshal(struct {
		At     time.Time `json:"at"`
		Code   string    `json:"code"`
		Detail string    `json:"detail"`
		Raw    string    `json:"raw"`
	}{e.clock(), rej.Code, rej.Detail, string(text)})
	if err == nil {
		_, _ = e.rej.Write(append(line, '\n'))
	}
}

// advance journals and applies an explicit watermark move.
func (e *Engine) advance(target int64, now time.Time) (created, updated []string, err error) {
	t := time.Unix(0, target).UTC()
	rec := journal.Record{Kind: journal.KindAdvance, Seq: e.seq + 1, At: now, AdvanceTo: &t}
	off, err := e.jr.Append(rec)
	if err != nil {
		e.st.journalErrors++
		return nil, nil, fmt.Errorf("%w: %v", ErrJournal, err)
	}
	e.seq++
	dets, err := e.applyInput(rec, off)
	if err != nil {
		return nil, nil, err
	}
	return e.emit(dets, now)
}

type FlushResult struct {
	Watermark     *time.Time `json:"watermark"`
	AlertsCreated []string   `json:"alerts_created"`
	AlertsUpdated []string   `json:"alerts_updated"`
}

// Flush declares the stream complete up to now: the watermark jumps one
// window past the newest event, which releases everything buffered and
// closes every window that holds data. Anything older that turns up
// afterwards is late. Use it at the end of a batch or a replay, not in
// the middle of a live stream.
func (e *Engine) Flush() (FlushResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := FlushResult{AlertsCreated: []string{}, AlertsUpdated: []string{}}
	if err := e.flushPending(); err != nil {
		return res, err
	}
	maxSeen, ok := e.reorder.MaxSeen()
	if !ok {
		return res, nil
	}
	target := maxSeen + int64(e.rs.Window.Size())
	if wm, _ := e.reorder.Watermark(); target > wm {
		created, updated, err := e.advance(target, e.clock())
		res.AlertsCreated = append(res.AlertsCreated, created...)
		res.AlertsUpdated = append(res.AlertsUpdated, updated...)
		if err != nil {
			return res, err
		}
	}
	if wm, ok := e.reorder.Watermark(); ok {
		t := time.Unix(0, wm).UTC()
		res.Watermark = &t
	}
	return res, nil
}

// Tick is called periodically. When the stream has been silent for the
// idle timeout while events are still buffered or windows still open, the
// watermark follows the local clock instead:
//
//	watermark = newest event time - allowed lateness + time spent idle
//
// so a quiet stream still gets its last events evaluated. It stops one
// window past the newest event, where there is nothing left to release.
func (e *Engine) Tick() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.flushPending(); err != nil {
		return err
	}
	if !e.active || (e.reorder.Buffered() == 0 && !e.windows.Pending()) {
		return nil
	}
	now := e.clock()
	idle := now.Sub(e.lastRecv)
	if idle < e.rs.Window.IdleTimeout() {
		return nil
	}
	maxSeen, ok := e.reorder.MaxSeen()
	if !ok {
		return nil
	}
	// One window past the newest event there is nothing left to release.
	// Checked on the durations, not the sum, so a very long silence
	// cannot overflow.
	target := maxSeen + int64(e.rs.Window.Size())
	if idle < e.rs.Window.Size()+e.rs.Window.Lateness() {
		target = maxSeen - int64(e.rs.Window.Lateness()) + int64(idle)
	}
	if wm, _ := e.reorder.Watermark(); target <= wm {
		return nil
	}
	e.st.idleAdvances++
	_, _, err := e.advance(target, now)
	return err
}

type RulesetInfo struct {
	Version string       `json:"version"`
	Hash    string       `json:"hash"`
	Changed bool         `json:"changed"`
	Config  rules.Config `json:"config"`
}

// ReloadRules re-reads the ruleset file. A file that does not validate
// leaves the current ruleset in force.
func (e *Engine) ReloadRules() (RulesetInfo, error) {
	c, err := rules.Load(e.cfg.RulesPath)
	if err != nil {
		return RulesetInfo{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.flushPending(); err != nil {
		return RulesetInfo{}, err
	}
	changed := c.Hash() != e.rsHash
	if changed {
		if err := e.activate(c, e.clock()); err != nil {
			return RulesetInfo{}, err
		}
	}
	return RulesetInfo{Version: e.rs.Version, Hash: e.rsHash, Changed: changed, Config: e.rs}, nil
}

func (e *Engine) Ruleset() RulesetInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return RulesetInfo{Version: e.rs.Version, Hash: e.rsHash, Config: e.rs}
}

// NextSimStart is the earliest event time a simulated scenario can start
// at without being late: after the wall clock and after the watermark.
func (e *Engine) NextSimStart() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.clock()
	if wm, ok := e.reorder.Watermark(); ok && wm > t.UnixNano() {
		t = time.Unix(0, wm).UTC()
	}
	return t.Truncate(time.Second).Add(time.Second)
}

type AlertFilter struct {
	Status, Symbol, RuleID, Account, Severity string
	Limit                                     int
}

// Alerts lists alerts, newest first.
func (e *Engine) Alerts(f AlertFilter) []domain.Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	all := e.book.All()
	out := []domain.Alert{}
	for i := len(all) - 1; i >= 0; i-- {
		a := all[i]
		if (f.Status != "" && a.Status != f.Status) || (f.Symbol != "" && a.Symbol != f.Symbol) ||
			(f.RuleID != "" && a.RuleID != f.RuleID) || (f.Severity != "" && a.Severity != f.Severity) ||
			(f.Account != "" && !has(a.Accounts, f.Account)) {
			continue
		}
		out = append(out, a)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

type AlertDetail struct {
	Alert            domain.Alert   `json:"alert"`
	Events           []domain.Event `json:"events"`
	MissingEventRefs []string       `json:"missing_event_refs,omitempty"`
}

// Alert returns one alert with the full text of its evidence events.
func (e *Engine) Alert(id string) (AlertDetail, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.book.Get(id)
	if !ok {
		return AlertDetail{}, ErrNotFound
	}
	evs, missing := e.events(a.EventRefs)
	return AlertDetail{Alert: a, Events: evs, MissingEventRefs: missing}, nil
}

// events reads evidence back from the journal, in event-time order.
func (e *Engine) events(refs []string) (evs []domain.Event, missing []string) {
	evs = []domain.Event{}
	for _, ref := range refs {
		off, ok := e.offsets[ref]
		if !ok {
			missing = append(missing, ref)
			continue
		}
		rec, err := e.jr.ReadAt(off)
		if err != nil || rec.Event == nil || rec.Event.ID != ref {
			missing = append(missing, ref)
			continue
		}
		evs = append(evs, *rec.Event)
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if a, b := evs[i].TS(), evs[j].TS(); a != b {
			return a < b
		}
		return evs[i].Seq < evs[j].Seq
	})
	return evs, missing
}

func (e *Engine) snapshot(a domain.Alert, now time.Time) domain.Snapshot {
	evs, missing := e.events(a.EventRefs)
	return domain.Snapshot{TakenAt: now.UTC(), Alert: a, Events: evs, MissingEventRefs: missing}
}

// syncAlert derives an alert's status from the cases it belongs to.
func (e *Engine) syncAlert(id string) {
	ids := e.cases.CasesOf(id)
	status := domain.AlertOpen
	if len(ids) > 0 {
		status = domain.AlertClosed
		for _, c := range ids {
			if e.cases.Status(c) != domain.CaseResolved {
				status = domain.AlertInCase
			}
		}
	}
	e.book.SetCase(id, status, ids)
}

func (e *Engine) syncCase(c domain.Case) {
	for _, id := range c.AlertIDs {
		e.syncAlert(id)
	}
}

// OpenCase starts a case on an alert and freezes its evidence.
func (e *Engine) OpenCase(actor, alertID, title, note string) (domain.Case, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.book.Get(alertID)
	if !ok {
		return domain.Case{}, ErrNotFound
	}
	if a.Status == domain.AlertInCase {
		return domain.Case{}, fmt.Errorf("%w: alert %s is already under review in %v", ErrConflict, alertID, a.CaseIDs)
	}
	now := e.clock()
	c, err := e.cases.Create(actor, now, title, e.snapshot(a, now), note)
	if err != nil {
		return domain.Case{}, err
	}
	e.syncCase(c)
	return c, nil
}

// AttachAlert adds another alert to an existing case.
func (e *Engine) AttachAlert(actor, caseID, alertID string) (domain.Case, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.book.Get(alertID)
	if !ok {
		return domain.Case{}, ErrNotFound
	}
	if a.Status == domain.AlertInCase {
		return domain.Case{}, fmt.Errorf("%w: alert %s is already under review in %v", ErrConflict, alertID, a.CaseIDs)
	}
	now := e.clock()
	c, err := e.cases.Attach(actor, now, caseID, e.snapshot(a, now))
	if err != nil {
		return domain.Case{}, err
	}
	e.syncCase(c)
	return c, nil
}

func (e *Engine) caseOp(fn func(now time.Time) (domain.Case, error)) (domain.Case, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := fn(e.clock())
	if err != nil {
		return domain.Case{}, err
	}
	e.syncCase(c)
	return c, nil
}

func (e *Engine) Triage(actor, id, priority, assignee string) (domain.Case, error) {
	return e.caseOp(func(now time.Time) (domain.Case, error) {
		return e.cases.Triage(actor, now, id, priority, assignee)
	})
}

func (e *Engine) AddNote(actor, id, text string) (domain.Case, error) {
	return e.caseOp(func(now time.Time) (domain.Case, error) { return e.cases.AddNote(actor, now, id, text) })
}

func (e *Engine) Escalate(actor, id, reason string) (domain.Case, error) {
	return e.caseOp(func(now time.Time) (domain.Case, error) { return e.cases.Escalate(actor, now, id, reason) })
}

func (e *Engine) Resolve(actor, id, disposition, summary string) (domain.Case, error) {
	return e.caseOp(func(now time.Time) (domain.Case, error) {
		c, err := e.cases.Resolve(actor, now, id, disposition, summary)
		if err == nil {
			e.ResolveTime.Observe(now.Sub(c.CreatedAt).Seconds())
		}
		return c, err
	})
}

func (e *Engine) Reopen(actor, id, reason string) (domain.Case, error) {
	return e.caseOp(func(now time.Time) (domain.Case, error) { return e.cases.Reopen(actor, now, id, reason) })
}

func (e *Engine) Case(id string) (domain.Case, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.cases.Get(id)
	if !ok {
		return domain.Case{}, ErrNotFound
	}
	return c, nil
}

func (e *Engine) Cases(status string) []domain.Case {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cases.List(status)
}

// Evidence exports a case's evidence bundle and records the export.
func (e *Engine) Evidence(actor, id string) (cases.Bundle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cases.Export(actor, e.clock(), id)
}

func (e *Engine) AuditVerify() cases.ChainStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cases.Verify()
}

func (e *Engine) AuditHead() cases.ChainStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cases.Head()
}

// Features returns recently closed feature windows, newest first.
func (e *Engine) Features(account, symbol string, limit int) []domain.FeatureWindow {
	e.mu.Lock()
	defer e.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := []domain.FeatureWindow{}
	for i := len(e.features) - 1; i >= 0 && len(out) < limit; i-- {
		fw := e.features[i]
		if (account == "" || fw.AccountID == account) && (symbol == "" || fw.Symbol == symbol) {
			out = append(out, fw)
		}
	}
	return out
}

// SetJournalFault and SetCaseLogFault inject write failures (tests, demo).
func (e *Engine) SetJournalFault(fn func() error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jr.Fail = fn
}

func (e *Engine) SetCaseLogFault(fn func() error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cases.Fail = fn
}
