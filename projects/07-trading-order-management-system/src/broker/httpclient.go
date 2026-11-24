package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPClient talks to the simulated broker's HTTP API.
type HTTPClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewHTTPClient builds a client. timeout bounds each call end to end; a call
// that times out is retryable because the broker may or may not have acted.
func NewHTTPClient(baseURL, apiKey string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *HTTPClient) Submit(ctx context.Context, r SubmitRequest) error {
	err := c.do(ctx, "submit", http.MethodPost, "/v1/orders", "", r, nil)
	if be, ok := err.(*Error); ok && be.Status == http.StatusNotFound {
		// The submit endpoint itself is missing: a routing or deployment
		// problem, not a verdict on this order.
		be.Retryable = true
	}
	return err
}

func (c *HTTPClient) Cancel(ctx context.Context, clientOrderID, requestID string) error {
	return c.do(ctx, "cancel", http.MethodPost, "/v1/orders/"+url.PathEscape(clientOrderID)+"/cancel", requestID, nil, nil)
}

func (c *HTTPClient) Replace(ctx context.Context, clientOrderID, requestID string, r ReplaceRequest) error {
	return c.do(ctx, "replace", http.MethodPost, "/v1/orders/"+url.PathEscape(clientOrderID)+"/replace", requestID, r, nil)
}

func (c *HTTPClient) GetOrder(ctx context.Context, clientOrderID string) (OrderSnapshot, error) {
	var o OrderSnapshot
	err := c.do(ctx, "get order", http.MethodGet, "/v1/orders/"+url.PathEscape(clientOrderID), "", nil, &o)
	if be, ok := err.(*Error); ok && be.Status == http.StatusNotFound {
		be.Err = ErrUnknownOrder
	}
	return o, err
}

func (c *HTTPClient) Snapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	err := c.do(ctx, "snapshot", http.MethodGet, "/v1/snapshot", "", nil, &s)
	return s, err
}

func (c *HTTPClient) do(ctx context.Context, op, method, path, requestID string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &Error{Op: op, Err: err}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		req.Header.Set("Idempotency-Key", requestID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// No response: the request may or may not have reached the broker.
		return &Error{Op: op, Retryable: true, Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Retryable: true, Err: err}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Op: op, Status: resp.StatusCode, Retryable: true, Err: fmt.Errorf("decode response: %w", err)}
		}
		return nil
	}

	be := &Error{Op: op, Status: resp.StatusCode}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil {
		be.Code, be.Message = payload.Error.Code, payload.Error.Message
	}
	// Only these statuses mean the broker understood this request and refused
	// it. Everything else (5xx, 408, 429, and also 401/403, which point at
	// credentials rather than at the order) is retried, so a broken deployment
	// cannot turn into a wave of permanently rejected orders.
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		be.Retryable = false
	default:
		be.Retryable = true
	}
	return be
}
