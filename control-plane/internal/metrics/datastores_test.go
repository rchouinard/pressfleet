package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
)

type fakeMariaDB struct {
	stats    mariadb.ServerStats
	statsErr error
	sizes    map[string]int64
	sizesErr error
}

func (f fakeMariaDB) ServerStats(context.Context) (mariadb.ServerStats, error) {
	return f.stats, f.statsErr
}

func (f fakeMariaDB) DatabaseSizes(context.Context) (map[string]int64, error) {
	return f.sizes, f.sizesErr
}

type fakeRedis struct {
	stats    redis.ServerStats
	statsErr error
	usage    map[string]redis.SiteUsage
	usageErr error
}

func (f fakeRedis) ServerStats(context.Context) (redis.ServerStats, error) {
	return f.stats, f.statsErr
}

func (f fakeRedis) SiteUsage(context.Context) (map[string]redis.SiteUsage, error) {
	return f.usage, f.usageErr
}

func byName(samples []metrics.Sample) map[string][]metrics.Sample {
	m := map[string][]metrics.Sample{}
	for _, s := range samples {
		m[s.Name] = append(m[s.Name], s)
	}
	return m
}

var dsTargets = []metrics.Target{
	{SiteID: "s1", ClientGroupID: "g1"},
	{SiteID: "s2", ClientGroupID: "g1"},
}

func TestMariaDBCollector(t *testing.T) {
	c := metrics.MariaDBCollector{Source: fakeMariaDB{
		stats: mariadb.ServerStats{ThreadsConnected: 3, MaxConnections: 100, SlowQueries: 9, BufferPoolUsedBytes: 10, BufferPoolSizeBytes: 20},
		sizes: map[string]int64{"s1": 4096, "gone": 1},
	}}
	got, err := c.Collect(context.Background(), dsTargets)
	if err != nil {
		t.Fatal(err)
	}
	m := byName(got)
	for name, want := range map[string]float64{
		"pressfleet_mariadb_threads_connected":             3,
		"pressfleet_mariadb_max_connections":               100,
		"pressfleet_mariadb_slow_queries_total":            9,
		"pressfleet_mariadb_innodb_buffer_pool_used_bytes": 10,
		"pressfleet_mariadb_innodb_buffer_pool_size_bytes": 20,
	} {
		if len(m[name]) != 1 || m[name][0].Value != want {
			t.Errorf("%s = %v, want %v", name, m[name], want)
			continue
		}
		if _, ok := m[name][0].Labels[metrics.LabelSiteID]; ok {
			t.Errorf("%s: platform metric has a site_id", name)
		}
	}
	sizes := m["pressfleet_site_database_size_bytes"]
	if len(sizes) != 1 || sizes[0].Value != 4096 || sizes[0].Labels[metrics.LabelSiteID] != "s1" || sizes[0].Labels[metrics.LabelClientGroupID] != "g1" {
		t.Errorf("database sizes = %+v, want only s1 with 4096", sizes)
	}
}

func TestMariaDBCollectorPartialFailure(t *testing.T) {
	boom := errors.New("boom")
	c := metrics.MariaDBCollector{Source: fakeMariaDB{statsErr: boom, sizes: map[string]int64{"s2": 1}}}
	got, err := c.Collect(context.Background(), dsTargets)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
	if len(got) != 1 || got[0].Name != "pressfleet_site_database_size_bytes" {
		t.Errorf("samples = %+v, want the size alone", got)
	}
}

func TestRedisCollector(t *testing.T) {
	c := metrics.RedisCollector{Source: fakeRedis{
		stats: redis.ServerStats{UsedMemoryBytes: 5, KeyspaceHits: 6, KeyspaceMisses: 7, EvictedKeys: 8},
		usage: map[string]redis.SiteUsage{"s2": {Keys: 12, MemoryBytes: 3400}},
	}}
	got, err := c.Collect(context.Background(), dsTargets)
	if err != nil {
		t.Fatal(err)
	}
	m := byName(got)
	for name, want := range map[string]float64{
		"pressfleet_redis_memory_used_bytes":     5,
		"pressfleet_redis_keyspace_hits_total":   6,
		"pressfleet_redis_keyspace_misses_total": 7,
		"pressfleet_redis_evicted_keys_total":    8,
	} {
		if len(m[name]) != 1 || m[name][0].Value != want {
			t.Errorf("%s = %v, want %v", name, m[name], want)
		}
	}
	keys, mem := m["pressfleet_site_object_cache_keys"], m["pressfleet_site_object_cache_memory_bytes"]
	if len(keys) != 1 || keys[0].Value != 12 || keys[0].Labels[metrics.LabelSiteID] != "s2" {
		t.Errorf("keys = %+v", keys)
	}
	if len(mem) != 1 || mem[0].Value != 3400 {
		t.Errorf("memory = %+v", mem)
	}
}

func TestRedisCollectorPartialFailure(t *testing.T) {
	boom := errors.New("boom")
	c := metrics.RedisCollector{Source: fakeRedis{usageErr: boom, stats: redis.ServerStats{EvictedKeys: 1}}}
	got, err := c.Collect(context.Background(), dsTargets)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
	if len(got) != 4 {
		t.Errorf("samples = %d, want the 4 server metrics", len(got))
	}
}
