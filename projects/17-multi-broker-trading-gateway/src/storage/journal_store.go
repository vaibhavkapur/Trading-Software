package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

type journalEntry struct {
	TimestampUTC time.Time                    `json:"timestamp_utc"`
	Kind         string                       `json:"kind"` // order, fill, webhook, reconcile, cursor
	Order        *domain.Order                `json:"order,omitempty"`
	Fill         *domain.Fill                 `json:"fill,omitempty"`
	WebhookKey   *webhookKey                  `json:"webhook_key,omitempty"`
	Report       *domain.ReconciliationReport `json:"report,omitempty"`
	Cursor       *cursorRecord                `json:"cursor,omitempty"`
}

type webhookKey struct {
	BrokerID string `json:"broker_id"`
	EventID  string `json:"event_id"`
}

type cursorRecord struct {
	BrokerID string `json:"broker_id"`
	Cursor   int64  `json:"cursor"`
}

// JournalStore wraps MemoryStore with an append-only JSONL write-ahead journal for deterministic crash recovery.
type JournalStore struct {
	*MemoryStore
	fileMu sync.Mutex
	path   string
}

// OpenJournalStore creates or replays an append-only journal at path.
func OpenJournalStore(path string) (*JournalStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	js := &JournalStore{
		MemoryStore: NewMemoryStore(),
		path:        path,
	}
	if err := js.replay(); err != nil {
		return nil, err
	}
	return js, nil
}

func (j *JournalStore) replay() error {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open journal for replay: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("corrupt journal line %d: %w", lineNo, err)
		}
		switch e.Kind {
		case "order":
			if e.Order != nil {
				_ = j.MemoryStore.SaveOrder(*e.Order)
			}
		case "fill":
			if e.Fill != nil {
				_, _ = j.MemoryStore.RecordFill(*e.Fill)
			}
		case "webhook":
			if e.WebhookKey != nil {
				_, _ = j.MemoryStore.MarkWebhookProcessed(e.WebhookKey.BrokerID, e.WebhookKey.EventID)
			}
		case "reconcile":
			if e.Report != nil {
				_ = j.MemoryStore.SaveReconciliationReport(*e.Report)
			}
		case "cursor":
			if e.Cursor != nil {
				_ = j.MemoryStore.SetReplayCursor(e.Cursor.BrokerID, e.Cursor.Cursor)
			}
		}
	}
	return sc.Err()
}

func (j *JournalStore) appendEntry(e journalEntry) error {
	j.fileMu.Lock()
	defer j.fileMu.Unlock()
	e.TimestampUTC = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (j *JournalStore) SaveOrder(order domain.Order) error {
	if err := j.appendEntry(journalEntry{Kind: "order", Order: &order}); err != nil {
		return err
	}
	return j.MemoryStore.SaveOrder(order)
}

func (j *JournalStore) RecordFill(fill domain.Fill) (bool, error) {
	inserted, err := j.MemoryStore.RecordFill(fill)
	if err != nil || !inserted {
		return inserted, err
	}
	if err := j.appendEntry(journalEntry{Kind: "fill", Fill: &fill}); err != nil {
		return false, err
	}
	return true, nil
}

func (j *JournalStore) MarkWebhookProcessed(brokerID, eventID string) (bool, error) {
	firstSeen, err := j.MemoryStore.MarkWebhookProcessed(brokerID, eventID)
	if err != nil || !firstSeen {
		return firstSeen, err
	}
	if err := j.appendEntry(journalEntry{Kind: "webhook", WebhookKey: &webhookKey{BrokerID: brokerID, EventID: eventID}}); err != nil {
		return false, err
	}
	return true, nil
}

func (j *JournalStore) SaveReconciliationReport(report domain.ReconciliationReport) error {
	if err := j.appendEntry(journalEntry{Kind: "reconcile", Report: &report}); err != nil {
		return err
	}
	return j.MemoryStore.SaveReconciliationReport(report)
}

func (j *JournalStore) SetReplayCursor(brokerID string, cursor int64) error {
	if err := j.appendEntry(journalEntry{Kind: "cursor", Cursor: &cursorRecord{BrokerID: brokerID, Cursor: cursor}}); err != nil {
		return err
	}
	return j.MemoryStore.SetReplayCursor(brokerID, cursor)
}
