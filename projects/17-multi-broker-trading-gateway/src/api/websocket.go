package api

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WSHub manages RFC 6455 WebSocket subscribers using Go stdlib only (zero external dependencies).
type WSHub struct {
	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	conn net.Conn
	mu   sync.Mutex
}

// NewWSHub creates a WebSocket event broadcast hub.
func NewWSHub() *WSHub {
	return &WSHub{
		clients: make(map[*wsClient]struct{}),
	}
}

// Broadcast serializes evt as JSON and sends it as an RFC 6455 text frame to every connected subscriber.
func (h *WSHub) Broadcast(evt domain.GatewayEvent) {
	payload, err := json.Marshal(evt)
	if err != nil {
		return
	}
	frame := encodeTextFrame(payload)

	h.mu.Lock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.mu.Lock()
		_, err := c.conn.Write(frame)
		c.mu.Unlock()
		if err != nil {
			h.remove(c)
		}
	}
}

func (h *WSHub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	_ = c.conn.Close()
}

// HandleUpgrade performs an RFC 6455 WebSocket handshake and keeps the connection subscribed until closed.
func (h *WSHub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected Upgrade: websocket", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}

	accept := computeAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}

	client := &wsClient{conn: conn}
	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()

	// Drain client control/close frames in background so disconnect cleans up promptly.
	go func() {
		defer h.remove(client)
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
}

// ComputeWebSocketAccept computes RFC 6455 Sec-WebSocket-Accept for handshake validation.
func computeAcceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func encodeTextFrame(payload []byte) []byte {
	n := len(payload)
	var header []byte
	header = append(header, 0x81) // FIN=1, opcode=0x1 (text)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 65535:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	return append(header, payload...)
}

// ReadTextFrame reads a single unmasked server-to-client RFC 6455 text frame (used by test clients).
func ReadTextFrame(br *bufio.Reader) ([]byte, error) {
	b1, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	if (b1 & 0x0F) != 0x01 {
		return nil, errors.New("unexpected non-text websocket frame")
	}
	b2, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	length := uint64(b2 & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(br, data); err != nil {
		return nil, err
	}
	return data, nil
}
