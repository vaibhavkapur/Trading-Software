package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"mdp/src/domain"
	"mdp/src/ingestors"
	"mdp/src/refdata"
	"mdp/src/stream"
)

// demo is a scripted walkthrough. Every figure it prints is read from the
// running pipeline, and every claim it makes is checked: a failed check is
// printed as FAIL and makes the command exit non-zero.
type demo struct {
	*rig
	ws       *wsClient
	failures int
	// expectations, maintained as the script injects faults
	raw    int64 // provider events that should be in the raw topic
	facts  int64 // distinct trade, quote and depth facts that should be stored
	pace   time.Duration
	frames struct {
		sync.Mutex
		alpha, beta string
	}
}

func (d *demo) step(title string) { fmt.Printf("\n== %s\n", title) }

func (d *demo) say(format string, args ...any) { fmt.Printf("   "+format+"\n", args...) }

func (d *demo) check(ok bool, format string, args ...any) {
	if ok {
		fmt.Printf("   ok    "+format+"\n", args...)
		return
	}
	d.failures++
	fmt.Printf("   FAIL  "+format+"\n", args...)
}

// feed publishes n events on each venue, paced.
func (d *demo) feed(n int) {
	for i := 0; i < n; i++ {
		d.sim.Step(1)
		time.Sleep(d.pace)
	}
}

// sync waits until the pipeline has absorbed everything published so far.
func (d *demo) sync() error {
	if err := d.waitRaw(d.raw, 20*time.Second); err != nil {
		return err
	}
	return d.settle(20 * time.Second)
}

func (d *demo) sum(name string, labels ...string) int64 { return int64(d.st.M.Sum(name, labels...)) }

// distinct counts the distinct facts in the canonical topics; the log may
// hold a fact more than once.
func (d *demo) distinct() (records, facts int) {
	seen := map[string]bool{}
	for _, topic := range []string{stream.TopicTrades, stream.TopicQuotes, stream.TopicDepth} {
		for _, r := range d.mem.All(topic) {
			records++
			var h struct {
				InstrumentID string    `json:"instrument_id"`
				EventTime    time.Time `json:"event_time"`
				TradeID      string    `json:"trade_id"`
				Seq          uint64    `json:"seq"`
			}
			json.Unmarshal(r.Value, &h)
			seen[fmt.Sprint(topic, h.InstrumentID, h.EventTime.UnixNano(), h.TradeID, h.Seq)] = true
		}
	}
	return records, len(seen)
}

func (d *demo) rejects() map[string]int {
	out := map[string]int{}
	for _, r := range d.mem.All(stream.TopicRejects) {
		var rej domain.Reject
		if json.Unmarshal(r.Value, &rej) == nil {
			out[rej.Reason]++
		}
	}
	return out
}

func (d *demo) verifyStore(what string) {
	stored, _ := d.stored()
	d.check(stored == d.facts, "%s: store holds %d rows, expected %d", what, stored, d.facts)
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func cmdDemo(ctx context.Context, args []string) error {
	c := newFlags("demo")
	seed := c.fs.Uint64("seed", 1, "random seed")
	rate := c.fs.Int("rate", 500, "simulated events per second per venue")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	if *rate < 1 {
		return errors.New("--rate must be at least 1")
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	opt := c.options()
	opt.Hub.StaleAfter = time.Second
	opt.Server.HeartbeatEvery = 200 * time.Millisecond
	opt.Sink.FlushEvery = 20 * time.Millisecond
	opt.Sink.BackoffMax = 100 * time.Millisecond
	opt.Ingest.CheckpointEvery = 50 * time.Millisecond
	opt.Normalize.CheckpointEvery = 50 * time.Millisecond
	r, err := newRig(ctx, reg, nil, nil, opt, *seed, nil)
	if err != nil {
		return err
	}
	d := &demo{rig: r, pace: time.Second / time.Duration(*rate)}

	fmt.Println("Market data platform: walkthrough")
	fmt.Println("SIMULATED DATA ONLY. Two invented venues publish random-walk prices; nothing here is")
	fmt.Println("real market data and nothing connects to a real venue or account. This process runs the")
	fmt.Println("same services as the Kafka/ClickHouse deployment on an in-memory log and store.")

	if err := d.startIngestors(); err != nil {
		return err
	}
	// Capture one raw trade frame per venue to show the wire formats. (A tap
	// counts as a venue subscriber, so it is attached after the ingestors.)
	d.sim.Alpha.Tap(func(f []byte) {
		d.frames.Lock()
		if d.frames.alpha == "" && strings.Contains(string(f), `"trade"`) {
			d.frames.alpha = string(f)
		}
		d.frames.Unlock()
	})
	d.sim.Beta.Tap(func(f []byte) {
		d.frames.Lock()
		if d.frames.beta == "" && strings.HasPrefix(string(f), "T|") {
			d.frames.beta = strings.TrimSpace(string(f))
		}
		d.frames.Unlock()
	})

	d.startNormalizer()
	d.startSink()
	if d.ws, err = d.subscribe("instrument_id=venueA:XYZ,venueB:XYZ.B", nil); err != nil {
		return err
	}
	defer d.ws.close()

	// ---------------------------------------------------------------
	d.step("1. Two feeds with different wire formats become one schema")
	d.feed(400)
	d.raw, d.facts = 800, 800
	if err := d.sync(); err != nil {
		return err
	}
	d.frames.Lock()
	d.say("venue alpha sent (JSON over WebSocket):   %s", d.frames.alpha)
	d.say("venue beta sent (delimited over TCP):     %s", d.frames.beta)
	d.frames.Unlock()
	for _, provider := range []string{"alpha", "beta"} {
		for _, rec := range d.mem.All(stream.TopicTrades) {
			var tr domain.Trade
			if json.Unmarshal(rec.Value, &tr) == nil && tr.Provider == provider {
				d.say("canonical trade from %-5s                 %s", provider, rec.Value)
				break
			}
		}
	}
	raw, _ := d.rawCount()
	records, facts := d.distinct()
	d.check(raw == 800, "800 provider events published, %d in the raw topic", raw)
	d.check(records == 800 && facts == 800, "%d canonical events, %d distinct", records, facts)
	d.verifyStore("clean flow")
	d.check(len(d.mem.All(stream.TopicGaps)) == 0, "no gaps declared")

	code, body, err := d.get("/v1/trades?instrument_id=venueA:XYZ&limit=2")
	if err != nil {
		return err
	}
	d.say("GET /v1/trades?instrument_id=venueA:XYZ&limit=2  ->  %d", code)
	if trades, ok := body["trades"].([]any); ok {
		for _, tr := range trades {
			d.say("    %s", compact(tr))
		}
	}
	d.say("    tick_size=%v lot_size=%v retention=%s", body["tick_size"], body["lot_size"], compact(body["retention"]))
	d.say("    clocks=%s", compact(body["clocks"]))
	d.check(code == 200 && body["count"] == float64(2), "historical query answered from the store")
	events, _, _ := d.ws.snapshot()
	d.check(events > 0, "live subscriber (WS /v1/stream) has received %d events for 2 instruments", events)

	// ---------------------------------------------------------------
	d.step("2. Both venues drop the connection: the ingestors resume where they stopped")
	d.sim.Alpha.Disconnect()
	d.sim.Beta.Disconnect()
	d.feed(200)
	d.raw, d.facts = d.raw+400, d.facts+400
	if err := d.sync(); err != nil {
		return err
	}
	reconnects := d.sum("mdp_ingest_reconnects_total")
	d.check(reconnects >= 2, "mdp_ingest_reconnects_total = %d", reconnects)
	d.check(len(d.mem.All(stream.TopicGaps)) == 0, "events published during the reconnect were resent by the venues: no gap")
	d.verifyStore("after reconnect")

	// ---------------------------------------------------------------
	d.step("3. Venue beta loses 4 events for good: the loss is declared, not hidden")
	d.sim.Beta.LoseNext(4)
	d.feed(100)
	d.raw, d.facts = d.raw+196, d.facts+196
	if err := d.sync(); err != nil {
		return err
	}
	var missing uint64
	for _, rec := range d.mem.All(stream.TopicGaps) {
		var g domain.Gap
		if json.Unmarshal(rec.Value, &g) == nil {
			missing += g.Missing()
			d.say("gap record: %s", rec.Value)
		}
	}
	d.check(missing == 4, "declared missing sequence numbers = %d", missing)
	d.check(d.sum("mdp_ingest_resyncs_total", "provider", "beta") >= 1, "the ingestor first asked the venue to resend (mdp_ingest_resyncs_total{provider=\"beta\"} = %d)",
		d.sum("mdp_ingest_resyncs_total", "provider", "beta"))
	if err := d.wait(5*time.Second, "gap notice on the WebSocket", func() bool { _, gaps, _ := d.ws.snapshot(); return len(gaps) > 0 }); err != nil {
		d.check(false, "live subscriber was told about the gap")
	} else {
		_, gaps, _ := d.ws.snapshot()
		d.say("live subscriber received: %s", compact(gaps[0]))
		d.check(gaps[0]["reason"] == "feed_gap" && gaps[0]["missing"] == float64(4), "live subscriber was told about the gap in-band")
	}
	code, body, _ = d.get("/v1/gaps?instrument_id=venueB:XYZ.B")
	d.check(code == 200 && body["count"] == float64(1), "GET /v1/gaps?instrument_id=venueB:XYZ.B reports %v gap", body["count"])
	d.verifyStore("after loss")

	// ---------------------------------------------------------------
	d.step("4. Duplicate delivery, unparseable frames and an off-tick price")
	d.sim.Alpha.DuplicateNext(5)
	d.sim.Alpha.GarbageNext(2)
	d.sim.Alpha.OffTickNext(1)
	d.feed(100)
	d.raw += 200
	if err := d.waitRaw(d.raw, 20*time.Second); err != nil {
		return err
	}
	if err := d.settle(20 * time.Second); err != nil {
		return err
	}
	rej := d.rejects()
	d.facts += 200 - int64(rej["PRICE_NOT_ON_TICK"])
	d.check(d.sum("mdp_ingest_duplicates_total", "provider", "alpha") == 5, "5 frames sent twice; mdp_ingest_duplicates_total{provider=\"alpha\"} = %d",
		d.sum("mdp_ingest_duplicates_total", "provider", "alpha"))
	d.check(rej["UNPARSEABLE_FRAME"] == 2 && rej["PRICE_NOT_ON_TICK"] == 1, "dead letters by reason: %v", rej)
	for _, rec := range d.mem.All(stream.TopicRejects) {
		if strings.Contains(string(rec.Value), "PRICE_NOT_ON_TICK") {
			d.say("dead letter: %s", rec.Value)
		}
	}
	d.say("a price between two ticks is rejected, never rounded: prices are integer ticks end to end")
	d.verifyStore("after bad input")

	// ---------------------------------------------------------------
	d.step("5. A venue clock running ahead, and late events: flagged, timestamps untouched")
	d.sim.Alpha.SetSkew(2 * time.Second)
	d.sim.Beta.StaleNext(3, 30*time.Second)
	d.feed(20)
	d.sim.Alpha.SetSkew(0)
	d.raw, d.facts = d.raw+40, d.facts+40
	if err := d.sync(); err != nil {
		return err
	}
	d.check(d.sum("mdp_flagged_events_total", "flag", domain.FlagClockSkew) == 20, "mdp_flagged_events_total{flag=\"clock_skew\"} = %d",
		d.sum("mdp_flagged_events_total", "flag", domain.FlagClockSkew))
	d.check(d.sum("mdp_flagged_events_total", "flag", domain.FlagStale) == 3, "mdp_flagged_events_total{flag=\"stale\"} = %d",
		d.sum("mdp_flagged_events_total", "flag", domain.FlagStale))
	for _, topic := range []string{stream.TopicTrades, stream.TopicQuotes} {
		shown := false
		for _, rec := range d.mem.All(topic) {
			var ev struct {
				Flags       []string  `json:"flags"`
				EventTime   time.Time `json:"event_time"`
				ReceiveTime time.Time `json:"receive_time"`
			}
			if json.Unmarshal(rec.Value, &ev) == nil && len(ev.Flags) > 0 && ev.EventTime.After(ev.ReceiveTime) && !shown {
				shown = true
				d.say("event_time (venue clock) %s", ev.EventTime.Format(time.RFC3339Nano))
				d.say("receive_time (our clock) %s   flags=%v", ev.ReceiveTime.Format(time.RFC3339Nano), ev.Flags)
				d.check(ev.EventTime.Sub(ev.ReceiveTime) > time.Second, "the venue's timestamp is kept as sent, %s ahead of ours; the two clocks are never merged",
					ev.EventTime.Sub(ev.ReceiveTime).Round(time.Millisecond))
			}
		}
		if shown {
			break
		}
	}
	d.verifyStore("after clock faults")

	// ---------------------------------------------------------------
	d.step("6. Venue alpha goes silent: subscribers see it go stale")
	d.sim.Alpha.Pause(true)
	began := time.Now()
	for time.Since(began) < 1500*time.Millisecond {
		d.sim.Step(1) // only beta publishes
		d.raw, d.facts = d.raw+1, d.facts+1
		time.Sleep(10 * time.Millisecond)
	}
	var stA, stB map[string]any
	err = d.wait(5*time.Second, "stale status", func() bool {
		_, _, status := d.ws.snapshot()
		if status == nil {
			return false
		}
		for _, v := range status["instruments"].([]any) {
			is := v.(map[string]any)
			switch is["instrument_id"] {
			case "venueA:XYZ":
				stA = is
			case "venueB:XYZ.B":
				stB = is
			}
		}
		return stA != nil && stA["stale"] == true
	})
	d.say("status for venueA:XYZ:   %s", compact(stA))
	d.say("status for venueB:XYZ.B: %s", compact(stB))
	d.check(err == nil && stB != nil && stB["stale"] == false, "venueA:XYZ is reported stale while venueB:XYZ.B stays fresh")
	d.sim.Alpha.Pause(false)
	d.feed(20)
	d.raw, d.facts = d.raw+40, d.facts+40
	if err := d.sync(); err != nil {
		return err
	}
	d.verifyStore("after the silence")

	// ---------------------------------------------------------------
	d.step("7. The store goes down: lag builds up where you can see it, then drains")
	d.memDB.SetFault(func(string, int) error { return errors.New("injected: store unreachable") })
	before, _, _ := d.ws.snapshot()
	d.feed(300)
	d.raw, d.facts = d.raw+600, d.facts+600
	if err := d.waitRaw(d.raw, 20*time.Second); err != nil {
		return err
	}
	lag := d.lag(d.sink.Progress)
	d.say("sink consumer lag = %d records, mdp_sink_retrying = %d, failed inserts = %d", lag, d.sum("mdp_sink_retrying"), d.sum("mdp_sink_insert_failures_total"))
	d.check(lag > 0 && d.sum("mdp_sink_retrying") == 1, "the outage is visible as lag and as a retrying sink")
	code, body, _ = d.get("/v1/trades?instrument_id=venueA:XYZ")
	d.say("GET /v1/trades during the outage -> %d %s", code, compact(body["error"]))
	d.check(code == 503, "historical queries fail loudly instead of returning partial data")
	after, _, _ := d.ws.snapshot()
	d.check(after > before, "live delivery does not depend on the store: %d more events reached the subscriber", after-before)
	stored, _ := d.stored()
	d.memDB.SetFault(nil)
	began = time.Now()
	if err := d.settle(20 * time.Second); err != nil {
		return err
	}
	d.say("store back: sink drained %d records in %s", d.facts-stored, time.Since(began).Round(time.Millisecond))
	d.check(d.lag(d.sink.Progress) == 0, "sink lag is back to 0")
	d.verifyStore("after the outage (nothing dropped, nothing doubled)")

	// ---------------------------------------------------------------
	d.step("8. The normalizer dies before saving its position: events are reprocessed, facts are not duplicated")
	d.mem.SetProduceHook(func(recs []stream.Record) error {
		for _, rec := range recs {
			if rec.Topic == stream.TopicCheckpoints && strings.HasPrefix(string(rec.Key), "normalizer/") {
				return errors.New("injected: checkpoint write lost")
			}
		}
		return nil
	})
	d.feed(200)
	d.raw, d.facts = d.raw+400, d.facts+400
	if err := d.sync(); err != nil {
		return err
	}
	d.normSvc.stop()
	d.mem.SetProduceHook(nil)
	suppressed := d.sum("mdp_ws_duplicates_suppressed_total")
	began = time.Now()
	d.startNormalizer()
	if err := d.sync(); err != nil {
		return err
	}
	records, facts = d.distinct()
	d.say("restarted normalizer caught up in %s", time.Since(began).Round(time.Millisecond))
	d.say("canonical log: %d records for %d distinct facts (%d delivered twice: at-least-once)", records, facts, records-facts)
	d.check(records > facts, "the restart did redeliver events")
	d.verifyStore("after redelivery (the store's sorting key is the deduplication key)")
	d.check(d.sum("mdp_ws_duplicates_suppressed_total") > suppressed, "the gateway did not forward the repeats to subscribers (%d suppressed)",
		d.sum("mdp_ws_duplicates_suppressed_total")-suppressed)
	d.check(len(d.mem.All(stream.TopicGaps)) == 1, "no new gap was declared")

	// ---------------------------------------------------------------
	d.step("Metrics (GET /metrics on the gateway), a selection")
	resp, err := http.Get(d.url + "/metrics")
	if err != nil {
		return err
	}
	text, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var lines []string
	for _, line := range strings.Split(string(text), "\n") {
		for _, prefix := range []string{"mdp_ingest_events_total", "mdp_ingest_bytes_total", "mdp_ingest_reconnects_total", "mdp_gaps_total",
			"mdp_gap_missing_events_total", "mdp_duplicates_total", "mdp_ingest_duplicates_total", "mdp_normalizer_rejects_total",
			"mdp_sink_rows_total", "mdp_sink_insert_failures_total", "mdp_consumer_catchup_seconds", "mdp_ws_dropped_total"} {
			if strings.HasPrefix(line, prefix) {
				lines = append(lines, line)
			}
		}
	}
	sort.Strings(lines)
	for _, line := range lines {
		d.say("%s", line)
	}
	for _, stage := range []string{"normalized", "stored", "delivered"} {
		p50, n := d.st.M.Quantile("mdp_pipeline_latency_seconds", 0.50, "stage", stage)
		p99, _ := d.st.M.Quantile("mdp_pipeline_latency_seconds", 0.99, "stage", stage)
		d.say("receive -> %-10s p50 <= %s, p99 <= %s  (histogram bucket bounds, n=%d; includes the injected outage)", stage,
			time.Duration(p50*float64(time.Second)).Round(10*time.Microsecond), time.Duration(p99*float64(time.Second)).Round(10*time.Microsecond), n)
	}

	fmt.Println()
	var health []string
	for _, h := range []*ingestors.Runner{d.alpha, d.beta} {
		ih := h.Health()
		health = append(health, fmt.Sprintf("%s: events=%d gaps=%d duplicates=%d reconnects=%d resyncs=%d parse_errors=%d", ih.Provider, ih.Events, ih.Gaps,
			ih.Duplicates, ih.Reconnects, ih.Resyncs, ih.ParseErrors))
	}
	fmt.Println("Ingest health:  " + strings.Join(health, "\n                "))
	if d.failures > 0 {
		return fmt.Errorf("%d check(s) failed", d.failures)
	}
	fmt.Println("All checks passed. Latency figures above are from this run on this machine and are not a performance claim.")
	return nil
}
