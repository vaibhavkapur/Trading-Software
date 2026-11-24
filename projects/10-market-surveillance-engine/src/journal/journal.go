// Package journal is the append-only log the engine is rebuilt from.
//
// It holds inputs (the active ruleset, every accepted event, every
// explicit watermark advance) and outputs (the alerts an input produced).
// An output record always sits directly after the input that caused it.
// One JSON object per line.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"surveillance/src/domain"
)

const (
	KindRuleset = "ruleset"
	KindEvent   = "event"
	KindAdvance = "advance"
	KindOut     = "out"
)

type Record struct {
	Kind string    `json:"k"`
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`

	// KindRuleset: the full ruleset that became active.
	Ruleset json.RawMessage `json:"ruleset,omitempty"`
	// KindEvent: the normalized event.
	Event *domain.Event `json:"event,omitempty"`
	// KindAdvance: the watermark was raised to this event time.
	AdvanceTo *time.Time `json:"advance_to,omitempty"`
	// KindOut: alerts created or updated by the input with the same Seq,
	// each in the state it was left in.
	Alerts []domain.Alert `json:"alerts,omitempty"`
}

type Journal struct {
	f      *os.File
	size   int64
	sync   bool
	broken error

	// Fail, when set, is consulted before each append; a non-nil error
	// makes the append fail without writing. It exists for fault injection.
	Fail func() error
}

// Open opens or creates the journal. Scan must be called before Append.
// With syncEach, every append is fsynced before it is acknowledged.
func Open(path string, syncEach bool) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Journal{f: f, size: st.Size(), sync: syncEach}, nil
}

// Scan replays the journal from the start.
//
// A final line with no newline is a write that was cut short by a crash:
// it was never acknowledged, so it is cut off and the journal continues
// from the last complete record. A complete line that does not parse is
// corruption, and that is an error: guessing past it could silently drop
// evidence.
func (j *Journal) Scan(fn func(off int64, rec Record) error) error {
	r := bufio.NewReaderSize(io.NewSectionReader(j.f, 0, j.size), 1<<16)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			if len(line) > 0 {
				if terr := j.f.Truncate(off); terr != nil {
					return fmt.Errorf("journal: dropping torn record at offset %d: %w", off, terr)
				}
			}
			j.size = off
			return nil
		}
		if err != nil {
			return fmt.Errorf("journal: read at offset %d: %w", off, err)
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("journal: corrupt record at offset %d: %w", off, err)
		}
		if err := fn(off, rec); err != nil {
			return err
		}
		off += int64(len(line))
	}
}

// Append writes one record and returns the offset it was written at. On
// failure nothing is left behind: a partial line is cut off again, and if
// even that fails the journal refuses all further writes.
func (j *Journal) Append(rec Record) (int64, error) {
	if j.broken != nil {
		return 0, j.broken
	}
	if j.Fail != nil {
		if err := j.Fail(); err != nil {
			return 0, err
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')
	off := j.size
	if _, err := j.f.WriteAt(b, off); err != nil {
		if terr := j.f.Truncate(off); terr != nil {
			j.broken = fmt.Errorf("journal unusable after failed write and failed rollback: %w", errors.Join(err, terr))
		}
		return 0, err
	}
	if j.sync {
		if err := j.f.Sync(); err != nil {
			if terr := j.f.Truncate(off); terr != nil {
				j.broken = fmt.Errorf("journal unusable after failed sync and failed rollback: %w", errors.Join(err, terr))
			}
			return 0, err
		}
	}
	j.size += int64(len(b))
	return off, nil
}

// ReadAt reads back the record that starts at off.
func (j *Journal) ReadAt(off int64) (Record, error) {
	var rec Record
	if off < 0 || off >= j.size {
		return rec, fmt.Errorf("journal: offset %d out of range", off)
	}
	r := bufio.NewReaderSize(io.NewSectionReader(j.f, off, j.size-off), 4096)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return rec, fmt.Errorf("journal: read at offset %d: %w", off, err)
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return rec, fmt.Errorf("journal: corrupt record at offset %d: %w", off, err)
	}
	return rec, nil
}

func (j *Journal) Size() int64 { return j.size }

func (j *Journal) Close() error { return j.f.Close() }
