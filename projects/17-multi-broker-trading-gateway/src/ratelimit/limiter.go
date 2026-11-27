package ratelimit

import (
	"fmt"
	"sync"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// Clock allows deterministic time control in tests and production UTC wall-clock operation.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// RealClock returns a UTC wall clock.
func RealClock() Clock { return realClock{} }

// ManualClock is a deterministic controllable clock for tests and simulations.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock initializes a ManualClock at t UTC.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t.UTC()}
}

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the manual clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TokenBucket enforces per-broker request rate limits and upstream Retry-After cool-down windows.
type TokenBucket struct {
	mu           sync.Mutex
	brokerID     string
	ratePerSec   float64
	burst        float64
	tokens       float64
	lastRefill   time.Time
	blockedUntil time.Time
	clock        Clock
}

// NewTokenBucket creates a token bucket limiter for a broker.
func NewTokenBucket(brokerID string, ratePerSec float64, burst int, clock Clock) *TokenBucket {
	if clock == nil {
		clock = RealClock()
	}
	if ratePerSec <= 0 {
		ratePerSec = 10
	}
	if burst <= 0 {
		burst = 1
	}
	return &TokenBucket{
		brokerID:   brokerID,
		ratePerSec: ratePerSec,
		burst:      float64(burst),
		tokens:     float64(burst),
		lastRefill: clock.Now(),
		clock:      clock,
	}
}

// Allow attempts to consume one request token. Returns a structured RATE_LIMITED error with RetryAfter if denied.
func (tb *TokenBucket) Allow() error {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := tb.clock.Now()
	if now.Before(tb.blockedUntil) {
		retryAfter := tb.blockedUntil.Sub(now)
		return domain.NewError(
			domain.ErrCodeRateLimited,
			fmt.Sprintf("broker %q rate limit cooldown active for %s", tb.brokerID, retryAfter.Round(time.Millisecond)),
			true,
		).WithContext(tb.brokerID, "").WithRetryAfter(retryAfter)
	}

	elapsed := now.Sub(tb.lastRefill).Seconds()
	if elapsed > 0 {
		tb.tokens += elapsed * tb.ratePerSec
		if tb.tokens > tb.burst {
			tb.tokens = tb.burst
		}
		tb.lastRefill = now
	}

	if tb.tokens < 1.0 {
		needed := 1.0 - tb.tokens
		waitSec := needed / tb.ratePerSec
		retryAfter := time.Duration(waitSec * float64(time.Second))
		if retryAfter < time.Millisecond {
			retryAfter = time.Millisecond
		}
		return domain.NewError(
			domain.ErrCodeRateLimited,
			fmt.Sprintf("broker %q local rate limit exceeded; retry after %s", tb.brokerID, retryAfter.Round(time.Millisecond)),
			true,
		).WithContext(tb.brokerID, "").WithRetryAfter(retryAfter)
	}

	tb.tokens -= 1.0
	return nil
}

// ApplyRetryAfter honors an upstream 429 Retry-After header by pausing outbound dispatch until now + d.
func (tb *TokenBucket) ApplyRetryAfter(d time.Duration) {
	if d <= 0 {
		return
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	until := tb.clock.Now().Add(d)
	if until.After(tb.blockedUntil) {
		tb.blockedUntil = until
	}
}

// Snapshot returns current available tokens and cooldown state for observability.
func (tb *TokenBucket) Snapshot() (tokens float64, blockedUntil time.Time) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.tokens, tb.blockedUntil
}
