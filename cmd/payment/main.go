// Command payment runs the mock payment gateway (internal/paymentmock) as its
// own HTTP service, so the SeatLock API talks to it over the network exactly
// as it would talk to a real gateway.
//
// Configuration (environment variables):
//
//	PAYMENT_ADDR        listen address            (default :8090)
//	PAYMENT_FAIL_RATE   0..1, random declines      (default 0)
//	PAYMENT_MIN_DELAY   random delay, lower bound  (default 0s)
//	PAYMENT_MAX_DELAY   random delay, upper bound  (default 0s)
//	PAYMENT_SLOW_DELAY  delay of a "slow" charge   (default 3s)
//
// Demos force behaviours with POST /debug/next-behavior and start clean with
// POST /debug/reset.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shreyas0917/seatlock/internal/paymentmock"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("payment mock failed", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	failRate, err := strconv.ParseFloat(envOr("PAYMENT_FAIL_RATE", "0"), 64)
	if err != nil || failRate < 0 || failRate > 1 {
		return fmt.Errorf("PAYMENT_FAIL_RATE must be a number between 0 and 1")
	}
	opts := paymentmock.Options{FailRate: failRate, Logger: logger}
	for _, d := range []struct {
		key string
		dst *time.Duration
		def string
	}{
		{"PAYMENT_MIN_DELAY", &opts.MinDelay, "0s"},
		{"PAYMENT_MAX_DELAY", &opts.MaxDelay, "0s"},
		{"PAYMENT_SLOW_DELAY", &opts.SlowDelay, "3s"},
	} {
		if *d.dst, err = time.ParseDuration(envOr(d.key, d.def)); err != nil {
			return fmt.Errorf("invalid %s: %w", d.key, err)
		}
	}

	addr := envOr("PAYMENT_ADDR", ":8090")
	server := &http.Server{Addr: addr, Handler: paymentmock.New(opts), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("payment mock listening", "addr", addr, "fail_rate", failRate)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
