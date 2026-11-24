// bench is an in-process load test. It replays a synthetic feed through a
// real engine (journal on disk, all four rules on) as fast as it will go
// and prints what it measured on this machine, this run. Nothing here is
// a target or a promise; see docs/benchmarks.md for how to read it.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"surveillance/src/api"
	"surveillance/src/engine"
	"surveillance/src/evaluation"
	"surveillance/src/normalize"
)

func main() {
	scenarios := flag.Int("scenarios", 3000, "synthetic scenarios to generate (about 66 events each)")
	seed := flag.Uint64("seed", 42, "scenario seed")
	batch := flag.Int("batch", 500, "events per ingest call")
	rulesPath := flag.String("rules", "examples/rules.json", "ruleset file")
	sync := flag.Bool("sync", false, "fsync the journal on every record")
	overHTTP := flag.Bool("http", false, "send batches through POST /v1/events on loopback instead of calling the engine directly")
	flag.Parse()

	if err := run(*scenarios, *seed, *batch, *rulesPath, *sync, *overHTTP); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p/100+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func heap() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

func run(scenarios int, seed uint64, batch int, rulesPath string, sync, overHTTP bool) error {
	if batch < 1 || scenarios < 1 {
		return fmt.Errorf("-batch and -scenarios must be positive")
	}
	scs, err := evaluation.Build(evaluation.Options{Seed: seed, Scenarios: scenarios})
	if err != nil {
		return err
	}
	stream := evaluation.Stream(scs, seed, 20)
	scs = nil // only the merged feed stays in memory

	dir, err := os.MkdirTemp("", "surv-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cfg := engine.Config{DataDir: dir, RulesPath: rulesPath, SyncJournal: sync}
	eng, err := engine.Open(cfg)
	if err != nil {
		return err
	}

	// send delivers one batch and reports how many events were stored.
	send := func(evs []normalize.Raw) (int, error) {
		res, err := eng.Ingest(evs, false)
		return res.Accepted + res.Late, err
	}
	path := "engine.Ingest (in-process, no HTTP)"
	if overHTTP {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		const tok = "bench-ingest-token-0001"
		srv := &http.Server{Handler: api.New(api.Config{Engine: eng, IngestToken: tok})}
		go srv.Serve(ln)
		defer srv.Close()
		url := "http://" + ln.Addr().String() + "/v1/events"
		path = "POST /v1/events on loopback, one connection, JSON encode and decode included"
		send = func(evs []normalize.Raw) (int, error) {
			body, err := json.Marshal(evs)
			if err != nil {
				return 0, err
			}
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK {
				return 0, fmt.Errorf("status %d: %s", res.StatusCode, raw)
			}
			var out engine.IngestResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return 0, err
			}
			return out.Accepted + out.Late, nil
		}
	}

	heapBefore := heap()
	var calls []time.Duration
	stored := 0
	start := time.Now()
	for i := 0; i < len(stream); i += batch {
		t0 := time.Now()
		n, err := send(stream[i:min(i+batch, len(stream))])
		if err != nil {
			return err
		}
		calls = append(calls, time.Since(t0))
		stored += n
	}
	if _, err := eng.Flush(); err != nil {
		return err
	}
	elapsed := time.Since(start)
	if stored != len(stream) {
		return fmt.Errorf("stored %d of %d events", stored, len(stream))
	}
	st := eng.Stats()
	heapAfter := heap()
	runtime.KeepAlive(stream) // so the feed is in both heap readings

	// Receipt of the triggering event to the alert being raised, per alert.
	var waits []time.Duration
	for _, a := range eng.Alerts(engine.AlertFilter{}) {
		if !a.TriggerReceivedAt.IsZero() {
			waits = append(waits, a.FirstDetectedAt.Sub(a.TriggerReceivedAt))
		}
	}
	if err := eng.Close(); err != nil {
		return err
	}

	// Recovery: open the same directory again and time the replay.
	t0 := time.Now()
	eng2, err := engine.Open(cfg)
	if err != nil {
		return err
	}
	recovery := time.Since(t0)
	st2 := eng2.Stats()
	eng2.Close()
	if st2.Alerts.Total != st.Alerts.Total || st2.Events.Total != st.Events.Total {
		return fmt.Errorf("recovery rebuilt %d events and %d alerts, expected %d and %d",
			st2.Events.Total, st2.Alerts.Total, st.Events.Total, st.Alerts.Total)
	}
	info, err := os.Stat(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return err
	}

	sort.Slice(calls, func(i, j int) bool { return calls[i] < calls[j] })
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	n := float64(len(stream))
	rs := eng2.Ruleset()

	fmt.Println("Measured on this machine, this run. Synthetic feed, single process. Not a target or a guarantee.")
	fmt.Printf("\nmachine      %s/%s, %d logical CPUs, %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Printf("ruleset      %s, window %ds hop %ds, allowed lateness %dms\n", rs.Version, rs.Config.Window.SizeS, rs.Config.Window.HopS, rs.Config.Window.AllowedLatenessMs)
	fmt.Printf("feed         %d events from %d scenarios (seed %d), 20%% of neighbours swapped, %d late\n", len(stream), scenarios, seed, st.Events.Late)
	fmt.Printf("path         %s\n", path)
	fmt.Printf("journal      fsync per record: %v, %.1f MiB written (%.0f bytes per event, alerts included)\n", sync, float64(info.Size())/(1<<20), float64(info.Size())/n)
	fmt.Printf("\nthroughput   %.0f events/s (%d events in %s, batches of %d)\n", n/elapsed.Seconds(), len(stream), elapsed.Round(time.Millisecond), batch)
	fmt.Printf("per batch    p50 %s  p99 %s  max %s  (%d calls)\n", pct(calls, 50).Round(time.Microsecond), pct(calls, 99).Round(time.Microsecond), calls[len(calls)-1].Round(time.Microsecond), len(calls))
	fmt.Printf("per event    %.1f us mean (batch time / events)\n", elapsed.Seconds()*1e6/n)
	fmt.Printf("alerts       %d (%.0f per million events), %d detections, %.1f%% folded by suppression\n",
		st.Alerts.Total, st.Alerts.PerMillionEvents, st.Alerts.Detections, 100*st.Alerts.DuplicateRate)
	if len(waits) > 0 {
		fmt.Printf("alert wait   p50 %s  p99 %s  max %s  (receipt of trigger event -> alert, %d alerts; replay speed, see note)\n",
			pct(waits, 50).Round(time.Microsecond), pct(waits, 99).Round(time.Microsecond), waits[len(waits)-1].Round(time.Microsecond), len(waits))
	}
	fmt.Printf("heap         %.1f MiB of live engine state after ingest (%.0f bytes per event; the feed itself, %.1f MiB, excluded)\n",
		heapAfter-heapBefore, (heapAfter-heapBefore)*(1<<20)/n, heapBefore)
	fmt.Printf("recovery     %s to reopen and replay the journal (%.0f events/s)\n", recovery.Round(time.Millisecond), n/recovery.Seconds())
	fmt.Println("\nnote: in replay, hours of event time pass in seconds, so 'alert wait' shows processing delay only.")
	fmt.Printf("On a live feed an alert cannot be raised before the watermark passes the evidence: at least the\n"+
		"allowed lateness (%dms) after the window or pattern ends, and up to the idle timeout (%dms) more if the feed goes quiet.\n",
		rs.Config.Window.AllowedLatenessMs, rs.Config.Window.IdleTimeoutMs)
	return nil
}
