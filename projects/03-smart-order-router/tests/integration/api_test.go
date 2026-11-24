// Package integration exercises the HTTP API end to end against a server on
// a manual simulated clock.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sor/src/api"
	"sor/src/domain"
	"sor/src/engine"
	"sor/src/marketview"
	"sor/src/replay"
	"sor/src/tca"
)

type client struct {
	t   *testing.T
	url string
}

func newClient(t *testing.T, log io.Writer) client {
	t.Helper()
	srv := httptest.NewServer(api.New(engine.Config{Seed: 4}, "ALPHA", 0, log).Handler())
	t.Cleanup(srv.Close)
	c := client{t, srv.URL}
	c.advance(1000)
	return c
}

// do sends a request, asserts the status code and decodes the JSON body into out.
func (c client) do(method, path, body string, wantCode int, out any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantCode {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

func (c client) advance(ms int) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]int{"ms": ms})
	c.do("POST", "/sim/advance", string(b), 200, nil)
}

const order = `{"symbol":"XYZ","side":"BUY","qty":10000,"strategy":"TWAP","duration_seconds":600}`

func TestParentLifecycleOverHTTP(t *testing.T) {
	var log bytes.Buffer
	c := newClient(t, &log)
	var p domain.ParentOrder
	c.do("POST", "/parents", order, 201, &p)
	if p.ID != "P-000001" || p.Status != domain.ParentWorking || p.DeadlineMs-p.StartMs != 600_000 || p.ArrivalMidMills == 0 {
		t.Fatalf("parent %+v", p)
	}
	c.advance(300_000)
	c.do("GET", "/parents/"+p.ID, "", 200, &p)
	if p.Status != domain.ParentWorking || p.FilledQty < 4000 || p.FilledQty > 5200 {
		t.Fatalf("halfway through a TWAP, filled %d", p.FilledQty)
	}
	c.advance(301_000)

	var children []domain.ChildOrder
	var fills []domain.Fill
	var rep tca.Report
	var all []domain.ParentOrder
	c.do("GET", "/parents/"+p.ID+"/children", "", 200, &children)
	c.do("GET", "/parents/"+p.ID+"/fills", "", 200, &fills)
	c.do("GET", "/parents/"+p.ID+"/execution-quality", "", 200, &rep)
	c.do("GET", "/parents", "", 200, &all)
	var sum int64
	for _, f := range fills {
		sum += f.Qty
	}
	if sum != 10000 || len(children) == 0 || len(all) != 1 || all[0].Status != domain.ParentFilled {
		t.Fatalf("fills sum %d, %d children, parents %+v", sum, len(children), all)
	}
	if rep.Label != "SIMULATED / PAPER" || rep.FilledQty != 10000 || rep.UnfilledQty != 0 || rep.AvgFillPx == 0 ||
		rep.MarketVWAPMills == 0 || len(rep.Venues) != 3 {
		t.Fatalf("report %+v", rep)
	}
	if b := rep.Baseline; b == nil || b.Venue != "ALPHA" || b.FilledQty == 0 {
		t.Fatalf("baseline %+v", rep.Baseline)
	}
	// The log written by the live server replays cleanly.
	if res, err := replay.Verify(&log); err != nil || res.Decisions == 0 || len(res.Mismatches) != 0 {
		t.Fatalf("replay %+v %v", res, err)
	}
}

func TestFaultInjectionOverHTTP(t *testing.T) {
	c := newClient(t, nil)
	var p domain.ParentOrder
	c.do("POST", "/parents", order, 201, &p)
	c.advance(65_000) // between slices, so nothing is in flight to BRAVO when its link drops
	c.do("POST", "/sim/venues/BRAVO/faults", `{"down":true}`, 200, nil)
	var view marketview.Snapshot
	c.do("GET", "/venues", "", 200, &view)
	if len(view.Venues) != 3 || view.Venues[1].Venue != "BRAVO" || view.Venues[1].Status != marketview.Down || view.Venues[0].Status != marketview.OK {
		t.Fatalf("view %+v", view.Venues)
	}
	var faultAt struct {
		SimTimeMs int64 `json:"sim_time_ms"`
	}
	c.do("GET", "/healthz", "", 200, &faultAt)
	c.advance(536_000)
	var children []domain.ChildOrder
	c.do("GET", "/parents/"+p.ID+"/children", "", 200, &children)
	for _, ch := range children {
		// A child sent in the same millisecond, before the fault landed, was legitimately in flight.
		if ch.Venue == "BRAVO" && ch.SentMs > faultAt.SimTimeMs {
			t.Fatalf("child routed to a down venue: %+v", ch)
		}
	}
	c.do("GET", "/parents/"+p.ID, "", 200, &p)
	if p.Status != domain.ParentFilled {
		t.Fatalf("router did not complete around the outage: %+v", p)
	}
	c.do("POST", "/sim/venues/NOPE/faults", `{"down":true}`, 404, nil)
	c.do("POST", "/sim/venues/BRAVO/faults", `{"reject_rate":1.5}`, 400, nil)
	c.do("POST", "/sim/venues/BRAVO/faults", `{"latency_ms":200,"reject_rate":0.1}`, 200, nil)
}

func TestValidationKillSwitchAndCancel(t *testing.T) {
	c := newClient(t, nil)
	var rej struct {
		Code   string             `json:"code"`
		Parent domain.ParentOrder `json:"parent"`
	}
	c.do("POST", "/parents", `{"symbol":"NOPE","side":"BUY","qty":100,"duration_seconds":60}`, 422, &rej)
	if rej.Code != "SYMBOL_NOT_ALLOWED" || rej.Parent.Status != domain.ParentRejected {
		t.Fatalf("rejection %+v", rej)
	}
	c.do("POST", "/parents", `{"symbol":"XYZ","side":"BUY","qty":-5,"duration_seconds":60}`, 422, &rej)
	c.do("POST", "/parents", `{not json`, 400, nil)
	c.do("POST", "/parents", `{"symbol":"XYZ","side":"BUY","qty":100,"duration_seconds":60,"leverage":9}`, 400, nil)
	c.do("GET", "/parents/P-999999", "", 404, nil)
	c.do("GET", "/parents/P-999999/execution-quality", "", 404, nil)

	var p, p2 domain.ParentOrder
	c.do("POST", "/parents", order, 201, &p)
	c.do("POST", "/parents", order, 201, &p2)
	c.advance(30_000)
	c.do("POST", "/parents/"+p2.ID+"/cancel", "", 200, &p2)
	c.do("POST", "/parents/"+p2.ID+"/cancel", "", 409, nil)
	c.do("POST", "/kill-switch", `{"engaged":true}`, 200, nil)
	c.do("POST", "/parents", order, 422, &rej)
	if rej.Code != "KILL_SWITCH" || p2.Status != domain.ParentCancelled {
		t.Fatalf("code %s, p2 %+v", rej.Code, p2)
	}
	c.advance(600_000)
	c.do("GET", "/parents/"+p.ID, "", 200, &p)
	if p.Status != domain.ParentCancelled || p.Reason != "KILL_SWITCH" || p.FilledQty >= 10000 {
		t.Fatalf("parent %+v", p)
	}
	resp, err := http.Get(c.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"sor_kill_switch_engaged 1", `sor_child_orders_total{venue="ALPHA"}`, `sor_filled_qty_total{venue="BRAVO"}`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}
