package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each series reads its own field of the pool statistics. Every field holds a
// value no other field holds, the fields no series reads included, so a
// series that reads the wrong field shows the wrong number.
func TestRegisterStorePool_readsEachSeriesFromItsOwnField(t *testing.T) {
	stats := &goredis.PoolStats{
		Hits: 101, Misses: 102, Timeouts: 103, WaitCount: 104, Unusable: 105, WaitDurationNs: 106,
		TotalConns: 107, IdleConns: 108, StaleConns: 109, PendingRequests: 110,
	}
	registry := prometheus.NewRegistry()

	RegisterStorePool(registry, func() *goredis.PoolStats { return stats })

	expected := `
# HELP ratelimit_store_pool_connections Open connections in the decision client's pool, in use and idle.
# TYPE ratelimit_store_pool_connections gauge
ratelimit_store_pool_connections 107
# HELP ratelimit_store_pool_hits_total Calls of the decision client that took an idle connection from its pool.
# TYPE ratelimit_store_pool_hits_total counter
ratelimit_store_pool_hits_total 101
# HELP ratelimit_store_pool_idle_connections Idle connections in the decision client's pool.
# TYPE ratelimit_store_pool_idle_connections gauge
ratelimit_store_pool_idle_connections 108
# HELP ratelimit_store_pool_misses_total Calls of the decision client that found no idle connection in its pool and waited for a dial.
# TYPE ratelimit_store_pool_misses_total counter
ratelimit_store_pool_misses_total 102
# HELP ratelimit_store_pool_removed_connections_total Connections the decision client closed and removed from its pool.
# TYPE ratelimit_store_pool_removed_connections_total counter
ratelimit_store_pool_removed_connections_total 109
# HELP ratelimit_store_pool_timeouts_total Calls of the decision client that waited for a free connection until the pool timeout.
# TYPE ratelimit_store_pool_timeouts_total counter
ratelimit_store_pool_timeouts_total 103
`
	assert.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(expected)))
}

// A scrape reads the pool once, when it runs: every series of the second
// scrape holds what the second reading returned.
func TestRegisterStorePool_readsThePoolOnceOnEveryScrape(t *testing.T) {
	var readings uint32
	registry := prometheus.NewRegistry()
	RegisterStorePool(registry, func() *goredis.PoolStats {
		readings++
		return &goredis.PoolStats{
			Hits: readings, Misses: readings, Timeouts: readings, StaleConns: readings,
			TotalConns: readings, IdleConns: readings,
		}
	})
	_, err := registry.Gather()
	require.NoError(t, err, "the first scrape")

	got := gatheredValues(t, registry)

	assert.Equal(t, map[string]float64{
		"ratelimit_store_pool_hits_total":                2,
		"ratelimit_store_pool_misses_total":              2,
		"ratelimit_store_pool_timeouts_total":            2,
		"ratelimit_store_pool_removed_connections_total": 2,
		"ratelimit_store_pool_connections":               2,
		"ratelimit_store_pool_idle_connections":          2,
	}, got)
}
