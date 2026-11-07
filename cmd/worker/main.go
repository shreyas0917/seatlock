// Command worker runs SeatLock's background jobs (internal/worker):
//
//	payment reconciler  every RECONCILE_INTERVAL (30s)  abandoned PENDING payments
//	outbox drainer      every OUTBOX_INTERVAL (5s)      refunds, Redis lock releases
//	hold expirer        every EXPIRE_INTERVAL (10s)     dead ACTIVE holds
//	invariants          every INVARIANTS_INTERVAL (15s) verify.sql checks as live gauges
//	key cleanup         every CLEANUP_INTERVAL (1h)     old idempotency keys
//
// It is wired from the SAME pieces as the API (config, pool, repository,
// Redis locker, payment client, booking.Service), so both apply the same rules.
// Several replicas can run at once: rows are claimed with FOR UPDATE SKIP LOCKED.
//
// On SIGTERM it stops starting new batches, finishes the running ones, then exits.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shreyas0917/seatlock/internal/booking"
	"github.com/shreyas0917/seatlock/internal/config"
	"github.com/shreyas0917/seatlock/internal/db"
	"github.com/shreyas0917/seatlock/internal/idempotency"
	"github.com/shreyas0917/seatlock/internal/invariants"
	"github.com/shreyas0917/seatlock/internal/lock"
	"github.com/shreyas0917/seatlock/internal/metrics"
	"github.com/shreyas0917/seatlock/internal/payment"
	"github.com/shreyas0917/seatlock/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker failed", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).With("service", "worker")
	slog.SetDefault(logger)

	// Refuse to start with timings that would make the jobs unsafe.
	if err := cfg.ValidateWorker(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.RunMigrations {
		// Safe to run alongside the API: golang-migrate takes an advisory lock.
		if err := db.MigrateUp(cfg.DatabaseURL); err != nil {
			return err
		}
	}
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBMinConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Metrics, created first so every component is wired before the jobs start.
	m := metrics.NewWorker()
	m.RegisterDBPool(pool)

	redisClient := lock.NewClient(lock.Options{
		Addr:         cfg.RedisAddr,
		DialTimeout:  cfg.RedisDialTimeout,
		ReadTimeout:  cfg.RedisReadTimeout,
		WriteTimeout: cfg.RedisWriteTimeout,
		PoolSize:     int(cfg.RedisPoolSize),
		MinIdleConns: int(cfg.RedisMinIdleConns),
	})
	defer redisClient.Close()
	m.RegisterRedisPool(redisClient)
	locker := lock.NewRedisLocker(redisClient, cfg.HoldTTL, cfg.RedisLockExtra).WithObserver(m.ObserveLockOp)

	repo := booking.NewPostgresRepository(pool, cfg.DBQueryTimeout)
	repo.SetLimits(booking.Limits{PendingHoldMax: cfg.PendingHoldMax, OutboxMaxAttempts: int(cfg.OutboxMaxAttempts)})
	gateway := payment.NewInstrumented(payment.NewClient(cfg.PaymentURL, cfg.PaymentTimeout), m)
	svc := booking.NewService(repo, locker, gateway, cfg.HoldTTL)
	keys := idempotency.NewStore(pool, cfg.IdempotencyLease, cfg.DBQueryTimeout)
	batch := int(cfg.WorkerBatchSize)
	checker := &worker.InvariantChecker{
		Store:   &invariants.Store{Pool: pool, Checks: invariants.Checks, StatementTimeout: cfg.InvariantStatementTimeout},
		Metrics: m, Logger: logger,
	}
	checker.Init()

	jobs := []worker.Scheduled{
		{Interval: cfg.ReconcileInterval, Job: &worker.Reconciler{
			Store: repo, Actions: svc, Logger: logger,
			OlderThan: cfg.ReconcileAfter, Lease: cfg.ReconcileLease, BatchSize: batch}},
		{Interval: cfg.OutboxInterval, Job: &worker.OutboxDrainer{
			Store: repo, Actions: svc, Logger: logger, Lease: cfg.OutboxLease, BatchSize: batch}},
		{Interval: cfg.ExpireInterval, Job: &worker.HoldExpirer{
			Store: repo, Logger: logger, BatchSize: batch}},
		{Interval: cfg.InvariantsInterval, Job: checker},
		{Interval: cfg.CleanupInterval, Job: &worker.KeyCleanup{
			Keys: keys, Logger: logger, Retention: cfg.IdempotencyRetention, BatchSize: batch}},
	}

	logger.Info("worker starting",
		"reconcile_after", cfg.ReconcileAfter.String(), "pending_hold_max", cfg.PendingHoldMax.String(),
		"outbox_max_attempts", cfg.OutboxMaxAttempts, "batch_size", batch)
	// /metrics and /debug/pprof on a separate internal port (default :9092), like the API.
	metricsAddr := cfg.MetricsAddr
	if metricsAddr == "" {
		metricsAddr = ":9092"
	}
	metrics.EnableContentionProfiles(cfg.PprofBlockRate, cfg.PprofMutexFraction)
	metricsServer := metrics.NewServer(metricsAddr, m)
	go func() {
		logger.Info("metrics server listening", "addr", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "error", err.Error())
		}
	}()

	worker.Run(ctx, jobs, logger, m) // returns after SIGTERM, once running batches finished

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsServer.Shutdown(shutdownCtx)
	logger.Info("worker stopped cleanly")
	return nil
}
