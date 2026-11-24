// Package audit is the append-only evidence log. Each line is a record whose
// hash covers the previous record's hash, so any edit, deletion or reordering
// breaks the chain, and each hash is signed with Ed25519 so a holder of the
// public key can verify records without being able to forge them.
//
// The log doubles as the engine's journal: state is rebuilt by replaying it.
package audit

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"riskengine/src/domain"
)

type Record struct {
	Seq      uint64          `json:"seq"`
	PrevHash string          `json:"prev_hash"`
	Event    json.RawMessage `json:"event"`
	Hash     string          `json:"hash"`
	Sig      string          `json:"sig"`
}

func (r Record) Seal() domain.Seal {
	return domain.Seal{Seq: r.Seq, Hash: r.Hash, Signature: r.Sig}
}

// Genesis is the prev_hash of the first record.
var Genesis = strings.Repeat("0", 64)

func recordHash(seq uint64, prev string, event []byte) string {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], seq)
	h.Write(n[:])
	h.Write([]byte(prev))
	h.Write(event)
	return hex.EncodeToString(h.Sum(nil))
}

// check verifies one record against its expected position in the chain.
func check(r Record, wantSeq uint64, prev string, pub ed25519.PublicKey) error {
	if r.Seq != wantSeq {
		return fmt.Errorf("seq %d: expected seq %d", r.Seq, wantSeq)
	}
	if r.PrevHash != prev {
		return fmt.Errorf("seq %d: prev_hash does not match the preceding record", r.Seq)
	}
	if recordHash(r.Seq, r.PrevHash, r.Event) != r.Hash {
		return fmt.Errorf("seq %d: hash mismatch (record altered)", r.Seq)
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil || !ed25519.Verify(pub, []byte(r.Hash), sig) {
		return fmt.Errorf("seq %d: bad signature", r.Seq)
	}
	return nil
}

// scan reads and verifies every record, calling fn for each. It returns the
// byte offset just past the last good record. A final line that is
// incomplete (no newline) is a torn write from a crash and is reported via
// torn rather than as corruption; anything else that fails is an error.
func scan(r io.Reader, pub ed25519.PublicKey, fn func(Record) error) (seq uint64, last string, good int64, torn bool, err error) {
	br := bufio.NewReaderSize(r, 1<<16)
	last = Genesis
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr == io.EOF {
			return seq, last, good, len(line) > 0, nil
		}
		if rerr != nil {
			return seq, last, good, false, rerr
		}
		var rec Record
		if e := json.Unmarshal(line, &rec); e != nil {
			return seq, last, good, false, fmt.Errorf("audit: record after seq %d is not valid JSON: %w", seq, e)
		}
		if e := check(rec, seq+1, last, pub); e != nil {
			return seq, last, good, false, fmt.Errorf("audit: %w", e)
		}
		if fn != nil {
			if e := fn(rec); e != nil {
				return seq, last, good, false, e
			}
		}
		seq, last = rec.Seq, rec.Hash
		good += int64(len(line))
	}
}

// Verify checks the whole chain in a file and returns the record count and
// head hash.
func Verify(path string, pub ed25519.PublicKey) (uint64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	seq, last, _, torn, err := scan(f, pub, nil)
	if err == nil && torn {
		err = errors.New("audit: trailing partial record")
	}
	return seq, last, err
}

type Log struct {
	mu   sync.Mutex
	f    *os.File
	key  ed25519.PrivateKey
	seq  uint64
	last string
	sync bool
}

// Open verifies the existing log, replays every record through fn, and
// positions for appending. A torn final line is truncated; any other
// verification failure aborts, because starting on top of a log that cannot
// be trusted would mean serving decisions from unknown state.
// With syncEach, every append is fsynced before it returns.
func Open(path string, key ed25519.PrivateKey, syncEach bool, fn func(Record) error) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	seq, last, good, torn, err := scan(f, key.Public().(ed25519.PublicKey), fn)
	if err != nil {
		f.Close()
		return nil, err
	}
	if torn {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &Log{f: f, key: key, seq: seq, last: last, sync: syncEach}, nil
}

// Append writes one event and returns its seal. The record reaches the OS
// before Append returns, so it survives a process crash; surviving power
// loss additionally needs syncEach or periodic Sync calls.
func (l *Log) Append(ev domain.Event) (domain.Seal, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return domain.Seal{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := Record{Seq: l.seq + 1, PrevHash: l.last, Event: body}
	rec.Hash = recordHash(rec.Seq, rec.PrevHash, body)
	rec.Sig = hex.EncodeToString(ed25519.Sign(l.key, []byte(rec.Hash)))
	line, err := json.Marshal(rec)
	if err != nil {
		return domain.Seal{}, err
	}
	line = append(line, '\n')
	off, err := l.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return domain.Seal{}, err
	}
	if _, err := l.f.Write(line); err != nil {
		// Drop any partial line so the chain stays parseable.
		l.f.Truncate(off)
		l.f.Seek(off, io.SeekStart)
		return domain.Seal{}, err
	}
	if l.sync {
		if err := l.f.Sync(); err != nil {
			return domain.Seal{}, err
		}
	}
	l.seq, l.last = rec.Seq, rec.Hash
	return rec.Seal(), nil
}

func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Sync()
}

func (l *Log) Head() (uint64, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq, l.last
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Sync()
	return l.f.Close()
}

// LoadOrCreateKey reads a hex Ed25519 seed from path, generating one (mode
// 0600) on first use. The key file must stay out of source control.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("audit: %s is not a %d-byte hex seed", path, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
