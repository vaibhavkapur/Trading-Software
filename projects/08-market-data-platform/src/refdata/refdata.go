// Package refdata is the instrument and provider reference data every
// service loads from the same file.
package refdata

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"mdp/src/domain"
	"mdp/src/fixed"
)

// Provider describes how one feed maps onto the canonical model.
type Provider struct {
	Provider string `json:"provider"`
	Venue    string `json:"venue"`
	// SeqScope says what one sequence domain covers: "symbol" (a gap affects
	// one instrument) or "feed" (a gap may affect any instrument of the venue).
	SeqScope string `json:"seq_scope"`
	// TradeSide says what the provider's trade side means: "aggressor" or
	// "resting". Canonical trades always carry the aggressor.
	TradeSide string `json:"trade_side"`
	// SeqStart is the first sequence number of a new session, if the
	// provider guarantees one. When a session starts at a higher number the
	// events before it were missed and are declared as a gap. 0 means
	// unknown: the start of a session cannot be checked.
	SeqStart uint64 `json:"seq_start,omitempty"`
}

// File is the on-disk layout of the reference data.
type File struct {
	Providers   []Provider          `json:"providers"`
	Instruments []domain.Instrument `json:"instruments"`
}

// Instrument is an instrument with its parsed units.
type Instrument struct {
	domain.Instrument
	Tick fixed.Unit
	Lot  fixed.Unit
}

// Registry is immutable after construction and safe for concurrent use.
type Registry struct {
	providers map[string]Provider
	byID      map[string]*Instrument
	byNative  map[string]*Instrument // venue + "\x00" + native symbol
	list      []domain.Instrument    // sorted by ID
}

func Load(path string) (*Registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("refdata: %w", err)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("refdata: %s: %w", path, err)
	}
	return New(f)
}

func New(f File) (*Registry, error) {
	r := &Registry{providers: map[string]Provider{}, byID: map[string]*Instrument{}, byNative: map[string]*Instrument{}}
	venues := map[string]bool{}
	for _, p := range f.Providers {
		if p.Provider == "" || p.Venue == "" {
			return nil, fmt.Errorf("refdata: provider entry needs provider and venue")
		}
		if p.SeqScope != "symbol" && p.SeqScope != "feed" {
			return nil, fmt.Errorf("refdata: provider %s: seq_scope must be symbol or feed", p.Provider)
		}
		if p.TradeSide != "aggressor" && p.TradeSide != "resting" {
			return nil, fmt.Errorf("refdata: provider %s: trade_side must be aggressor or resting", p.Provider)
		}
		if _, dup := r.providers[p.Provider]; dup {
			return nil, fmt.Errorf("refdata: duplicate provider %s", p.Provider)
		}
		r.providers[p.Provider] = p
		venues[p.Venue] = true
	}
	for _, in := range f.Instruments {
		if in.ID != in.Venue+":"+in.NativeSymbol {
			return nil, fmt.Errorf("refdata: instrument_id %q must be venue:native_symbol", in.ID)
		}
		if !venues[in.Venue] {
			return nil, fmt.Errorf("refdata: instrument %s: no provider serves venue %s", in.ID, in.Venue)
		}
		if _, dup := r.byID[in.ID]; dup {
			return nil, fmt.Errorf("refdata: duplicate instrument %s", in.ID)
		}
		tick, err := fixed.ParseUnit(in.TickSize)
		if err != nil {
			return nil, fmt.Errorf("refdata: instrument %s tick_size: %w", in.ID, err)
		}
		lot, err := fixed.ParseUnit(in.LotSize)
		if err != nil {
			return nil, fmt.Errorf("refdata: instrument %s lot_size: %w", in.ID, err)
		}
		inst := &Instrument{Instrument: in, Tick: tick, Lot: lot}
		r.byID[in.ID] = inst
		r.byNative[in.Venue+"\x00"+in.NativeSymbol] = inst
		r.list = append(r.list, in)
	}
	sort.Slice(r.list, func(i, j int) bool { return r.list[i].ID < r.list[j].ID })
	return r, nil
}

func (r *Registry) Provider(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Lookup resolves a provider's native symbol.
func (r *Registry) Lookup(venue, nativeSymbol string) (*Instrument, bool) {
	in, ok := r.byNative[venue+"\x00"+nativeSymbol]
	return in, ok
}

func (r *Registry) Get(id string) (*Instrument, bool) {
	in, ok := r.byID[id]
	return in, ok
}

// All returns every instrument sorted by ID.
func (r *Registry) All() []domain.Instrument {
	return append([]domain.Instrument(nil), r.list...)
}

// Search returns instruments whose ID contains q, ignoring case. An empty q
// matches everything.
func (r *Registry) Search(q string) []domain.Instrument {
	q = strings.ToLower(q)
	out := []domain.Instrument{}
	for _, in := range r.list {
		if q == "" || strings.Contains(strings.ToLower(in.ID), q) {
			out = append(out, in)
		}
	}
	return out
}

// Symbols returns the native symbols a provider's venue lists, sorted.
func (r *Registry) Symbols(provider string) []string {
	p, ok := r.providers[provider]
	if !ok {
		return nil
	}
	var out []string
	for _, in := range r.list {
		if in.Venue == p.Venue {
			out = append(out, in.NativeSymbol)
		}
	}
	return out
}

// InVenue returns the IDs of a venue's instruments, sorted.
func (r *Registry) InVenue(venue string) []string {
	var out []string
	for _, in := range r.list {
		if in.Venue == venue {
			out = append(out, in.ID)
		}
	}
	return out
}
