package mariadb

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

func TestStatsFrom(t *testing.T) {
	status := map[string]string{
		"Threads_connected":              "7",
		"Slow_queries":                   "42",
		"Innodb_buffer_pool_pages_total": "8192",
		"Innodb_buffer_pool_pages_free":  "192",
		"Innodb_page_size":               "16384",
	}
	vars := map[string]string{"max_connections": "151"}
	got, err := statsFrom(status, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := ServerStats{
		ThreadsConnected: 7, MaxConnections: 151, SlowQueries: 42,
		BufferPoolSizeBytes: 8192 * 16384, BufferPoolUsedBytes: 8000 * 16384,
	}
	if got != want {
		t.Errorf("statsFrom = %+v, want %+v", got, want)
	}

	delete(status, "Slow_queries")
	if _, err := statsFrom(status, vars); err == nil {
		t.Error("missing status: want an error")
	}
	status["Slow_queries"] = "x"
	if _, err := statsFrom(status, vars); err == nil {
		t.Error("malformed status: want an error")
	}
}

func TestStatsUnsupportedOnFake(t *testing.T) {
	m := newManager(newFakeServer(), nil, secrets.Store{}, 0, nil)
	if _, err := m.ServerStats(context.Background()); !errors.Is(err, ErrStatsUnsupported) {
		t.Errorf("ServerStats error = %v, want ErrStatsUnsupported", err)
	}
	if _, err := m.DatabaseSizes(context.Background()); !errors.Is(err, ErrStatsUnsupported) {
		t.Errorf("DatabaseSizes error = %v, want ErrStatsUnsupported", err)
	}
}
