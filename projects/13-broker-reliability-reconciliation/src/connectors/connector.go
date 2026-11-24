package connectors

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
)

var (
	// ErrDisconnected indicates a broker or internal OMS connector cannot reach its upstream.
	ErrDisconnected = errors.New("connector upstream disconnected")
	// ErrRateLimited indicates temporary upstream throttling; safe to retry.
	ErrRateLimited = errors.New("connector rate limited")
	// ErrInvalidCursor indicates an unrecognized pagination token.
	ErrInvalidCursor = errors.New("invalid pagination cursor")
)

// PageRequest controls cursor-based pagination against internal OMS and broker APIs.
type PageRequest struct {
	AccountID string
	Cutoff    time.Time
	Cursor    string
	Limit     int
}

// PageResponse carries a page of records plus the opaque cursor for the next page.
type PageResponse[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// InternalConnector is a strictly read-only adapter to the internal OMS and ledger.
// Non-goal invariant: this interface exposes no write/mutate/order-placement methods.
type InternalConnector interface {
	Name() string
	ListOrders(ctx context.Context, req PageRequest) (PageResponse[domain.LedgerOrder], error)
	ListFills(ctx context.Context, req PageRequest) (PageResponse[domain.LedgerFill], error)
	ListPositions(ctx context.Context, req PageRequest) (PageResponse[domain.PositionSnapshot], error)
	ListCashBalances(ctx context.Context, req PageRequest) (PageResponse[domain.CashSnapshot], error)
}

// BrokerConnector is a strictly read-only adapter to broker APIs and end-of-day statements.
// Non-goal invariant: this interface cannot place offsetting trades or alter broker records.
type BrokerConnector interface {
	Name() string
	ListOrders(ctx context.Context, req PageRequest) (PageResponse[domain.BrokerOrder], error)
	ListFills(ctx context.Context, req PageRequest) (PageResponse[domain.BrokerFill], error)
	ListPositions(ctx context.Context, req PageRequest) (PageResponse[domain.PositionSnapshot], error)
	ListCashBalances(ctx context.Context, req PageRequest) (PageResponse[domain.CashSnapshot], error)
}

// RetryConfig configures bounded retries for transient connector errors.
type RetryConfig struct {
	MaxAttempts int
	BaseBackoff time.Duration
}

// DefaultRetryConfig returns a conservative retry policy for paginated fetches.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: 10 * time.Millisecond,
	}
}

// FetchAllPages drains a cursor-paginated endpoint with bounded retries on transient errors.
func FetchAllPages[T any](
	ctx context.Context,
	accountID string,
	cutoff time.Time,
	pageSize int,
	retryCfg RetryConfig,
	fetch func(context.Context, PageRequest) (PageResponse[T], error),
) ([]T, int, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	if retryCfg.MaxAttempts <= 0 {
		retryCfg.MaxAttempts = 1
	}

	var all []T
	cursor := ""
	pages := 0
	seenCursors := map[string]bool{}

	for {
		var resp PageResponse[T]
		var err error
		for attempt := 1; attempt <= retryCfg.MaxAttempts; attempt++ {
			resp, err = fetch(ctx, PageRequest{
				AccountID: accountID,
				Cutoff:    cutoff,
				Cursor:    cursor,
				Limit:     pageSize,
			})
			if err == nil {
				break
			}
			if !errors.Is(err, ErrRateLimited) && !errors.Is(err, ErrDisconnected) {
				return nil, pages, err
			}
			if attempt < retryCfg.MaxAttempts {
				delay := retryCfg.BaseBackoff * time.Duration(attempt)
				select {
				case <-ctx.Done():
					return nil, pages, ctx.Err()
				case <-time.After(delay):
				}
			}
		}
		if err != nil {
			return nil, pages, fmt.Errorf("fetch page %d (cursor=%q): %w", pages+1, cursor, err)
		}

		pages++
		all = append(all, resp.Items...)
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		if seenCursors[resp.NextCursor] {
			return nil, pages, fmt.Errorf("pagination loop detected at cursor %q", resp.NextCursor)
		}
		seenCursors[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
	return all, pages, nil
}
