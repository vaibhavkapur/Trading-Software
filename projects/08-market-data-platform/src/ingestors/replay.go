package ingestors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"mdp/src/domain"
)

// CaptureLine is one line of a capture file: a provider frame exactly as it
// was read, with the ingestor's receive time. Captures are NDJSON.
type CaptureLine struct {
	Provider string    `json:"provider"`
	Recv     time.Time `json:"recv"`
	Frame    string    `json:"frame"`
}

// CaptureWriter appends frames to a capture. It is safe for concurrent use
// so several ingestors can record into one file.
type CaptureWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func NewCaptureWriter(w io.Writer) *CaptureWriter {
	return &CaptureWriter{w: bufio.NewWriter(w)}
}

func (c *CaptureWriter) Write(provider string, recv time.Time, frame []byte) error {
	b, err := json.Marshal(CaptureLine{Provider: provider, Recv: recv.UTC(), Frame: string(frame)})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Write(b)
	return c.w.WriteByte('\n')
}

func (c *CaptureWriter) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Flush()
}

// Replay is a feed adapter that reads one provider's frames from a capture
// file and runs them through that provider's parser. Receive times come from
// the capture, so replaying the same file always yields the same events.
type Replay struct {
	Path   string
	Name   string        // provider whose frames to replay
	Parser func() Parser // that provider's parser
	// Speed paces the replay by captured receive times: 1 is real time, 2 is
	// twice as fast. 0 replays as fast as the pipeline accepts.
	Speed float64
}

// NewReplay returns a replay adapter for a known provider.
func NewReplay(path, provider string, speed float64) (*Replay, error) {
	r := &Replay{Path: path, Name: provider, Speed: speed}
	switch provider {
	case ProviderAlpha:
		r.Parser = func() Parser { return &AlphaParser{} }
	case ProviderBeta:
		r.Parser = func() Parser { return &BetaParser{} }
	default:
		return nil, fmt.Errorf("ingestors: no parser for provider %q", provider)
	}
	return r, nil
}

func (r *Replay) Provider() string  { return r.Name }
func (r *Replay) Source() string    { return domain.SourceReplay }
func (r *Replay) Live() bool        { return false }
func (r *Replay) NewParser() Parser { return r.Parser() }

// Domains: a capture's domains are known only by reading it.
func (r *Replay) Domains() []string { return nil }

func (r *Replay) Dial(context.Context, []domain.FeedOffset) (Conn, error) {
	f, err := os.Open(r.Path)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	return &replayConn{f: f, sc: sc, provider: r.Name, speed: r.Speed}, nil
}

type replayConn struct {
	f         *os.File
	sc        *bufio.Scanner
	provider  string
	speed     float64
	line      int
	firstRecv time.Time
	started   time.Time
}

func (c *replayConn) ReadFrame(ctx context.Context) (Frame, error) {
	for c.sc.Scan() {
		c.line++
		var l CaptureLine
		if err := json.Unmarshal(c.sc.Bytes(), &l); err != nil {
			return Frame{}, fmt.Errorf("replay: line %d: %w", c.line, err)
		}
		if l.Provider != c.provider {
			continue
		}
		if c.speed > 0 {
			if c.firstRecv.IsZero() {
				c.firstRecv, c.started = l.Recv, time.Now()
			}
			due := c.started.Add(time.Duration(float64(l.Recv.Sub(c.firstRecv)) / c.speed))
			if wait := time.Until(due); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return Frame{}, ctx.Err()
				}
			}
		}
		return Frame{Data: []byte(l.Frame), Recv: l.Recv.UTC()}, nil
	}
	if err := c.sc.Err(); err != nil {
		return Frame{}, fmt.Errorf("replay: %w", err)
	}
	return Frame{}, io.EOF
}

func (c *replayConn) Close() error { return c.f.Close() }

// ReadCapture loads a whole capture file.
func ReadCapture(path string) ([]CaptureLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var out []CaptureLine
	for n := 1; sc.Scan(); n++ {
		var l CaptureLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("capture %s line %d: %w", path, n, err)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}
