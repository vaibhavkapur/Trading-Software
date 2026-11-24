package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/store"
	"oms/tests/harness"
)

// These tests run the shipped binaries (cmd/omsd and cmd/brokersim) as
// separate processes configured only through their environment, the way
// docker-compose.yml runs them.

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func binaries(t *testing.T) (omsd, sim string) {
	t.Helper()
	buildOnce.Do(func() {
		// Built into the git-ignored cache directory, so repeated runs reuse
		// the build cache and leave nothing behind in the system temp dir.
		if binDir, buildErr = filepath.Abs("../../.cache/test-bin"); buildErr != nil {
			return
		}
		for _, name := range []string{"omsd", "brokersim"} {
			cmd := exec.Command("go", "build", "-race", "-o", filepath.Join(binDir, name), "./src/cmd/"+name)
			cmd.Dir = "../.."
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", name, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, "omsd"), filepath.Join(binDir, "brokersim")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type proc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
	err  error
}

// start runs a binary with exactly the given environment.
func start(t *testing.T, bin string, env map[string]string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(bin, args...), out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(bin), err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// wait returns the exit code once the process ends.
func (p *proc) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		t.Fatalf("process did not exit within %s; output:\n%s", within, p.out)
		return -1
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// deployment is one omsd + brokersim pair, as separate processes.
type deployment struct {
	t                        *testing.T
	omsURL, simURL           string
	trader, operator         string
	brokerKey, webhookSecret string
	omsEnv                   map[string]string
	omsBin                   string
	dbURL                    string
	sim                      *proc
	client                   *http.Client
}

func deploy(t *testing.T) *deployment {
	t.Helper()
	omsBin, simBin := binaries(t)
	omsAddr, simAddr := freeAddr(t), freeAddr(t)
	d := &deployment{
		t: t, omsBin: omsBin, omsURL: "http://" + omsAddr, simURL: "http://" + simAddr,
		trader: harness.Secret(), operator: harness.Secret(), brokerKey: harness.Secret(), webhookSecret: harness.Secret(),
		dbURL: harness.NewDatabase(t), client: &http.Client{Timeout: 10 * time.Second},
	}

	dir := t.TempDir()
	keys, _ := json.Marshal([]map[string]any{
		{"principal": "alice", "role": "trader", "accounts": []string{"ACC-1"}, "token": d.trader},
		{"principal": "olivia", "role": "operator", "accounts": []string{"*"}, "token": d.operator},
	})
	keysFile := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(keysFile, keys, 0o600); err != nil {
		t.Fatal(err)
	}
	limits, err := filepath.Abs("../../examples/risk-limits.json")
	if err != nil {
		t.Fatal(err)
	}

	d.sim = start(t, simBin, map[string]string{
		"BROKERSIM_HTTP_ADDR": simAddr, "BROKERSIM_API_KEY": d.brokerKey,
		"BROKERSIM_WEBHOOK_URL": d.omsURL + "/v1/broker/webhooks", "BROKERSIM_WEBHOOK_SECRET": d.webhookSecret,
		"BROKERSIM_ACK_DELAY": "5ms", "BROKERSIM_FILL_DELAY": "20ms",
	})
	d.omsEnv = map[string]string{
		"OMS_DATABASE_URL": d.dbURL, "OMS_HTTP_ADDR": omsAddr, "OMS_API_KEYS_FILE": keysFile,
		"OMS_RISK_LIMITS_FILE": limits, "OMS_BROKER_URL": d.simURL, "OMS_BROKER_API_KEY": d.brokerKey,
		"OMS_BROKER_WEBHOOK_SECRET": d.webhookSecret, "OMS_BROKER_TIMEOUT": "2s",
		"OMS_RECONCILE_INTERVAL": "0", "OMS_RECONCILE_GRACE": "1s", "OMS_LOG_LEVEL": "debug",
	}
	return d
}

func (d *deployment) startOMS() *proc {
	d.t.Helper()
	p := start(d.t, d.omsBin, d.omsEnv)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := d.client.Get(d.omsURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case <-p.done:
			d.t.Fatalf("omsd exited during startup:\n%s", p.out)
		default:
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("omsd did not become ready:\n%s", p.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *deployment) do(base, token, method, path string, body any, headers ...string) (int, []byte) {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (d *deployment) placeOrders(prefix string, n int) []string {
	d.t.Helper()
	ids := make([]string, n)
	for i := range ids {
		status, body := d.do(d.omsURL, d.trader, "POST", "/v1/orders", harness.OrderReq(), "Idempotency-Key", fmt.Sprintf("%s-%d", prefix, i))
		var o struct{ ID string }
		json.Unmarshal(body, &o)
		if status != http.StatusCreated || o.ID == "" {
			d.t.Fatalf("create order: %d %s", status, body)
		}
		ids[i] = o.ID
	}
	return ids
}

func (d *deployment) states(ids []string) map[string]int {
	d.t.Helper()
	counts := map[string]int{}
	for _, id := range ids {
		status, body := d.do(d.omsURL, d.trader, "GET", "/v1/orders/"+id, nil)
		var o struct {
			State     string
			FilledQty int64 `json:"filled_qty"`
		}
		json.Unmarshal(body, &o)
		if status != http.StatusOK {
			d.t.Fatalf("get order: %d %s", status, body)
		}
		if o.State == "FILLED" && o.FilledQty != 100 {
			d.t.Fatalf("order %s is FILLED with %d of 100", id, o.FilledQty)
		}
		counts[o.State]++
	}
	return counts
}

func (d *deployment) waitFilled(ids []string, p *proc) {
	d.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		counts := d.states(ids)
		if counts["FILLED"] == len(ids) {
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("orders did not all fill: %v\nomsd output:\n%s", counts, p.out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *deployment) setFaults(f brokersim.Faults) {
	d.t.Helper()
	if status, body := d.do(d.simURL, d.brokerKey, "PUT", "/sim/faults", f); status != http.StatusOK {
		d.t.Fatalf("set faults: %d %s", status, body)
	}
}

// TestProcesses_KillAndRestartDrainsTheOutbox is the definition of done run
// against the real binaries: omsd is killed with SIGKILL while submissions
// are failing and in flight, and a fresh process finishes the work without
// creating a single duplicate order at the broker.
func TestProcesses_KillAndRestartDrainsTheOutbox(t *testing.T) {
	d := deploy(t)
	oms := d.startOMS()

	// Normal operation.
	first := d.placeOrders("first", 20)
	d.waitFilled(first, oms)

	// Now everything goes wrong at once. The broker accepts the next 12
	// submissions but the responses are lost; it then stops answering; and
	// none of its reports reach the OMS. The broker has orders the OMS
	// believes were never delivered, and fills the OMS has never heard of.
	d.setFaults(brokersim.Faults{LoseResponses: 12, DropWebhooks: true})
	second := d.placeOrders("second", 25)
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, body := d.do(d.simURL, d.brokerKey, "GET", "/sim/faults", nil)
		var f brokersim.Faults
		json.Unmarshal(body, &f)
		if f.LoseResponses == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker never saw the submissions: %s\n%s", body, oms.out)
		}
		time.Sleep(2 * time.Millisecond)
	}
	d.setFaults(brokersim.Faults{FailRequests: 1_000_000, DropWebhooks: true})
	time.Sleep(300 * time.Millisecond) // retries are now failing

	// And the OMS process is killed outright.
	if err := oms.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	oms.wait(t, 10*time.Second)

	// The broker recovers and a new OMS process starts. Orders accepted
	// before the crash are durable, and so is the work still owed on them:
	// the outbox drains. (A message whose worker died mid-call is retried
	// once its 30s lease expires, so allow for that.)
	d.setFaults(brokersim.Faults{})
	oms = d.startOMS()
	deadline = time.Now().Add(60 * time.Second)
	for {
		resp, err := d.client.Get(d.omsURL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		metrics, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(metrics), "\noms_outbox_pending 0\n") {
			if !strings.Contains(string(metrics), "\noms_outbox_dead 0\n") {
				t.Fatalf("messages were dead-lettered:\n%s", oms.out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox did not drain after the restart:\n%s", oms.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Every order has now been sent. The ones the broker accepted before the
	// crash are still shown as sent and unacknowledged, because the broker's
	// reports about them were lost. (An order whose first attempt had not
	// got through before the crash is submitted fresh and fills normally.)
	got := d.states(second)
	if got["SUBMITTING"] < 12 || got["SUBMITTING"]+got["ACKNOWLEDGED"]+got["PARTIALLY_FILLED"]+got["FILLED"] != len(second) {
		t.Fatalf("after the restart: %v; want every order sent, at least 12 still SUBMITTING", got)
	}

	// Reconciliation finds the difference and repairs it from the broker's
	// books.
	time.Sleep(1500 * time.Millisecond) // past the 1s grace window
	status, body := d.do(d.omsURL, d.operator, "POST", "/v1/reconciliation/runs", nil)
	var run store.Run
	json.Unmarshal(body, &run)
	if status != http.StatusCreated || run.DriftFound == 0 || run.AutoRepaired != run.DriftFound || run.IssuesOpened != 0 {
		t.Fatalf("reconciliation: %d %s", status, body)
	}
	t.Logf("reconciliation after the restart: drift %d, repaired %d", run.DriftFound, run.AutoRepaired)
	d.waitFilled(second, oms)

	// Exactly one broker order per OMS order, each filled exactly once.
	_, body = d.do(d.simURL, d.brokerKey, "GET", "/v1/snapshot", nil)
	var snap broker.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, o := range snap.Orders {
		seen[o.ClientOrderID]++
		if o.CumQty != 100 {
			t.Errorf("broker order %s filled %d", o.ClientOrderID, o.CumQty)
		}
	}
	if len(snap.Orders) != 45 || len(seen) != 45 {
		t.Errorf("broker has %d orders (%d distinct), want 45", len(snap.Orders), len(seen))
	}
	_, body = d.do(d.simURL, d.brokerKey, "GET", "/sim/stats", nil)
	var stats brokersim.Stats
	json.Unmarshal(body, &stats)
	if stats.DuplicateSubmits == 0 {
		t.Errorf("expected resubmissions the broker had to deduplicate; stats %+v", stats)
	}
	t.Logf("broker stats: %+v", stats)

	// A second run agrees with the broker, and the position is exact:
	// 45 orders x 100 shares x 150.00.
	status, body = d.do(d.omsURL, d.operator, "POST", "/v1/reconciliation/runs", nil)
	run = store.Run{}
	json.Unmarshal(body, &run)
	if status != http.StatusCreated || run.DriftFound != 0 || run.OrdersChecked != 45 {
		t.Errorf("reconciliation: %d %s", status, body)
	}
	_, body = d.do(d.omsURL, d.trader, "GET", "/v1/positions?account_id=ACC-1", nil)
	if !strings.Contains(string(body), `"net_qty":4500,"net_cash":"-675000.00"`) {
		t.Errorf("position: %s", body)
	}

	// SIGTERM is a clean shutdown.
	if err := oms.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := oms.wait(t, 30*time.Second); code != 0 {
		t.Errorf("omsd exited %d after SIGTERM:\n%s", code, oms.out)
	}
	if !strings.Contains(oms.out.String(), `"msg":"omsd stopped"`) {
		t.Errorf("no clean shutdown message:\n%s", oms.out)
	}

	// `omsd verify` replays the event log against the projections.
	verify := start(t, d.omsBin, map[string]string{"OMS_DATABASE_URL": d.dbURL}, "verify")
	if code := verify.wait(t, 30*time.Second); code != 0 {
		t.Errorf("omsd verify exited %d:\n%s", code, verify.out)
	}
	var rep store.VerifyReport
	if err := json.Unmarshal([]byte(verify.out.String()), &rep); err != nil || rep.Orders != 45 || !rep.OK() {
		t.Errorf("verify report: %s (%v)", verify.out, err)
	}

	// Nothing secret was logged, at debug level, by either process.
	logs := oms.out.String() + d.sim.out.String()
	for name, secret := range map[string]string{
		"trader token": d.trader, "operator token": d.operator, "broker key": d.brokerKey,
		"webhook secret": d.webhookSecret, "database URL": d.dbURL,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the %s appears in the logs", name)
		}
	}
	if !strings.Contains(oms.out.String(), `"mode":"PAPER"`) || !strings.Contains(d.sim.out.String(), `"mode":"SIMULATED"`) {
		t.Error("the processes do not announce that they are paper / simulated")
	}
}

func TestProcesses_OmsdFailsClosedOnBadConfiguration(t *testing.T) {
	d := deploy(t)

	run := func(env map[string]string, args ...string) (int, string) {
		p := start(t, d.omsBin, env, args...)
		return p.wait(t, 30*time.Second), p.out.String()
	}
	with := func(changes map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range d.omsEnv {
			env[k] = v
		}
		for k, v := range changes {
			if v == "" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
		return env
	}

	// No configuration at all: every missing variable is named, nothing starts.
	code, out := run(nil)
	for _, name := range []string{"OMS_DATABASE_URL", "OMS_RISK_LIMITS_FILE", "OMS_BROKER_URL", "OMS_BROKER_API_KEY", "OMS_BROKER_WEBHOOK_SECRET"} {
		if !strings.Contains(out, name+" is required") {
			t.Errorf("missing configuration: %s not reported:\n%s", name, out)
		}
	}
	if code == 0 {
		t.Error("omsd started with no configuration")
	}

	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"no API keys":          {with(map[string]string{"OMS_API_KEYS_FILE": ""}), "OMS_API_KEYS"},
		"weak webhook secret":  {with(map[string]string{"OMS_BROKER_WEBHOOK_SECRET": "short"}), "at least 24 characters"},
		"missing risk limits":  {with(map[string]string{"OMS_RISK_LIMITS_FILE": filepath.Join(t.TempDir(), "none.json")}), "risk: read limits"},
		"unknown role":         {with(map[string]string{"OMS_ROLES": "api,admin"}), `unknown role "admin"`},
		"broker unreachable":   {with(map[string]string{"OMS_BROKER_URL": "http://" + freeAddr(t), "OMS_BROKER_WAIT": "1s"}), "could not confirm that the broker"},
		"unreachable database": {with(map[string]string{"OMS_DATABASE_URL": "postgres://oms:oms@" + freeAddr(t) + "/oms?sslmode=disable&connect_timeout=2"}), "open database"},
		"unknown subcommand":   {d.omsEnv, "unknown command"},
	}
	for name, c := range cases {
		var args []string
		if name == "unknown subcommand" {
			args = []string{"trade-for-real"}
		}
		code, out := run(c.env, args...)
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, output:\n%s\nwant it to mention %q", name, code, out, c.want)
		}
		for _, secret := range []string{d.brokerKey, d.webhookSecret, d.trader} {
			if strings.Contains(out, secret) {
				t.Errorf("%s: a secret appears in the error output", name)
			}
		}
	}

	// The paper-trading guard: something that answers at the broker URL but
	// does not say it is a simulator is refused.
	notASim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer notASim.Close()
	code, out = run(with(map[string]string{"OMS_BROKER_URL": notASim.URL}))
	if code == 0 || !strings.Contains(out, "paper trading only") {
		t.Errorf("omsd accepted a broker that is not a simulator: exit %d\n%s", code, out)
	}

	// brokersim also refuses to start without its secrets.
	_, simBin := binaries(t)
	p := start(t, simBin, nil)
	if code := p.wait(t, 10*time.Second); code == 0 || !strings.Contains(p.out.String(), "BROKERSIM_WEBHOOK_SECRET is required") {
		t.Errorf("brokersim with no configuration: exit %d\n%s", code, p.out)
	}
}

func TestProcesses_WorkerOnlyProcessServesNoAPI(t *testing.T) {
	d := deploy(t)
	d.omsEnv["OMS_ROLES"] = "outbox"
	d.startOMS()

	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 200, "/v1/orders": 404, "/v1/broker/webhooks": 404} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", d.omsURL+path, nil)
		req.Header.Set("Authorization", "Bearer "+d.trader)
		resp, err := d.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}
