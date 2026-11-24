package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mdp/src/gateway"
)

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func request(header string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://gateway/v1/trades?token=secret-a", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestEntitlements(t *testing.T) {
	file := fmt.Sprintf(`{"clients":[
		{"name":"desk-a","token_sha256":%q,"venues":["venueA"]},
		{"name":"ops","token_sha256":%q,"venues":["*"]}]}`, digest("secret-a"), digest("secret-ops"))
	path := filepath.Join(t.TempDir(), "entitlements.json")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	ent, err := gateway.LoadEntitlements(path)
	if err != nil {
		t.Fatal(err)
	}
	if ent.Open() {
		t.Error("a loaded file must not be open access")
	}
	if strings.Contains(file, "secret-a") {
		t.Fatal("test setup: the file must hold digests only")
	}

	p, ok := ent.Authenticate(request("Bearer secret-a"))
	if !ok || p.Name != "desk-a" || !p.Allows("venueA") || p.Allows("venueB") {
		t.Errorf("desk-a: ok=%v %+v", ok, p)
	}
	p, ok = ent.Authenticate(request("Bearer secret-ops"))
	if !ok || p.Name != "ops" || !p.Allows("venueA") || !p.Allows("venueB") || !p.Allows("anything") {
		t.Errorf("ops: ok=%v %+v", ok, p)
	}
	for _, header := range []string{
		"", // no header; the token in the URL is ignored
		"Bearer wrong",
		"Bearer ",
		"Bearer",
		"bearer secret-a", // scheme is case-sensitive here
		"Basic secret-a",
		"secret-a",
		"Bearer " + digest("secret-a"), // the stored digest is not a credential
		"Bearer secret-a ",
	} {
		if p, ok := ent.Authenticate(request(header)); ok {
			t.Errorf("header %q authenticated as %+v", header, p)
		}
	}
}

func TestOpenEntitlementsAllowEverything(t *testing.T) {
	ent := gateway.OpenEntitlements()
	p, ok := ent.Authenticate(request(""))
	if !ok || !ent.Open() || !p.Allows("venueA") || !p.Allows("venueB") {
		t.Errorf("open access: ok=%v %+v", ok, p)
	}
}

func TestEntitlementsFileIsValidated(t *testing.T) {
	good := digest("x")
	cases := map[string]string{
		"not JSON":        `{"clients":`,
		"plaintext token": `{"clients":[{"name":"a","token_sha256":"secret-a","venues":["venueA"]}]}`,
		"short digest":    `{"clients":[{"name":"a","token_sha256":"abcd","venues":["venueA"]}]}`,
		"no venues":       `{"clients":[{"name":"a","token_sha256":"` + good + `","venues":[]}]}`,
		"no name":         `{"clients":[{"token_sha256":"` + good + `","venues":["venueA"]}]}`,
	}
	for name, body := range cases {
		if _, err := gateway.ParseEntitlements([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := gateway.LoadEntitlements(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file must be an error, not open access")
	}
	// No clients at all is valid and authenticates nobody.
	ent, err := gateway.ParseEntitlements([]byte(`{"clients":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ent.Authenticate(request("Bearer anything")); ok || ent.Open() {
		t.Error("an empty client list must authenticate nobody")
	}
}

// The committed template must load, and must not be usable as it stands.
func TestExampleEntitlementsFileGrantsNothing(t *testing.T) {
	ent, err := gateway.LoadEntitlements(filepath.Join("..", "..", "examples", "entitlements.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ent.Open() {
		t.Error("a configured entitlements file must not mean open access")
	}
	for _, token := range []string{"", "secret-a", strings.Repeat("0", 64)} {
		if _, ok := ent.Authenticate(request("Bearer " + token)); ok {
			t.Errorf("the example file authenticated token %q", token)
		}
	}
}
