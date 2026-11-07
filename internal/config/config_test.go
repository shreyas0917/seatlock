// config_test.go tests the worker's startup safety checks (ValidateWorker).
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsAreSafeForTheWorker(t *testing.T) {
	cfg, err := Load()
	require.NoError(t, err)
	assert.NoError(t, cfg.ValidateWorker())
}

func TestValidateWorker_RefusesUnsafeTimings(t *testing.T) {
	base, err := Load()
	require.NoError(t, err)

	tests := []struct {
		name   string
		change func(c *Config)
		want   string
	}{
		{"reconcile too early", func(c *Config) { c.ReconcileAfter = 10 * time.Second }, "RECONCILE_AFTER"},
		{"pending hold max too short", func(c *Config) { c.PendingHoldMax = time.Minute }, "PENDING_HOLD_MAX"},
		{"reconcile lease too short", func(c *Config) { c.ReconcileLease = 5 * time.Second }, "RECONCILE_LEASE"},
		{"outbox lease too short", func(c *Config) { c.OutboxLease = time.Second }, "OUTBOX_LEASE"},
		{"retention too short", func(c *Config) { c.IdempotencyRetention = time.Second }, "IDEMPOTENCY_RETENTION"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.change(&c)
			err := c.ValidateWorker()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
