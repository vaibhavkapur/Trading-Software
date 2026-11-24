// Package alertengine manages alert threshold policies, cooldowns, lifecycle expiry,
// config versioning, and durable evidence persistence.
package alertengine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"arbmonitor/src/domain"
)

// AlertStore persists immutable alerts with their evidence snapshots.
type AlertStore interface {
	Append(alert domain.Alert) error
	List() ([]domain.Alert, error)
}

// MemoryAlertStore is a thread-safe in-memory AlertStore.
type MemoryAlertStore struct {
	mu     sync.RWMutex
	alerts []domain.Alert
}

// NewMemoryAlertStore creates an empty MemoryAlertStore.
func NewMemoryAlertStore() *MemoryAlertStore {
	return &MemoryAlertStore{}
}

func (s *MemoryAlertStore) Append(alert domain.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, alert)
	return nil
}

func (s *MemoryAlertStore) List() ([]domain.Alert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Alert, len(s.alerts))
	copy(out, s.alerts)
	return out, nil
}

// JSONLAlertStore durably appends alerts as newline-delimited JSON with fsync.
type JSONLAlertStore struct {
	mu   sync.Mutex
	path string
}

// NewJSONLAlertStore opens or creates a JSONL alert archive at path.
func NewJSONLAlertStore(path string) (*JSONLAlertStore, error) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create alert store dir: %w", err)
		}
	}
	return &JSONLAlertStore{path: path}, nil
}

func (s *JSONLAlertStore) Append(alert domain.Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}
	data = append(data, '\n')

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open alert archive: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write alert archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync alert archive: %w", err)
	}
	return nil
}

func (s *JSONLAlertStore) List() ([]domain.Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open alert archive: %w", err)
	}
	defer f.Close()

	var alerts []domain.Alert
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var a domain.Alert
		if err := json.Unmarshal(line, &a); err != nil {
			return nil, fmt.Errorf("parse alert archive: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan alert archive: %w", err)
	}
	return alerts, nil
}
