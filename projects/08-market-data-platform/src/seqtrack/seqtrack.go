// Package seqtrack classifies provider sequence numbers as in-order,
// duplicate, or following a gap. It is the single definition of those terms,
// shared by the ingestors (to decide when to resync) and the normalizer (to
// declare gaps and drop redelivered events).
package seqtrack

import "sort"

type Verdict int

const (
	OK        Verdict = iota // next expected number, or first sighting of the domain
	Duplicate                // already seen (seq <= last)
	Gap                      // one or more numbers were skipped
	Reset                    // the provider started a new session; numbering restarts
)

func (v Verdict) String() string {
	return [...]string{"ok", "duplicate", "gap", "reset"}[v]
}

// Result describes one observation. For Gap, [MissingFrom, MissingTo] is the
// inclusive range of numbers that did not arrive. First marks the first
// sighting of a domain: there is no earlier number to compare against, so
// whether anything was missed before it depends on where the provider starts
// counting, which only the caller knows.
type Result struct {
	Verdict     Verdict
	First       bool
	MissingFrom uint64
	MissingTo   uint64
}

// State is the tracked position of one sequence domain.
type State struct {
	Session string `json:"session"`
	Last    uint64 `json:"last"`
}

// Tracker follows any number of sequence domains. It is not safe for
// concurrent use.
type Tracker struct {
	domains map[string]State
}

func New() *Tracker { return &Tracker{domains: map[string]State{}} }

// Classify reports what seq would mean for the domain without recording it.
func (t *Tracker) Classify(domain, session string, seq uint64) Result {
	st, seen := t.domains[domain]
	switch {
	case !seen:
		return Result{Verdict: OK, First: true}
	case st.Session != session:
		return Result{Verdict: Reset}
	case seq <= st.Last:
		return Result{Verdict: Duplicate}
	case seq == st.Last+1:
		return Result{Verdict: OK}
	default:
		return Result{Verdict: Gap, MissingFrom: st.Last + 1, MissingTo: seq - 1}
	}
}

// Observe classifies seq and, unless it is a duplicate, records it as the
// domain's latest position.
func (t *Tracker) Observe(domain, session string, seq uint64) Result {
	r := t.Classify(domain, session, seq)
	if r.Verdict != Duplicate {
		t.domains[domain] = State{Session: session, Last: seq}
	}
	return r
}

// Get returns the tracked position of a domain.
func (t *Tracker) Get(domain string) (State, bool) {
	st, ok := t.domains[domain]
	return st, ok
}

// Set forces a domain's position, used when restoring from a checkpoint.
func (t *Tracker) Set(domain string, st State) { t.domains[domain] = st }

// Domains returns the tracked domain names in sorted order.
func (t *Tracker) Domains() []string {
	out := make([]string, 0, len(t.domains))
	for d := range t.domains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Snapshot copies the tracker state for checkpointing.
func (t *Tracker) Snapshot() map[string]State {
	out := make(map[string]State, len(t.domains))
	for d, st := range t.domains {
		out[d] = st
	}
	return out
}

// Restore replaces the tracker state with a snapshot.
func (t *Tracker) Restore(s map[string]State) {
	t.domains = make(map[string]State, len(s))
	for d, st := range s {
		t.domains[d] = st
	}
}
