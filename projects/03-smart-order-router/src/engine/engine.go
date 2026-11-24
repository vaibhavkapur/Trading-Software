// Package engine wires market-view, route-planner, risk, child-OMS, the venue
// simulator and TCA together on a single-threaded, simulated-time event loop.
// There is no wall clock and no goroutine inside the engine, so a run is
// fully reproducible. Callers that need concurrency (the HTTP API) serialize
// access themselves.
package engine

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"sor/src/domain"
	"sor/src/marketview"
	"sor/src/oms"
	"sor/src/planner"
	"sor/src/risk"
	"sor/src/tca"
	"sor/src/venuesim"
)

type event struct {
	at  int64
	seq uint64
	fn  func()
}

type eventHeap []event

func (h eventHeap) Len() int      { return len(h) }
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h eventHeap) Less(i, j int) bool {
	return h[i].at < h[j].at || (h[i].at == h[j].at && h[i].seq < h[j].seq)
}
func (h *eventHeap) Push(x any) { *h = append(*h, x.(event)) }
func (h *eventHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// wave is one routing attempt of a slice: the children sent together.
type wave struct {
	parentID     string
	want, filled int64
	pending      int
	attempt      int
	maxLevels    int
	exclude      []string
}

type parentMeta struct {
	deadlinePassed bool
	doneLogged     bool
}

type ParentRequest struct {
	Symbol          string      `json:"symbol"`
	Side            domain.Side `json:"side"`
	Qty             int64       `json:"qty"`
	Strategy        string      `json:"strategy"`
	DurationSeconds int64       `json:"duration_seconds"`
	IntervalSeconds int64       `json:"interval_seconds,omitempty"`
}

type Engine struct {
	cfg      Config
	now      int64
	queue    eventHeap
	seq      uint64
	step     int
	venues   map[string]*venuesim.Venue
	names    []string
	view     *marketview.View
	book     *oms.Book
	risk     *risk.Gate
	waves    map[string]*wave // by child id
	meta     map[string]*parentMeta
	cumVol   []int64 // cumulative market volume through step k
	cumNotl  []int64 // cumulative market notional (mills) through step k
	log      io.Writer
	counters map[string]int64
}

// New builds an engine at simulated time cfg.StartMs. log may be nil;
// otherwise every decision and order event is appended to it as JSON lines.
func New(cfg Config, log io.Writer) *Engine {
	cfg = cfg.WithDefaults()
	e := &Engine{cfg: cfg, now: cfg.StartMs, venues: map[string]*venuesim.Venue{},
		view: marketview.New(cfg.View, cfg.Symbol), book: oms.NewBook(), risk: risk.New(cfg.Risk),
		waves: map[string]*wave{}, meta: map[string]*parentMeta{}, log: log, counters: map[string]int64{}}
	path := venuesim.NewMidPath(cfg.Seed, cfg.StartMidTicks, cfg.MovePct)
	for _, vc := range cfg.Venues {
		e.venues[vc.Name] = venuesim.New(vc, cfg.Symbol, cfg.LotSize, cfg.Seed, path)
		e.names = append(e.names, vc.Name)
		e.view.AddVenue(vc.Name, vc.TakerFeeMills, vc.LatencyMs)
	}
	sort.Strings(e.names)
	e.at(cfg.StartMs, e.stepTick)
	return e
}

func (e *Engine) Config() Config { return e.cfg }
func (e *Engine) Now() int64     { return e.now }

func (e *Engine) at(t int64, fn func()) {
	e.seq++
	heap.Push(&e.queue, event{at: max(t, e.now), seq: e.seq, fn: fn})
}

func (e *Engine) emit(kind string, data any) {
	if e.log == nil {
		return
	}
	b, err := json.Marshal(struct {
		TsMs int64  `json:"ts_ms"`
		Kind string `json:"kind"`
		Data any    `json:"data"`
	}{e.now, kind, data})
	if err == nil {
		e.log.Write(append(b, '\n'))
	}
}

func (e *Engine) count(name, venue string) {
	e.counters[fmt.Sprintf("%s{venue=%q}", name, venue)]++
}

// Counters returns a copy of the monotonic event counters, keyed in
// Prometheus `name{labels}` form.
func (e *Engine) Counters() map[string]int64 {
	out := make(map[string]int64, len(e.counters))
	for k, v := range e.counters {
		out[k] = v
	}
	return out
}

// AdvanceTo processes every event scheduled up to and including t.
func (e *Engine) AdvanceTo(t int64) {
	for len(e.queue) > 0 && e.queue[0].at <= t {
		ev := heap.Pop(&e.queue).(event)
		e.now = ev.at
		ev.fn()
	}
	e.now = max(e.now, t)
}

func (e *Engine) active() bool {
	for _, id := range e.book.ParentIDs() {
		if e.book.Parent(id).Status == domain.ParentWorking || e.book.InFlight(id) > 0 {
			return true
		}
	}
	return false
}

// RunUntilDone advances event by event until no parent is working and no
// child is in flight, or simulated time reaches maxMs.
func (e *Engine) RunUntilDone(maxMs int64) {
	for e.active() && len(e.queue) > 0 && e.queue[0].at <= maxMs {
		ev := heap.Pop(&e.queue).(event)
		e.now = ev.at
		ev.fn()
	}
}

// stepTick advances every venue one step and publishes quotes.
func (e *Engine) stepTick() {
	var vol, notional int64
	for _, name := range e.names {
		v := e.venues[name]
		tv, tpx := v.Step(e.step)
		vol += tv
		notional += tv * tpx * domain.MillsPerTick
		if f := v.Fault(); f.Down || f.StopQuotes {
			continue
		}
		q := v.Quote(e.now)
		e.at(e.now+v.Latency(), func() {
			if v.Fault().Down {
				return // lost with the link
			}
			q.RecvMs = e.now
			e.view.OnQuote(q)
		})
	}
	if n := len(e.cumVol); n > 0 {
		vol += e.cumVol[n-1]
		notional += e.cumNotl[n-1]
	}
	e.cumVol, e.cumNotl = append(e.cumVol, vol), append(e.cumNotl, notional)
	e.step++
	e.at(e.cfg.StartMs+int64(e.step)*e.cfg.StepMs, e.stepTick)
}

// MarketVWAPMills is the simulated market VWAP over [fromMs, toMs].
func (e *Engine) MarketVWAPMills(fromMs, toMs int64) int64 {
	if len(e.cumVol) == 0 {
		return 0
	}
	idx := func(ms int64) int {
		return int(min(max((ms-e.cfg.StartMs)/e.cfg.StepMs, 0), int64(len(e.cumVol)-1)))
	}
	i, j := idx(fromMs), idx(toMs)
	vol, notional := e.cumVol[j], e.cumNotl[j]
	if i > 0 {
		vol -= e.cumVol[i-1]
		notional -= e.cumNotl[i-1]
	}
	if vol == 0 {
		return 0
	}
	return notional / vol
}

// SubmitParent validates and accepts a parent order at the current simulated
// time. A refused order is still recorded (status REJECTED) for audit.
func (e *Engine) SubmitParent(r ParentRequest) (domain.ParentOrder, error) {
	strategy := strings.ToUpper(r.Strategy)
	if strategy == "" {
		strategy = domain.StrategyTWAP
	}
	interval := r.IntervalSeconds * 1000
	if interval == 0 {
		interval = e.cfg.Router.DefaultIntervalMs
		if strategy == domain.StrategyLiquidity {
			interval = 1000
		}
	}
	p := domain.ParentOrder{Symbol: r.Symbol, Side: r.Side, TargetQty: r.Qty, Strategy: strategy, StartMs: e.now,
		DeadlineMs: e.now + r.DurationSeconds*1000, IntervalMs: interval, Status: domain.ParentWorking}
	mid, haveMid := e.view.Peek(e.now).MidMills()
	var err error
	switch {
	case strategy != domain.StrategyTWAP && strategy != domain.StrategyLiquidity:
		err = &risk.Rejection{Code: "BAD_STRATEGY", Msg: "strategy must be TWAP or LIQUIDITY"}
	case !haveMid:
		if err = e.risk.CheckParent(p, 1); err == nil {
			err = &risk.Rejection{Code: "NO_MARKET_DATA", Msg: "no healthy venue is quoting"}
		}
	default:
		err = e.risk.CheckParent(p, mid)
	}
	if err != nil {
		p.Status, p.Reason, p.DoneMs = domain.ParentRejected, err.Error(), e.now
		pp := e.book.AddParent(p)
		e.emit("parent_rejected", pp)
		return *pp, err
	}
	p.ArrivalMidMills = mid
	pp := e.book.AddParent(p)
	id := pp.ID
	e.meta[id] = &parentMeta{}
	e.emit("parent_accepted", pp)
	n := max((p.DeadlineMs-p.StartMs)/interval, 1)
	for i := int64(0); i < n; i++ {
		e.at(p.StartMs+i*interval, func() { e.slice(id, i, n) })
	}
	e.at(p.DeadlineMs, func() {
		e.meta[id].deadlinePassed = true
		e.settle(id)
	})
	return *pp, nil
}

// slice runs scheduled slice i of n for a parent.
func (e *Engine) slice(id string, i, n int64) {
	p := e.book.Parent(id)
	if p.Status != domain.ParentWorking {
		return
	}
	want, maxLevels := oms.Available(p), 1
	if p.Strategy == domain.StrategyTWAP {
		// Catch-up TWAP: aim for the scheduled cumulative quantity, so
		// earlier shortfalls roll into later slices automatically.
		lot := e.cfg.LotSize
		target := p.TargetQty
		if i < n-1 {
			target = p.TargetQty * (i + 1) / n / lot * lot
		}
		want, maxLevels = min(want, target-p.FilledQty-p.ReservedQty), 0
	}
	e.route(p, want, maxLevels, 0, nil)
}

// route plans and sends one wave of children for up to want shares.
func (e *Engine) route(p *domain.ParentOrder, want int64, maxLevels, attempt int, exclude []string) {
	if want < e.cfg.LotSize || p.Status != domain.ParentWorking || e.now >= p.DeadlineMs || e.risk.Killed() {
		return
	}
	snap := e.view.Snapshot(e.now)
	mid, ok := snap.MidMills()
	if !ok {
		e.emit("route_skipped", map[string]any{"parent_id": p.ID, "snapshot_id": snap.ID, "reason": "no healthy venue is quoting"})
		return
	}
	in := planner.Input{ParentID: p.ID, Side: p.Side, WantQty: want, LimitPx: e.risk.CollarPx(p.Side, mid),
		MaxLevels: maxLevels, DecidedMs: e.now, Exclude: append([]string{}, exclude...), Policy: e.cfg.Policy, Snapshot: snap}
	dec := planner.Plan(in)
	e.emit("route_decision", map[string]any{"input": in, "decision": dec})
	w := &wave{parentID: p.ID, want: want, attempt: attempt, maxLevels: maxLevels, exclude: in.Exclude}
	for _, a := range dec.Allocations {
		if err := e.risk.CheckChild(p.Side, a.Qty, a.LimitPx, mid, oms.Available(p)); err != nil {
			e.emit("child_blocked", map[string]any{"parent_id": p.ID, "allocation": a, "reason": err.Error()})
			continue
		}
		c, err := e.book.NewChild(p.ID, a, snap.ID, mid, attempt, e.now)
		if err != nil {
			e.emit("child_blocked", map[string]any{"parent_id": p.ID, "allocation": a, "reason": err.Error()})
			continue
		}
		w.pending++
		e.waves[c.ID] = w
		e.send(c)
	}
}

func (e *Engine) send(c *domain.ChildOrder) {
	v := e.venues[c.Venue]
	id := c.ID
	e.count("sor_child_orders_total", c.Venue)
	e.emit("child_sent", c)
	if v.Fault().Down { // the gateway knows the session is down: fail fast, nothing left the building
		e.at(e.now, func() {
			e.onReport(domain.ExecReport{ChildID: id, Venue: c.Venue, Status: domain.ReportRejected,
				Reason: "VENUE_DOWN", Fills: []domain.VenueFill{}, VenueMs: e.now})
		})
		return
	}
	ord := venuesim.Order{ChildID: id, Side: c.Side, Qty: c.Qty, LimitPx: c.LimitPx}
	e.at(e.now+v.Latency(), func() {
		rep, dup := v.Submit(e.now, ord)
		back := e.now + v.Latency()
		e.at(back, func() { e.deliver(rep) })
		if dup {
			e.at(back+1, func() { e.deliver(rep) })
		}
	})
	e.at(e.now+e.cfg.Router.ChildTimeoutMs, func() { e.onTimeout(id) })
}

// deliver carries a venue report over the link; it is lost if the link is down.
func (e *Engine) deliver(rep domain.ExecReport) {
	if e.venues[rep.Venue].Fault().Down {
		e.count("sor_reports_lost_total", rep.Venue)
		return
	}
	e.onReport(rep)
}

func (e *Engine) onReport(rep domain.ExecReport) {
	c := e.book.Child(rep.ChildID)
	if c == nil {
		return
	}
	wasInFlight := c.Status == domain.ChildSent
	fills, changed, err := e.book.ApplyReport(rep, e.now)
	if err != nil {
		// A report we cannot reconcile means our view of exposure is wrong: stop trading.
		e.emit("anomaly", map[string]any{"report": rep, "error": err.Error()})
		e.SetKill(true)
		return
	}
	if !changed {
		e.count("sor_duplicate_reports_total", rep.Venue)
		return
	}
	e.emit("exec_report", map[string]any{"report": rep, "child_status": c.Status, "recv_ms": e.now})
	var filled int64
	for _, f := range fills {
		filled += f.Qty
	}
	e.counters[fmt.Sprintf("sor_filled_qty_total{venue=%q}", rep.Venue)] += filled
	failed := c.Status == domain.ChildRejected
	if failed {
		e.count("sor_child_rejects_total", rep.Venue)
	}
	if wasInFlight {
		e.view.RecordOutcome(c.Venue, !failed, e.now-c.SentMs, c.FilledQty, c.Qty, e.now)
		e.waveResolved(c, filled, failed)
	}
	e.settle(c.ParentID)
}

func (e *Engine) onTimeout(childID string) {
	if !e.book.Timeout(childID) {
		return
	}
	c := e.book.Child(childID)
	e.count("sor_child_timeouts_total", c.Venue)
	e.emit("child_timeout", map[string]any{"child_id": childID, "venue": c.Venue})
	e.view.RecordOutcome(c.Venue, false, 0, 0, c.Qty, e.now)
	e.waveResolved(c, 0, true)
	e.settle(c.ParentID)
}

// waveResolved accounts for one child of a wave; when the whole wave is in,
// any residual is re-routed (bounded by MaxReplansPerSlice) away from venues
// that failed.
func (e *Engine) waveResolved(c *domain.ChildOrder, filled int64, failed bool) {
	w := e.waves[c.ID]
	if w == nil {
		return
	}
	delete(e.waves, c.ID)
	w.filled += filled
	w.pending--
	if failed && !slices.Contains(w.exclude, c.Venue) {
		w.exclude = append(w.exclude, c.Venue)
	}
	residual := w.want - w.filled
	if w.pending > 0 || residual < e.cfg.LotSize || w.attempt >= e.cfg.Router.MaxReplansPerSlice {
		return
	}
	e.at(e.now+e.cfg.Router.ReplanDelayMs, func() {
		p := e.book.Parent(w.parentID)
		e.route(p, min(residual, oms.Available(p)), w.maxLevels, w.attempt+1, w.exclude)
	})
}

// settle logs completion and expires a parent whose deadline has passed once
// nothing is in flight.
func (e *Engine) settle(id string) {
	p, m := e.book.Parent(id), e.meta[id]
	if m.deadlinePassed && e.book.InFlight(id) == 0 {
		e.book.FinishParent(id, domain.ParentExpired, "deadline reached", e.now)
	}
	if p.Status != domain.ParentWorking && !m.doneLogged {
		m.doneLogged = true
		e.emit("parent_done", p)
	}
}

// SetKill engages or releases the kill switch. Engaging it cancels every
// working parent; children already at a venue are still accounted for.
func (e *Engine) SetKill(on bool) {
	e.risk.SetKill(on)
	e.emit("kill_switch", map[string]bool{"engaged": on})
	if !on {
		return
	}
	for _, id := range e.book.ParentIDs() {
		if e.book.FinishParent(id, domain.ParentCancelled, "KILL_SWITCH", e.now) {
			e.settle(id)
		}
	}
}

func (e *Engine) Killed() bool { return e.risk.Killed() }

// CancelParent cancels one working parent.
func (e *Engine) CancelParent(id string) bool {
	ok := e.book.FinishParent(id, domain.ParentCancelled, "cancelled by user", e.now)
	if ok {
		e.settle(id)
	}
	return ok
}

// SetFault replaces a venue's fault state. Clearing `down` triggers an
// order-status reconciliation for children whose reports were lost.
func (e *Engine) SetFault(venue string, f venuesim.Fault) error {
	v, ok := e.venues[venue]
	if !ok {
		return fmt.Errorf("unknown venue %q", venue)
	}
	if f.RejectRate < 0 || f.RejectRate > 1 || f.DuplicateRate < 0 || f.DuplicateRate > 1 || f.LatencyMs < 0 {
		return fmt.Errorf("rates must be within [0,1] and latency_ms >= 0")
	}
	wasDown := v.Fault().Down
	v.SetFault(f)
	e.view.SetLink(venue, !f.Down)
	e.emit("fault", map[string]any{"venue": venue, "fault": f})
	if wasDown && !f.Down {
		for _, pid := range e.book.ParentIDs() {
			for _, c := range e.book.Children(pid) {
				if c.Venue != venue || c.Status.Terminal() {
					continue
				}
				if rep, ok := v.Result(c.ID); ok {
					e.emit("reconcile", map[string]string{"child_id": c.ID, "venue": venue})
					e.at(e.now+v.Latency(), func() { e.deliver(rep) })
				}
			}
		}
	}
	return nil
}

func (e *Engine) Fault(venue string) (venuesim.Fault, bool) {
	v, ok := e.venues[venue]
	if !ok {
		return venuesim.Fault{}, false
	}
	return v.Fault(), true
}

// MarketView returns the current normalized quotes and venue status.
func (e *Engine) MarketView() marketview.Snapshot { return e.view.Peek(e.now) }

func (e *Engine) Parent(id string) (domain.ParentOrder, bool) {
	p := e.book.Parent(id)
	if p == nil {
		return domain.ParentOrder{}, false
	}
	return *p, true
}

func (e *Engine) Parents() []domain.ParentOrder {
	out := []domain.ParentOrder{}
	for _, id := range e.book.ParentIDs() {
		out = append(out, *e.book.Parent(id))
	}
	return out
}

func (e *Engine) Children(parentID string) []domain.ChildOrder {
	out := []domain.ChildOrder{}
	for _, c := range e.book.Children(parentID) {
		out = append(out, *c)
	}
	return out
}

func (e *Engine) Fills(parentID string) []domain.Fill {
	return append([]domain.Fill{}, e.book.Fills(parentID)...)
}

// Report builds the execution-quality report for a parent.
func (e *Engine) Report(parentID string) (tca.Report, bool) {
	p, ok := e.Parent(parentID)
	if !ok {
		return tca.Report{}, false
	}
	end := e.now
	if p.DoneMs > 0 {
		end = p.DoneMs
	}
	return tca.Build(p, e.Children(parentID), e.Fills(parentID), e.MarketVWAPMills(p.StartMs, end)), true
}
