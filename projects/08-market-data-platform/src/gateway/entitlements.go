package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Entitlements decides which venues a caller may read.
//
// Clients authenticate with "Authorization: Bearer <token>". The
// entitlements file stores only SHA-256 digests of tokens, so the file does
// not contain usable credentials:
//
//	{"clients":[{"name":"desk-a","token_sha256":"<hex>","venues":["venueA"]},
//	            {"name":"ops","token_sha256":"<hex>","venues":["*"]}]}
//
// Tokens are only accepted in the header, never in the URL, so they do not
// end up in access logs or browser history.
type Entitlements struct {
	open    bool
	clients []entitledClient
}

type entitledClient struct {
	Name        string   `json:"name"`
	TokenSHA256 string   `json:"token_sha256"`
	Venues      []string `json:"venues"`
	digest      []byte
}

// Principal is an authenticated caller.
type Principal struct {
	Name   string
	all    bool
	venues map[string]bool
}

// Allows reports whether the caller may read a venue's data.
func (p *Principal) Allows(venue string) bool { return p.all || p.venues[venue] }

// OpenEntitlements allows every caller to read every venue. It is the
// default for local, simulated use when no entitlements file is configured.
func OpenEntitlements() *Entitlements { return &Entitlements{open: true} }

// LoadEntitlements reads an entitlements file.
func LoadEntitlements(path string) (*Entitlements, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("entitlements: %w", err)
	}
	return ParseEntitlements(b)
}

func ParseEntitlements(b []byte) (*Entitlements, error) {
	var f struct {
		Clients []entitledClient `json:"clients"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("entitlements: %w", err)
	}
	for i := range f.Clients {
		c := &f.Clients[i]
		d, err := hex.DecodeString(c.TokenSHA256)
		if err != nil || len(d) != sha256.Size {
			return nil, fmt.Errorf("entitlements: client %q: token_sha256 must be 64 hex characters", c.Name)
		}
		if c.Name == "" || len(c.Venues) == 0 {
			return nil, fmt.Errorf("entitlements: every client needs a name and at least one venue")
		}
		c.digest = d
	}
	return &Entitlements{clients: f.Clients}, nil
}

// Open reports whether authentication is disabled.
func (e *Entitlements) Open() bool { return e.open }

// Authenticate resolves the caller, or returns false if the request carries
// no valid token.
func (e *Entitlements) Authenticate(r *http.Request) (*Principal, bool) {
	if e.open {
		return &Principal{Name: "anonymous", all: true}, true
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	var found *entitledClient
	// Compare against every client so timing does not reveal which matched.
	for i := range e.clients {
		if subtle.ConstantTimeCompare(sum[:], e.clients[i].digest) == 1 {
			found = &e.clients[i]
		}
	}
	if found == nil {
		return nil, false
	}
	p := &Principal{Name: found.Name, venues: map[string]bool{}}
	for _, v := range found.Venues {
		if v == "*" {
			p.all = true
		}
		p.venues[v] = true
	}
	return p, true
}
