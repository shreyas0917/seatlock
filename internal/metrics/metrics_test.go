// metrics_test.go guards the rules every metric must follow: no label that
// Prometheus reserves or adds (job, instance), and no high-cardinality label
// (user, seat, hold, key, raw path).
package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// touchAll creates one child of every labelled metric, so Gather sees its labels.
func touchAll(m *Metrics) {
	m.HTTPRequests.WithLabelValues("/v1/holds", "POST", "201").Inc()
	m.HTTPDuration.WithLabelValues("/v1/holds", "POST").Observe(0.01)
	m.Holds.WithLabelValues("created").Inc()
	m.Confirms.WithLabelValues("booked").Inc()
	m.Idempotency.WithLabelValues("new").Inc()
	m.GatewayRequests.WithLabelValues("charge", "ok").Inc()
	m.GatewayDuration.WithLabelValues("charge").Observe(0.01)
	m.RedisLockOps.WithLabelValues("acquire", "ok").Inc()
	m.WorkerRuns.WithLabelValues("outbox_drainer", "ok").Inc()
	m.WorkerDuration.WithLabelValues("outbox_drainer").Observe(0.01)
	m.WorkerRows.WithLabelValues("outbox_drainer", "done").Inc()
	m.WorkerLastSuccess.WithLabelValues("outbox_drainer").Set(1)
	m.WorkerInterval.WithLabelValues("outbox_drainer").Set(5)
	m.InvariantViolations.WithLabelValues("double_booking").Set(0)
	m.InvariantCheckErrors.WithLabelValues("double_booking").Inc()
	m.InvariantLastSuccess.WithLabelValues("double_booking").Set(1)
	m.OutboxRows.WithLabelValues("PENDING").Set(0)
}

func TestNoReservedOrHighCardinalityLabels(t *testing.T) {
	reserved := map[string]bool{"job": true, "instance": true}
	highCardinality := []string{"user", "seat", "hold", "key", "path", "booking", "payment_id", "id"}

	for name, m := range map[string]*Metrics{"api": NewAPI(), "worker": NewWorker()} {
		touchAll(m)
		families, err := m.Registry.Gather()
		require.NoError(t, err, name)
		require.NotEmpty(t, families)

		for _, f := range families {
			for _, metric := range f.GetMetric() {
				for _, label := range metric.GetLabel() {
					l := label.GetName()
					assert.False(t, reserved[l], "%s: metric %s uses reserved label %q", name, f.GetName(), l)
					assert.False(t, strings.HasPrefix(l, "exported_"), "%s: %s has %q", name, f.GetName(), l)
					for _, bad := range highCardinality {
						assert.NotEqual(t, bad, l, "%s: metric %s uses high-cardinality label %q", name, f.GetName(), l)
						assert.False(t, strings.HasPrefix(l, bad+"_"), "%s: metric %s uses high-cardinality label %q", name, f.GetName(), l)
					}
				}
			}
		}
	}
}

func TestAPIAndWorkerExportOnlyTheirOwnMetrics(t *testing.T) {
	api, worker := NewAPI(), NewWorker()
	touchAll(api)
	touchAll(worker)

	assert.True(t, has(t, api.Registry, "seatlock_http_requests_total"))
	assert.False(t, has(t, api.Registry, "seatlock_invariant_violations"), "the API does not run invariant checks")
	assert.True(t, has(t, worker.Registry, "seatlock_invariant_violations"))
	assert.False(t, has(t, worker.Registry, "seatlock_http_requests_total"), "the worker serves no HTTP API")
	assert.True(t, has(t, worker.Registry, "go_goroutines"), "runtime collectors are registered")
}

func has(t *testing.T, reg *prometheus.Registry, name string) bool {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == name {
			return true
		}
	}
	return false
}
