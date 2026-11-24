package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"surveillance/src/domain"
)

// Book holds every alert and applies suppression.
//
// Suppression policy: a detection is folded into an existing alert, rather
// than raising a new one, when all of these hold:
//
//   - same dedup key (rule + account(s) + symbol, plus side for layering);
//   - that alert is still OPEN, i.e. no analyst has picked it up;
//   - the detection's evidence ends within the cooldown (event time) of
//     the end of the alert's evidence.
//
// Folding keeps the evidence: event refs are unioned, the window is
// widened, the occurrence count goes up and the severity can only rise.
// Once an alert is in a case, further activity raises a fresh alert, so
// an analyst's snapshot is never quietly extended behind their back.
type Book struct {
	cooldown int64
	maxRefs  int
	alerts   map[string]*domain.Alert
	order    []string
	latest   map[string]string // dedup key -> most recent alert ID
}

func NewBook(s Suppression) *Book {
	b := &Book{alerts: map[string]*domain.Alert{}, latest: map[string]string{}}
	b.SetConfig(s)
	return b
}

func (b *Book) SetConfig(s Suppression) {
	b.cooldown = int64(s.CooldownS) * int64(time.Second)
	b.maxRefs = s.MaxEvidenceRefs
}

// Meta identifies the ruleset a detection was made under.
type Meta struct {
	RulesetVersion string
	RulesetHash    string
}

// Apply turns a detection into a new alert or folds it into an open one.
// It returns the alert as it now stands and whether it was just created.
func (b *Book) Apply(d Detection, m Meta, now time.Time) (domain.Alert, bool) {
	if id, ok := b.latest[d.DedupKey]; ok {
		a := b.alerts[id]
		gap := d.WindowEnd.UnixNano() - a.WindowEnd.UnixNano()
		if a.Status == domain.AlertOpen && gap <= b.cooldown {
			b.merge(a, d, now)
			return clone(a), false
		}
	}
	a := &domain.Alert{
		ID:     b.newID(d),
		RuleID: d.RuleID, RuleVersion: d.RuleVersion,
		RulesetVersion: m.RulesetVersion, RulesetHash: m.RulesetHash,
		Severity: d.Severity, Status: domain.AlertOpen,
		Symbol: d.Symbol, Accounts: append([]string(nil), d.Accounts...), DedupKey: d.DedupKey,
		WindowStart: d.WindowStart, WindowEnd: d.WindowEnd,
		Thresholds: d.Thresholds, Observed: d.Observed,
		EvidenceTruncated: d.Truncated,
		Explanation:       d.Explanation, Disclaimer: domain.Disclaimer,
		Occurrences:    1,
		TriggerEventID: d.TriggerEventID, TriggerEventTime: d.TriggerEventTime, TriggerReceivedAt: d.TriggerReceivedAt,
		FirstDetectedAt: now, LastDetectedAt: now,
	}
	a.EventRefs, a.EvidenceTruncated = unionRefs(nil, d.EventRefs, b.maxRefs, d.Truncated)
	b.put(a)
	return clone(a), true
}

func (b *Book) merge(a *domain.Alert, d Detection, now time.Time) {
	a.Occurrences++
	a.LastDetectedAt = now
	if d.WindowStart.Before(a.WindowStart) {
		a.WindowStart = d.WindowStart
	}
	if d.WindowEnd.After(a.WindowEnd) {
		a.WindowEnd = d.WindowEnd
	}
	a.EventRefs, a.EvidenceTruncated = unionRefs(a.EventRefs, d.EventRefs, b.maxRefs, a.EvidenceTruncated || d.Truncated)
	// Keep the figures of the strongest occurrence; on a tie, the latest.
	if domain.SeverityRank(d.Severity) >= domain.SeverityRank(a.Severity) {
		a.Severity = d.Severity
		a.Observed = d.Observed
		a.Thresholds = d.Thresholds
		a.Explanation = d.Explanation
		a.RuleVersion = d.RuleVersion
	}
	for _, acct := range d.Accounts {
		if !contains(a.Accounts, acct) {
			a.Accounts = append(a.Accounts, acct)
		}
	}
}

func unionRefs(have, add []string, max int, truncated bool) ([]string, bool) {
	seen := make(map[string]bool, len(have)+len(add))
	out := make([]string, 0, len(have)+len(add))
	for _, lists := range [][]string{have, add} {
		for _, r := range lists {
			if seen[r] {
				continue
			}
			seen[r] = true
			if len(out) >= max {
				truncated = true
				continue
			}
			out = append(out, r)
		}
	}
	return out, truncated
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// newID derives the ID from what was detected, so replaying the same
// input yields the same alert IDs.
func (b *Book) newID(d Detection) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		d.RuleID, fmt.Sprint(d.RuleVersion), d.DedupKey, d.TriggerEventID, fmt.Sprint(d.WindowEnd.UnixNano()),
	}, "\x00")))
	base := "alr-" + hex.EncodeToString(h[:6])
	id := base
	for n := 2; ; n++ {
		if _, taken := b.alerts[id]; !taken {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

func (b *Book) put(a *domain.Alert) {
	if _, exists := b.alerts[a.ID]; !exists {
		b.order = append(b.order, a.ID)
	}
	b.alerts[a.ID] = a
	b.latest[a.DedupKey] = a.ID
}

// Restore loads an alert exactly as it was journalled. Its case status is
// not stored with it; it is re-derived from the case log.
func (b *Book) Restore(a domain.Alert) (created bool) {
	_, exists := b.alerts[a.ID]
	a.Status = domain.AlertOpen
	a.CaseIDs = nil
	c := a
	b.put(&c)
	return !exists
}

// SetCase records which cases an alert belongs to and the status that follows.
func (b *Book) SetCase(id, status string, caseIDs []string) {
	if a, ok := b.alerts[id]; ok {
		a.Status = status
		a.CaseIDs = caseIDs
	}
}

func (b *Book) Get(id string) (domain.Alert, bool) {
	a, ok := b.alerts[id]
	if !ok {
		return domain.Alert{}, false
	}
	return clone(a), true
}

// All returns every alert, oldest first.
func (b *Book) All() []domain.Alert {
	out := make([]domain.Alert, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, clone(b.alerts[id]))
	}
	return out
}

func (b *Book) Len() int { return len(b.order) }

func clone(a *domain.Alert) domain.Alert {
	c := *a
	c.Accounts = append([]string(nil), a.Accounts...)
	c.EventRefs = append([]string(nil), a.EventRefs...)
	c.CaseIDs = append([]string(nil), a.CaseIDs...)
	return c
}
