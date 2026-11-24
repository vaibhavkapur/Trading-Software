// Command brokersim runs the simulated broker as its own process, so the OMS
// can be exercised against something it does not control.
//
// SIMULATED. It matches orders against fixed reference prices and never
// connects to a real venue.
//
//	BROKERSIM_HTTP_ADDR        listen address (default 127.0.0.1:9090)
//	BROKERSIM_API_KEY          bearer token the OMS must present (required)
//	BROKERSIM_WEBHOOK_URL      where to push reports, e.g.
//	                           http://127.0.0.1:8080/v1/broker/webhooks (required)
//	BROKERSIM_WEBHOOK_SECRET   shared secret that signs webhooks (required)
//	BROKERSIM_PRICES           JSON object of symbol to reference price
//	                           (default AAPL, MSFT, TSLA, NVDA)
//	BROKERSIM_ACK_DELAY        default 20ms
//	BROKERSIM_FILL_DELAY       default 100ms
//	BROKERSIM_CANCEL_DELAY     default 20ms
//
// Faults are injected at runtime through PUT /sim/faults; see the README.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"oms/src/brokersim"
	"oms/src/fixed"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "brokersim:", err)
		os.Exit(1)
	}
}

func run() error {
	var problems []string
	required := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			problems = append(problems, name+" is required")
		}
		return v
	}
	duration := func(name string, def time.Duration) time.Duration {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			problems = append(problems, name+" is not a valid duration")
			return def
		}
		return d
	}

	cfg := brokersim.Config{
		APIKey:        required("BROKERSIM_API_KEY"),
		WebhookURL:    required("BROKERSIM_WEBHOOK_URL"),
		WebhookSecret: required("BROKERSIM_WEBHOOK_SECRET"),
		Auto:          true,
		AutoDeliver:   true,
		AckDelay:      duration("BROKERSIM_ACK_DELAY", 20*time.Millisecond),
		FillDelay:     duration("BROKERSIM_FILL_DELAY", 100*time.Millisecond),
		CancelDelay:   duration("BROKERSIM_CANCEL_DELAY", 20*time.Millisecond),
		FeePerUnit:    fixed.MustParse("0.005"),
		Prices: map[string]fixed.Micros{
			"AAPL": fixed.MustParse("150"), "MSFT": fixed.MustParse("400"),
			"TSLA": fixed.MustParse("250"), "NVDA": fixed.MustParse("900"),
		},
		Log: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	if raw := os.Getenv("BROKERSIM_PRICES"); raw != "" {
		var prices map[string]fixed.Micros
		if err := json.Unmarshal([]byte(raw), &prices); err != nil || len(prices) == 0 {
			problems = append(problems, `BROKERSIM_PRICES must be a JSON object such as {"AAPL":"150.00"}`)
		} else {
			for symbol, p := range prices {
				if p <= 0 {
					problems = append(problems, "BROKERSIM_PRICES: the price of "+symbol+" must be positive")
				}
			}
			cfg.Prices = prices
		}
	}
	addr := os.Getenv("BROKERSIM_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9090"
	}
	if len(problems) > 0 {
		return errors.New("configuration:\n  " + strings.Join(problems, "\n  "))
	}

	sim := brokersim.New(cfg)
	defer sim.Close()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           sim.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	cfg.Log.Info("brokersim started", "mode", "SIMULATED", "addr", ln.Addr().String(), "symbols", len(cfg.Prices))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	cfg.Log.Info("brokersim stopped")
	return nil
}
