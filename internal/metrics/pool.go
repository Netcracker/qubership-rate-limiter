package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
)

var (
	descPoolHits = prometheus.NewDesc("ratelimit_store_pool_hits_total",
		"Calls of the decision client that took an idle connection from its pool.",
		nil, nil)
	descPoolMisses = prometheus.NewDesc("ratelimit_store_pool_misses_total",
		"Calls of the decision client that found no idle connection in its pool and waited for a dial.",
		nil, nil)
	descPoolTimeouts = prometheus.NewDesc("ratelimit_store_pool_timeouts_total",
		"Calls of the decision client that waited for a free connection until the pool timeout.",
		nil, nil)
	descPoolRemoved = prometheus.NewDesc("ratelimit_store_pool_removed_connections_total",
		"Connections the decision client closed and removed from its pool.",
		nil, nil)
	descPoolConnections = prometheus.NewDesc("ratelimit_store_pool_connections",
		"Open connections in the decision client's pool, in use and idle.",
		nil, nil)
	descPoolIdleConnections = prometheus.NewDesc("ratelimit_store_pool_idle_connections",
		"Idle connections in the decision client's pool.",
		nil, nil)
)

// poolCollector renders the connection pool of the client that decides the
// checks from one call of stats per scrape, so every series of a scrape comes
// from the same reading. go-redis keeps the counts in uint32, which
// wraps to 0 past 4294967295; Prometheus reads the wrap as a counter reset,
// and rate and increase lose the calls of at most one scrape interval.
type poolCollector struct {
	stats func() *goredis.PoolStats
}

func (poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPoolHits
	ch <- descPoolMisses
	ch <- descPoolTimeouts
	ch <- descPoolRemoved
	ch <- descPoolConnections
	ch <- descPoolIdleConnections
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.stats()
	ch <- prometheus.MustNewConstMetric(descPoolHits, prometheus.CounterValue, float64(stats.Hits))
	ch <- prometheus.MustNewConstMetric(descPoolMisses, prometheus.CounterValue, float64(stats.Misses))
	ch <- prometheus.MustNewConstMetric(descPoolTimeouts, prometheus.CounterValue, float64(stats.Timeouts))
	ch <- prometheus.MustNewConstMetric(descPoolRemoved, prometheus.CounterValue, float64(stats.StaleConns))
	ch <- prometheus.MustNewConstMetric(descPoolConnections, prometheus.GaugeValue, float64(stats.TotalConns))
	ch <- prometheus.MustNewConstMetric(descPoolIdleConnections, prometheus.GaugeValue, float64(stats.IdleConns))
}

// RegisterStorePool registers a collector that calls stats on every scrape and
// reports the decision client's connection pool; pass the client's PoolStats
// method. Call it once, at wiring time.
func RegisterStorePool(registry prometheus.Registerer, stats func() *goredis.PoolStats) {
	registry.MustRegister(poolCollector{stats: stats})
}
