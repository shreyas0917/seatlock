// pools.go exports connection-pool statistics of the database (pgxpool) and
// Redis (go-redis) as metrics.
//
// Both libraries already keep these numbers; a small custom collector reads
// them at scrape time (Collect), instead of copying them into gauges on a
// timer, so the values are exactly current whenever Prometheus asks.
//
// Panels: "DB pool in use / acquire wait", "Redis pool".
// Alert: DBPoolSaturated uses seatlock_db_pool_acquire_wait_seconds_total.
package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// ------------------------------------------------------------------ pgxpool

type dbPoolCollector struct {
	pool *pgxpool.Pool

	conns          *prometheus.Desc // by state: total, idle, acquired
	maxConns       *prometheus.Desc
	acquires       *prometheus.Desc
	emptyAcquires  *prometheus.Desc
	canceled       *prometheus.Desc
	acquireWaitSec *prometheus.Desc
}

// RegisterDBPool exports the pgx pool's statistics.
func (m *Metrics) RegisterDBPool(pool *pgxpool.Pool) {
	m.Registry.MustRegister(&dbPoolCollector{
		pool: pool,
		conns: prometheus.NewDesc("seatlock_db_pool_connections",
			"Database pool connections by state (total, idle, acquired).", []string{"state"}, nil),
		maxConns: prometheus.NewDesc("seatlock_db_pool_max_connections",
			"Configured maximum pool size (DB_MAX_CONNS).", nil, nil),
		acquires: prometheus.NewDesc("seatlock_db_pool_acquires_total",
			"Successful connection acquires from the pool.", nil, nil),
		emptyAcquires: prometheus.NewDesc("seatlock_db_pool_empty_acquires_total",
			"Acquires that had to WAIT because no idle connection was free.", nil, nil),
		canceled: prometheus.NewDesc("seatlock_db_pool_canceled_acquires_total",
			"Acquires abandoned because the request's context ended (e.g. timeout).", nil, nil),
		// Total time spent WAITING for a connection. Its rate is "seconds of
		// waiting per second": the clearest sign the pool is too small,
		// because waiting eats into every request's query timeout (Phase 2).
		acquireWaitSec: prometheus.NewDesc("seatlock_db_pool_acquire_wait_seconds_total",
			"Total time spent waiting to acquire a connection.", nil, nil),
	})
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.conns, c.maxConns, c.acquires, c.emptyAcquires, c.canceled, c.acquireWaitSec} {
		ch <- d
	}
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.TotalConns()), "total")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquires, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.canceled, prometheus.CounterValue, float64(s.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.acquireWaitSec, prometheus.CounterValue, s.AcquireDuration().Seconds())
}

// ------------------------------------------------------------------ go-redis

type redisPoolCollector struct {
	client *redis.Client

	hits     *prometheus.Desc
	misses   *prometheus.Desc
	timeouts *prometheus.Desc
	conns    *prometheus.Desc // by state: total, idle
}

// RegisterRedisPool exports the go-redis client's pool statistics.
func (m *Metrics) RegisterRedisPool(client *redis.Client) {
	m.Registry.MustRegister(&redisPoolCollector{
		client: client,
		hits: prometheus.NewDesc("seatlock_redis_pool_hits_total",
			"Times a free pooled Redis connection was reused.", nil, nil),
		misses: prometheus.NewDesc("seatlock_redis_pool_misses_total",
			"Times no free connection was pooled (a new one was dialed).", nil, nil),
		timeouts: prometheus.NewDesc("seatlock_redis_pool_timeouts_total",
			"Times waiting for a pooled connection timed out.", nil, nil),
		conns: prometheus.NewDesc("seatlock_redis_pool_connections",
			"Redis pool connections by state (total, idle).", []string{"state"}, nil),
	})
}

func (c *redisPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.hits, c.misses, c.timeouts, c.conns} {
		ch <- d
	}
}

func (c *redisPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.client.PoolStats()
	ch <- prometheus.MustNewConstMetric(c.hits, prometheus.CounterValue, float64(s.Hits))
	ch <- prometheus.MustNewConstMetric(c.misses, prometheus.CounterValue, float64(s.Misses))
	ch <- prometheus.MustNewConstMetric(c.timeouts, prometheus.CounterValue, float64(s.Timeouts))
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.TotalConns), "total")
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.IdleConns), "idle")
}
