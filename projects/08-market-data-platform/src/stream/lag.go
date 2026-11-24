package stream

import (
	"context"
	"time"
)

// LagObserver receives consumer lag measurements.
type LagObserver struct {
	// OnLag is called for every partition on every measurement.
	OnLag func(tp TP, lag int64)
	// OnCaughtUp is called once, the first time total lag reaches zero, with
	// the time since WatchLag started: the replay catch-up time.
	OnCaughtUp func(elapsed time.Duration)
}

// WatchLag measures how far positions() trails the end of the log every
// interval until ctx ends. Lag is the platform's backpressure signal: a
// stage that cannot keep up shows up here instead of dropping records.
func WatchLag(ctx context.Context, bus Bus, positions func() map[TP]int64, every time.Duration, obs LagObserver) {
	start := time.Now()
	caughtUp := false
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		lags, err := Lag(ctx, bus, positions())
		if err == nil {
			var total int64
			for tp, lag := range lags {
				total += lag
				if obs.OnLag != nil {
					obs.OnLag(tp, lag)
				}
			}
			if total == 0 && !caughtUp {
				caughtUp = true
				if obs.OnCaughtUp != nil {
					obs.OnCaughtUp(time.Since(start))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
