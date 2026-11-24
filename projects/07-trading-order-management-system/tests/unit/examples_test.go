package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"oms/src/auth"
	"oms/src/broker"
	"oms/src/domain"
	"oms/src/risk"
	"oms/src/service"
)

const root = "../.."

func readExample(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func strictDecode(t *testing.T, path string, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(readExample(t, path)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The files under examples/ are documentation that people copy. They must
// keep working, and the credential template must never be usable as is.
func TestExamples_AreValid(t *testing.T) {
	if _, err := risk.LoadStaticLimits(filepath.Join(root, "examples/risk-limits.json")); err != nil {
		t.Errorf("risk limits: %v", err)
	}

	var create service.CreateOrderRequest
	strictDecode(t, "examples/requests/create-order.json", &create)
	if errs := domain.ValidateNewOrder(domain.OrderCreated{
		AccountID: create.AccountID, ClientOrderID: create.ClientOrderID, Symbol: create.Symbol, Side: create.Side,
		Type: create.Type, TimeInForce: create.TimeInForce, Qty: create.Qty, LimitPrice: create.LimitPrice,
	}); len(errs) != 0 {
		t.Errorf("create-order.json is not a valid order: %+v", errs)
	}
	var replace service.ReplaceOrderRequest
	strictDecode(t, "examples/requests/replace-order.json", &replace)
	if replace.Qty == nil || replace.LimitPrice == nil {
		t.Errorf("replace-order.json: %+v", replace)
	}
	var resolve struct{ Action, Note string }
	strictDecode(t, "examples/requests/resolve-issue.json", &resolve)
	if resolve.Action != service.ActionAcknowledge || resolve.Note == "" {
		t.Errorf("resolve-issue.json: %+v", resolve)
	}

	for file, kind := range map[string]domain.ReportKind{
		"examples/webhooks/fill.json":           domain.ReportFill,
		"examples/webhooks/fill-corrected.json": domain.ReportCorrect,
	} {
		r, err := broker.NormalizeWebhook(readExample(t, file))
		if err != nil || r.Kind != kind {
			t.Errorf("%s: kind %q, err %v", file, r.Kind, err)
		}
	}
}

func TestExamples_CredentialTemplatesAreNotUsable(t *testing.T) {
	// The shape is right, but the placeholder tokens are too short to load.
	data := readExample(t, "examples/api-keys.example.json")
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("api-keys.example.json: %v", err)
	}
	if _, err := auth.ParseKeys(data); err == nil {
		t.Error("api-keys.example.json loads as real credentials; its placeholder tokens must be rejected")
	}

	// .env.example carries no values for any secret.
	secretName := regexp.MustCompile(`(PASSWORD|SECRET|TOKEN|KEY|KEYS)$`)
	for _, line := range strings.Split(string(readExample(t, ".env.example")), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if secretName.MatchString(name) && value != "" {
			t.Errorf(".env.example sets %s; secret values must be left empty", name)
		}
	}
}

// credentialFindings returns what in a line of text looks like a committed
// credential: a long random-looking value assigned to something named like a
// secret, a private key, a cloud access key, or a database URL with a
// password other than the fixed oms:oms login of the throwaway local server.
func credentialFindings(line string) []string {
	var found []string
	for what, re := range credentialPatterns {
		if re.MatchString(line) {
			found = append(found, what)
		}
	}
	for _, m := range databaseURL.FindAllStringSubmatch(line, -1) {
		if m[2] != "oms" {
			found = append(found, "database URL with an inline password")
		}
	}
	return found
}

var (
	credentialPatterns = map[string]*regexp.Regexp{
		"private key":            regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		"secret-looking literal": regexp.MustCompile(`(?i)(token|secret|password|api_?key)["']?\s*[:=]\s*["']?[A-Za-z0-9+/_-]{32,}`),
		"cloud access key":       regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	}
	databaseURL = regexp.MustCompile(`postgres(ql)?://[^:/\s"']+:([^@\s"'$%]+)@`)
)

func TestCredentialScan_RecognisesWhatItShould(t *testing.T) {
	filler := strings.Repeat("0a1b2c3d", 5) // 40 characters, built here so this file holds no literal
	flagged := []string{
		`api_key = "` + filler + `"`,
		`OMS_BROKER_WEBHOOK_SECRET=` + filler,
		`"token": "` + filler + `"`,
		`password: ` + filler,
		"-----BEGIN " + "RSA PRIVATE KEY-----",
		"aws = AKIA" + strings.Repeat("Z", 16),
		"postgres://app:" + "hunter2" + "@db.internal:5432/oms",
	}
	for _, line := range flagged {
		if len(credentialFindings(line)) == 0 {
			t.Errorf("not flagged: %s", line)
		}
	}
	clean := []string{
		`OMS_BROKER_API_KEY: ${OMS_BROKER_API_KEY:?run make env first}`,
		`POSTGRES_PASSWORD=`,
		`"token": "CHANGE_ME"`,
		`req.Header.Set("Authorization", "Bearer "+token)`,
		`postgres://oms:oms@127.0.0.1:5432/oms?sslmode=disable`,
		`postgres://oms:${POSTGRES_PASSWORD}@postgres:5432/oms`,
		`url="postgres://postgres:$pw@127.0.0.1:5432/postgres?sslmode=disable"`,
		`fmt.Sprintf("postgres://oms:oms@127.0.0.1:%d/oms?sslmode=disable", port)`,
	}
	for _, line := range clean {
		if found := credentialFindings(line); len(found) != 0 {
			t.Errorf("false positive %v: %s", found, line)
		}
	}
}

// No credential may be committed: every text file in the project is scanned.
func TestRepository_ContainsNoCredentials(t *testing.T) {
	skipDirs := map[string]bool{".git": true, ".cache": true, ".local": true, "bin": true}
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// .env is git-ignored by design and holds generated local secrets;
		// go.sum is hashes.
		if d.Name() == ".env" || d.Name() == "go.sum" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil // binary
		}
		files++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			for _, what := range credentialFindings(line) {
				t.Errorf("%s:%d looks like it contains a %s", rel, i+1, what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 50 {
		t.Errorf("only %d files scanned; is the project root wrong?", files)
	}
}
