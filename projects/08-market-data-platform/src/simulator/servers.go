package simulator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"mdp/src/ingestors"
)

// clientQueue is how many frames a client may fall behind before the venue
// disconnects it.
const clientQueue = 8192

const heartbeatEvery = 500 * time.Millisecond

func newClient(want map[string]bool) (*subscriber, chan []byte) {
	ch := make(chan []byte, clientQueue)
	s := &subscriber{want: want, done: make(chan struct{})}
	s.deliver = func(frame []byte) bool {
		select {
		case ch <- frame:
			return true
		default:
			return false
		}
	}
	return s, ch
}

// pump writes the hello frame, the backlog, then live frames and heartbeats
// until the client or the venue goes away.
func (v *Venue) pump(ctx context.Context, s *subscriber, ch chan []byte, resume map[string]string,
	write func([]byte) error, heartbeat func() []byte) {
	session, backlog, next := v.subscribe(s, resume)
	defer v.unsubscribe(s)
	if write(v.hello(session, next)) != nil {
		return
	}
	for _, f := range backlog {
		if write(f) != nil {
			return
		}
	}
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	for {
		select {
		case f := <-ch:
			if write(f) != nil {
				return
			}
		case <-hb.C:
			if write(heartbeat()) != nil {
				return
			}
		case <-s.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// ServeAlpha is the alpha venue's WebSocket endpoint.
func (v *Venue) ServeAlpha(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	rctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	_, data, err := c.Read(rctx)
	cancel()
	if err != nil {
		return
	}
	var req struct {
		Op      string            `json:"op"`
		Symbols []string          `json:"symbols"`
		Resume  map[string]string `json:"resume"`
	}
	if json.Unmarshal(data, &req) != nil || req.Op != "subscribe" {
		c.Close(websocket.StatusPolicyViolation, "expected subscribe")
		return
	}
	var want map[string]bool
	if len(req.Symbols) > 0 {
		want = map[string]bool{}
		for _, sym := range req.Symbols {
			want[sym] = true
		}
	}
	ctx := c.CloseRead(r.Context())
	s, ch := newClient(want)
	v.pump(ctx, s, ch, req.Resume,
		func(f []byte) error {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return c.Write(wctx, websocket.MessageText, f)
		},
		func() []byte {
			return fmt.Appendf(nil, `{"op":"hb","ts":%q}`, time.Now().UTC().Format(time.RFC3339Nano))
		})
}

// ServeBeta accepts beta venue TCP clients until ctx ends.
func (v *Venue) ServeBeta(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go v.serveBetaConn(ctx, conn)
	}
}

func (v *Venue) serveBetaConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	f := strings.Split(strings.TrimSpace(line), "|")
	if len(f) != 3 || f[0] != "LOGIN" {
		fmt.Fprint(conn, "ERR|expected LOGIN|session|last_seq\n")
		return
	}
	conn.SetReadDeadline(time.Time{})
	resume := map[string]string{}
	if f[1] != "" {
		resume[ingestors.BetaDomain] = f[1] + "@" + f[2]
	}
	// The client sends nothing after LOGIN; a read returning means it left.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		conn.Read(make([]byte, 1))
		cancel()
	}()
	s, ch := newClient(nil)
	v.pump(cctx, s, ch, resume,
		func(frame []byte) error {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			// The frame is shared with the resend buffer and with every
			// other connection, so the newline goes into a copy: appending
			// in place would write into the shared backing array.
			line := make([]byte, len(frame)+1)
			copy(line, frame)
			line[len(frame)] = '\n'
			_, err := conn.Write(line)
			return err
		},
		func() []byte {
			return fmt.Appendf(nil, "H|%d|%d", v.lastSeq(ingestors.BetaDomain), time.Now().UnixMicro())
		})
}
