// Package cases is the analyst workflow: triage, annotate, resolve, export.
//
// The case log is the only source of truth. Every change is one audit
// event appended to a hash-chained file, and a case is nothing more than
// the result of applying its events in order. There is no way to change a
// case without leaving an event, because there is no other write path.
//
//	OPEN --triage--> IN_REVIEW --escalate--> ESCALATED
//	                     |                       |
//	                     +-------resolve---------+--> RESOLVED --reopen--> IN_REVIEW
//
// A Service is not safe for concurrent use; the engine serialises access.
package cases

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"surveillance/src/domain"
)

const (
	ActOpened   = "CASE_OPENED"
	ActAttached = "ALERT_ATTACHED"
	ActTriaged  = "TRIAGED"
	ActNote     = "NOTE_ADDED"
	ActEscalate = "ESCALATED"
	ActResolved = "RESOLVED"
	ActReopened = "REOPENED"
	ActExported = "EVIDENCE_EXPORTED"
)

var (
	ErrNotFound    = errors.New("case not found")
	ErrInvalid     = errors.New("invalid request")
	ErrConflict    = errors.New("not allowed in the case's current state")
	ErrUnavailable = errors.New("case log unavailable")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

// Details is the payload of an audit event. Only the fields relevant to
// the action are set. For CASE_OPENED and ALERT_ATTACHED it carries the
// hash of the evidence snapshot, so the chain covers the evidence too.
type Details struct {
	Title        string `json:"title,omitempty"`
	AlertID      string `json:"alert_id,omitempty"`
	SnapshotHash string `json:"snapshot_hash,omitempty"`
	EventCount   int    `json:"event_count,omitempty"`
	Note         string `json:"note,omitempty"`
	Priority     string `json:"priority,omitempty"`
	Assignee     string `json:"assignee,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
	Summary      string `json:"summary,omitempty"`
	FromStatus   string `json:"from_status,omitempty"`
	ToStatus     string `json:"to_status,omitempty"`
	BundleHash   string `json:"bundle_hash,omitempty"`
}

// record is one line of the case log: the audit event, plus the snapshot
// body when the event attached evidence.
type record struct {
	domain.AuditEvent
	Snapshot *domain.Snapshot `json:"snapshot,omitempty"`
}

type Service struct {
	f     *os.File
	path  string
	size  int64
	seq   uint64
	head  string
	cases map[string]*domain.Case
	order []string
	audit map[string][]domain.AuditEvent
	// byAlert lists the cases each alert has been attached to.
	byAlert map[string][]string

	// Fail, when set, is consulted before each write (fault injection).
	Fail func() error
}

// Open loads and verifies the case log. A log that does not verify is an
// error: the service will not build cases on top of a broken chain.
func Open(path string) (*Service, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Service{f: f, path: path, cases: map[string]*domain.Case{},
		audit: map[string][]domain.AuditEvent{}, byAlert: map[string][]string{}}
	if err := s.load(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error { return s.f.Close() }

func (s *Service) load() error {
	st, err := s.f.Stat()
	if err != nil {
		return err
	}
	var off int64
	err = scan(io.NewSectionReader(s.f, 0, st.Size()), func(rec record, n int, torn bool) error {
		if torn {
			return s.f.Truncate(off)
		}
		if err := checkLink(rec, s.seq, s.head); err != nil {
			return fmt.Errorf("case log %s: %w", s.path, err)
		}
		s.apply(rec)
		off += int64(n)
		return nil
	})
	s.size = off
	return err
}

// scan walks a case log, handing each complete record to fn. A final line
// without a newline is reported once with torn set.
func scan(r io.Reader, fn func(rec record, n int, torn bool) error) error {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			if len(line) > 0 {
				return fn(record{}, len(line), true)
			}
			return nil
		}
		if err != nil {
			return err
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("case log: corrupt record: %w", err)
		}
		if err := fn(rec, len(line), false); err != nil {
			return err
		}
	}
}

// eventHash covers every field of the event except the hash itself.
func eventHash(e domain.AuditEvent) string {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SnapshotHash covers the alert and the full text of its events.
func SnapshotHash(sn domain.Snapshot) string {
	sn.Hash = ""
	b, err := json.Marshal(sn)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func checkLink(rec record, prevSeq uint64, prevHash string) error {
	if rec.Seq != prevSeq+1 {
		return fmt.Errorf("audit event out of sequence: got %d after %d", rec.Seq, prevSeq)
	}
	if rec.PrevHash != prevHash {
		return fmt.Errorf("audit event %d does not link to its predecessor", rec.Seq)
	}
	if eventHash(rec.AuditEvent) != rec.Hash {
		return fmt.Errorf("audit event %d has been altered (hash mismatch)", rec.Seq)
	}
	if rec.Snapshot != nil {
		var d Details
		if err := json.Unmarshal(rec.Details, &d); err != nil {
			return fmt.Errorf("audit event %d: %w", rec.Seq, err)
		}
		if SnapshotHash(*rec.Snapshot) != d.SnapshotHash || rec.Snapshot.Hash != d.SnapshotHash {
			return fmt.Errorf("evidence snapshot of audit event %d has been altered (hash mismatch)", rec.Seq)
		}
	}
	return nil
}

// append writes one audit event and only then applies it.
func (s *Service) append(actor string, now time.Time, action, caseID string, d Details, snap *domain.Snapshot) error {
	if s.Fail != nil {
		if err := s.Fail(); err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	details, err := json.Marshal(d)
	if err != nil {
		return err
	}
	ev := domain.AuditEvent{Seq: s.seq + 1, Time: now.UTC(), Actor: actor, Action: action,
		CaseID: caseID, Details: details, PrevHash: s.head}
	ev.Hash = eventHash(ev)
	rec := record{AuditEvent: ev, Snapshot: snap}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.f.WriteAt(b, s.size); err != nil {
		_ = s.f.Truncate(s.size)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.f.Sync(); err != nil {
		_ = s.f.Truncate(s.size)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.size += int64(len(b))
	s.apply(rec)
	return nil
}

// apply folds one event into the case it belongs to. It is the same code
// on a live write and on replay, so a restart reproduces the exact state.
func (s *Service) apply(rec record) {
	s.seq, s.head = rec.Seq, rec.Hash
	var d Details
	_ = json.Unmarshal(rec.Details, &d)

	c := s.cases[rec.CaseID]
	if rec.Action == ActOpened {
		c = &domain.Case{ID: rec.CaseID, Title: d.Title, Status: domain.CaseOpen,
			CreatedBy: rec.Actor, CreatedAt: rec.Time, Notes: []domain.Note{}}
		s.cases[c.ID] = c
		s.order = append(s.order, c.ID)
	}
	if c == nil {
		return
	}
	s.audit[c.ID] = append(s.audit[c.ID], rec.AuditEvent)
	c.UpdatedAt = rec.Time

	switch rec.Action {
	case ActOpened, ActAttached:
		c.AlertIDs = append(c.AlertIDs, d.AlertID)
		s.byAlert[d.AlertID] = append(s.byAlert[d.AlertID], c.ID)
		if rec.Snapshot != nil {
			c.Evidence = append(c.Evidence, *rec.Snapshot)
		}
		if d.Note != "" {
			c.Notes = append(c.Notes, domain.Note{At: rec.Time, Author: rec.Actor, Text: d.Note})
		}
	case ActTriaged:
		c.Priority, c.Assignee, c.Status = d.Priority, d.Assignee, d.ToStatus
	case ActNote:
		c.Notes = append(c.Notes, domain.Note{At: rec.Time, Author: rec.Actor, Text: d.Note})
	case ActEscalate:
		c.Status = domain.CaseEscalated
	case ActResolved:
		c.Status, c.Disposition, c.Summary = domain.CaseResolved, d.Disposition, d.Summary
		t := rec.Time
		c.ResolvedAt = &t
	case ActReopened:
		c.Status, c.Disposition, c.Summary, c.ResolvedAt = domain.CaseInReview, "", "", nil
	}
}

const (
	maxTitle = 200
	maxText  = 10000
)

func text(field, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		if required {
			return "", invalid("%s is required", field)
		}
		return "", nil
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		return "", invalid("%s must be valid text of at most %d characters", field, max)
	}
	return v, nil
}

func actorOK(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return invalid("an identified analyst is required")
	}
	return nil
}

// Create opens a case on one alert and freezes that alert's evidence.
func (s *Service) Create(actor string, now time.Time, title string, snap domain.Snapshot, note string) (domain.Case, error) {
	if err := actorOK(actor); err != nil {
		return domain.Case{}, err
	}
	title, err := text("title", title, maxTitle, false)
	if err != nil {
		return domain.Case{}, err
	}
	if title == "" {
		title = fmt.Sprintf("%s: %s in %s", snap.Alert.RuleID, strings.Join(snap.Alert.Accounts, ", "), snap.Alert.Symbol)
	}
	note, err = text("note", note, maxText, false)
	if err != nil {
		return domain.Case{}, err
	}
	snap.Hash = SnapshotHash(snap)
	id := fmt.Sprintf("case-%06d", len(s.order)+1)
	d := Details{Title: title, AlertID: snap.Alert.ID, SnapshotHash: snap.Hash, EventCount: len(snap.Events), Note: note}
	if err := s.append(actor, now, ActOpened, id, d, &snap); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

// Attach adds a further alert, with its own snapshot, to an unresolved case.
func (s *Service) Attach(actor string, now time.Time, id string, snap domain.Snapshot) (domain.Case, error) {
	c, err := s.find(actor, id)
	if err != nil {
		return domain.Case{}, err
	}
	if c.Status == domain.CaseResolved {
		return domain.Case{}, conflict("case %s is resolved; reopen it before attaching alerts", id)
	}
	for _, a := range c.AlertIDs {
		if a == snap.Alert.ID {
			return domain.Case{}, conflict("alert %s is already in case %s", a, id)
		}
	}
	snap.Hash = SnapshotHash(snap)
	d := Details{AlertID: snap.Alert.ID, SnapshotHash: snap.Hash, EventCount: len(snap.Events)}
	if err := s.append(actor, now, ActAttached, id, d, &snap); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

func (s *Service) find(actor, id string) (*domain.Case, error) {
	if err := actorOK(actor); err != nil {
		return nil, err
	}
	c, ok := s.cases[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

// Triage sets priority and assignee. From OPEN it moves the case to
// IN_REVIEW; later it re-prioritises or reassigns without changing status.
func (s *Service) Triage(actor string, now time.Time, id, priority, assignee string) (domain.Case, error) {
	c, err := s.find(actor, id)
	if err != nil {
		return domain.Case{}, err
	}
	switch priority {
	case "P1", "P2", "P3":
	default:
		return domain.Case{}, invalid("priority must be one of P1, P2, P3")
	}
	assignee, err = text("assignee", assignee, 64, true)
	if err != nil {
		return domain.Case{}, err
	}
	if c.Status == domain.CaseResolved {
		return domain.Case{}, conflict("case %s is resolved; reopen it first", id)
	}
	to := c.Status
	if c.Status == domain.CaseOpen {
		to = domain.CaseInReview
	}
	d := Details{Priority: priority, Assignee: assignee, FromStatus: c.Status, ToStatus: to}
	if err := s.append(actor, now, ActTriaged, id, d, nil); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

// AddNote appends an analyst note. Notes are never edited or removed.
func (s *Service) AddNote(actor string, now time.Time, id, note string) (domain.Case, error) {
	if _, err := s.find(actor, id); err != nil {
		return domain.Case{}, err
	}
	note, err := text("text", note, maxText, true)
	if err != nil {
		return domain.Case{}, err
	}
	if err := s.append(actor, now, ActNote, id, Details{Note: note}, nil); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

func (s *Service) Escalate(actor string, now time.Time, id, reason string) (domain.Case, error) {
	c, err := s.find(actor, id)
	if err != nil {
		return domain.Case{}, err
	}
	reason, err = text("reason", reason, maxText, true)
	if err != nil {
		return domain.Case{}, err
	}
	if c.Status != domain.CaseInReview {
		return domain.Case{}, conflict("only a case IN_REVIEW can be escalated; %s is %s", id, c.Status)
	}
	d := Details{Reason: reason, FromStatus: c.Status, ToStatus: domain.CaseEscalated}
	if err := s.append(actor, now, ActEscalate, id, d, nil); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

// Resolve closes a case with a disposition and a written summary. A case
// that was never triaged cannot be resolved: someone has to have owned it.
func (s *Service) Resolve(actor string, now time.Time, id, disposition, summary string) (domain.Case, error) {
	c, err := s.find(actor, id)
	if err != nil {
		return domain.Case{}, err
	}
	if !domain.ValidDisposition(disposition) {
		return domain.Case{}, invalid("disposition must be one of %s, %s, %s, %s",
			domain.DispFalsePositive, domain.DispBenignExplained, domain.DispInconclusive, domain.DispReferred)
	}
	summary, err = text("summary", summary, maxText, true)
	if err != nil {
		return domain.Case{}, err
	}
	if c.Status != domain.CaseInReview && c.Status != domain.CaseEscalated {
		return domain.Case{}, conflict("only a case IN_REVIEW or ESCALATED can be resolved; %s is %s", id, c.Status)
	}
	d := Details{Disposition: disposition, Summary: summary, FromStatus: c.Status, ToStatus: domain.CaseResolved}
	if err := s.append(actor, now, ActResolved, id, d, nil); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

func (s *Service) Reopen(actor string, now time.Time, id, reason string) (domain.Case, error) {
	c, err := s.find(actor, id)
	if err != nil {
		return domain.Case{}, err
	}
	reason, err = text("reason", reason, maxText, true)
	if err != nil {
		return domain.Case{}, err
	}
	if c.Status != domain.CaseResolved {
		return domain.Case{}, conflict("only a RESOLVED case can be reopened; %s is %s", id, c.Status)
	}
	d := Details{Reason: reason, FromStatus: c.Status, ToStatus: domain.CaseInReview}
	if err := s.append(actor, now, ActReopened, id, d, nil); err != nil {
		return domain.Case{}, err
	}
	return s.view(id), nil
}

// view returns a copy with the audit trail but without the evidence
// bodies, which are large and have their own export.
func (s *Service) view(id string) domain.Case {
	c := *s.cases[id]
	c.AlertIDs = append([]string(nil), c.AlertIDs...)
	c.Notes = append([]domain.Note{}, c.Notes...)
	c.Evidence = nil
	c.AuditEvents = append([]domain.AuditEvent(nil), s.audit[id]...)
	return c
}

func (s *Service) Get(id string) (domain.Case, bool) {
	if _, ok := s.cases[id]; !ok {
		return domain.Case{}, false
	}
	return s.view(id), true
}

// List returns cases, newest first, without their audit trails.
func (s *Service) List(status string) []domain.Case {
	out := []domain.Case{}
	for i := len(s.order) - 1; i >= 0; i-- {
		c := s.view(s.order[i])
		c.AuditEvents = nil
		if status == "" || c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

// CasesOf lists the cases an alert is attached to, oldest first.
func (s *Service) CasesOf(alertID string) []string {
	return append([]string(nil), s.byAlert[alertID]...)
}

// Status reports a case's status, or "" if there is no such case.
func (s *Service) Status(id string) string {
	if c, ok := s.cases[id]; ok {
		return c.Status
	}
	return ""
}

type ChainStatus struct {
	Verified bool   `json:"verified"`
	Events   uint64 `json:"events"`
	Head     string `json:"head"`
	Error    string `json:"error,omitempty"`
}

// Head reports the in-memory chain head without touching the disk.
func (s *Service) Head() ChainStatus {
	return ChainStatus{Events: s.seq, Head: s.head}
}

// Verify re-reads the whole log from disk and re-checks every link and
// every snapshot, independently of what is in memory.
func (s *Service) Verify() ChainStatus {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return ChainStatus{Error: err.Error()}
	}
	var seq uint64
	var head string
	err = scan(bytes.NewReader(b), func(rec record, _ int, torn bool) error {
		if torn {
			return errors.New("case log ends in an incomplete record")
		}
		if err := checkLink(rec, seq, head); err != nil {
			return err
		}
		seq, head = rec.Seq, rec.Hash
		return nil
	})
	st := ChainStatus{Events: seq, Head: head}
	switch {
	case err != nil:
		st.Error = err.Error()
	case seq != s.seq || head != s.head:
		st.Error = "case log on disk does not match the state in memory"
	default:
		st.Verified = true
	}
	return st
}

// Bundle is the evidence export for one case.
type Bundle struct {
	Disclaimer string            `json:"disclaimer"`
	ExportedAt time.Time         `json:"exported_at"`
	ExportedBy string            `json:"exported_by"`
	Case       domain.Case       `json:"case"`
	Evidence   []domain.Snapshot `json:"evidence"`
	// AuditTrail is this case's slice of the global chain. Each event's
	// hash can be recomputed from its own fields; the links between them
	// run through other cases' events, which AuditChain vouches for.
	AuditTrail []domain.AuditEvent `json:"audit_trail"`
	AuditChain ChainStatus         `json:"audit_chain"`
	BundleHash string              `json:"bundle_hash"`
}

// Export builds the evidence bundle and records that it was taken. If the
// export cannot be recorded it is not handed out.
func (s *Service) Export(actor string, now time.Time, id string) (Bundle, error) {
	if _, err := s.find(actor, id); err != nil {
		return Bundle{}, err
	}
	chain := s.Verify()
	c := s.view(id)
	trail := c.AuditEvents
	c.AuditEvents = nil
	b := Bundle{Disclaimer: domain.Disclaimer, ExportedAt: now.UTC(), ExportedBy: actor, Case: c,
		Evidence:   append([]domain.Snapshot{}, s.cases[id].Evidence...),
		AuditTrail: trail, AuditChain: chain}
	raw, err := json.Marshal(b)
	if err != nil {
		return Bundle{}, err
	}
	h := sha256.Sum256(raw)
	b.BundleHash = hex.EncodeToString(h[:])
	if err := s.append(actor, now, ActExported, id, Details{BundleHash: b.BundleHash, EventCount: len(trail)}, nil); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// Summary is the analyst-side statistics.
type Summary struct {
	Total             int            `json:"total"`
	ByStatus          map[string]int `json:"by_status"`
	ByDisposition     map[string]int `json:"by_disposition"`
	ResolutionRate    float64        `json:"resolution_rate"`
	MeanResolveSecs   float64        `json:"mean_seconds_to_resolve"`
	FalsePositiveRate float64        `json:"false_positive_share_of_resolved"`
}

func (s *Service) Summary() Summary {
	sm := Summary{Total: len(s.order), ByStatus: map[string]int{}, ByDisposition: map[string]int{}}
	var resolved int
	var secs float64
	for _, id := range s.order {
		c := s.cases[id]
		sm.ByStatus[c.Status]++
		if c.Status == domain.CaseResolved {
			resolved++
			sm.ByDisposition[c.Disposition]++
			secs += c.ResolvedAt.Sub(c.CreatedAt).Seconds()
		}
	}
	if sm.Total > 0 {
		sm.ResolutionRate = float64(resolved) / float64(sm.Total)
	}
	if resolved > 0 {
		sm.MeanResolveSecs = secs / float64(resolved)
		sm.FalsePositiveRate = float64(sm.ByDisposition[domain.DispFalsePositive]) / float64(resolved)
	}
	return sm
}
