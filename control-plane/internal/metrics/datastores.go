package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
)

// The collectors below read the shared MariaDB and object cache servers
// through the managers that already hold admin access to them (design doc
// §7.1, §7.2), so they open no connections of their own. The sources are
// small interfaces, which the managers satisfy and tests fake.

// MariaDBSource is what [MariaDBCollector] reads; *mariadb.Manager
// implements it.
type MariaDBSource interface {
	ServerStats(ctx context.Context) (mariadb.ServerStats, error)
	// DatabaseSizes returns the size in bytes of each site database, by
	// site ID.
	DatabaseSizes(ctx context.Context) (map[string]int64, error)
}

// MariaDBCollector reports the platform-wide MariaDB metrics (connections,
// slow queries, buffer pool) and each site's database size.
//
//	pressfleet_mariadb_threads_connected             gauge
//	pressfleet_mariadb_max_connections               gauge
//	pressfleet_mariadb_slow_queries_total            counter
//	pressfleet_mariadb_innodb_buffer_pool_used_bytes gauge
//	pressfleet_mariadb_innodb_buffer_pool_size_bytes gauge
//	pressfleet_site_database_size_bytes              gauge, per site
//
// The two halves fail independently: if the server statistics can't be
// read, the sizes are still reported, and the other way round.
type MariaDBCollector struct {
	Source MariaDBSource
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = MariaDBCollector{}

// Name implements [Collector].
func (MariaDBCollector) Name() string { return "mariadb" }

// Interval implements [Collector].
func (c MariaDBCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c MariaDBCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var (
		out  []Sample
		errs []error
	)
	if st, err := c.Source.ServerStats(ctx); err != nil {
		errs = append(errs, err)
	} else {
		out = append(out,
			Sample{Name: "pressfleet_mariadb_threads_connected", Value: float64(st.ThreadsConnected)},
			Sample{Name: "pressfleet_mariadb_max_connections", Value: float64(st.MaxConnections)},
			Sample{Name: "pressfleet_mariadb_slow_queries_total", Value: float64(st.SlowQueries)},
			Sample{Name: "pressfleet_mariadb_innodb_buffer_pool_used_bytes", Value: float64(st.BufferPoolUsedBytes)},
			Sample{Name: "pressfleet_mariadb_innodb_buffer_pool_size_bytes", Value: float64(st.BufferPoolSizeBytes)},
		)
	}
	if sizes, err := c.Source.DatabaseSizes(ctx); err != nil {
		errs = append(errs, err)
	} else {
		for _, t := range targets {
			// A site without a database yet (still provisioning) has no
			// series rather than a zero one.
			if size, ok := sizes[t.SiteID]; ok {
				out = append(out, t.Sample("pressfleet_site_database_size_bytes", float64(size)))
			}
		}
	}
	return out, errors.Join(errs...)
}

// RedisSource is what [RedisCollector] reads; *redis.Manager implements
// it.
type RedisSource interface {
	ServerStats(ctx context.Context) (redis.ServerStats, error)
	// SiteUsage returns the key count and estimated memory of each site's
	// cache, by site ID.
	SiteUsage(ctx context.Context) (map[string]redis.SiteUsage, error)
}

// RedisCollector reports the platform-wide object cache server metrics and
// each site's keys and memory.
//
//	pressfleet_redis_memory_used_bytes         gauge
//	pressfleet_redis_keyspace_hits_total       counter
//	pressfleet_redis_keyspace_misses_total     counter
//	pressfleet_redis_evicted_keys_total        counter
//	pressfleet_site_object_cache_keys          gauge, per site
//	pressfleet_site_object_cache_memory_bytes  gauge, per site (estimated)
//
// There are no per-site hit and miss counters
// (pressfleet_site_object_cache_hits_total and _misses_total): the server
// counts lookups for the whole instance only, so the per-site hit ratio
// has no source. The platform-wide pressfleet_redis_keyspace_* counters
// cover the instance.
type RedisCollector struct {
	Source RedisSource
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = RedisCollector{}

// Name implements [Collector].
func (RedisCollector) Name() string { return "redis" }

// Interval implements [Collector].
func (c RedisCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c RedisCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var (
		out  []Sample
		errs []error
	)
	if st, err := c.Source.ServerStats(ctx); err != nil {
		errs = append(errs, err)
	} else {
		out = append(out,
			Sample{Name: "pressfleet_redis_memory_used_bytes", Value: float64(st.UsedMemoryBytes)},
			Sample{Name: "pressfleet_redis_keyspace_hits_total", Value: float64(st.KeyspaceHits)},
			Sample{Name: "pressfleet_redis_keyspace_misses_total", Value: float64(st.KeyspaceMisses)},
			Sample{Name: "pressfleet_redis_evicted_keys_total", Value: float64(st.EvictedKeys)},
		)
	}
	if usage, err := c.Source.SiteUsage(ctx); err != nil {
		errs = append(errs, err)
	} else {
		for _, t := range targets {
			if u, ok := usage[t.SiteID]; ok {
				out = append(out,
					t.Sample("pressfleet_site_object_cache_keys", float64(u.Keys)),
					t.Sample("pressfleet_site_object_cache_memory_bytes", float64(u.MemoryBytes)),
				)
			}
		}
	}
	return out, errors.Join(errs...)
}
