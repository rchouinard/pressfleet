package mariadb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// ServerStats are the platform-wide figures of the MariaDB server that the
// metrics collector (design doc §7.2) reports.
type ServerStats struct {
	// ThreadsConnected is the number of open connections.
	ThreadsConnected int64
	// MaxConnections is the server's connection limit.
	MaxConnections int64
	// SlowQueries is the cumulative count of queries slower than
	// long_query_time since the server started.
	SlowQueries int64
	// BufferPoolUsedBytes and BufferPoolSizeBytes are the InnoDB buffer pool
	// in use (pages that aren't free) and its total size.
	BufferPoolUsedBytes int64
	BufferPoolSizeBytes int64
}

// statsServer is the read-only monitoring side of the server. It is a
// separate interface from server so the fakes of the management tests
// don't need it; a server that doesn't have it makes the methods below
// return ErrStatsUnsupported.
type statsServer interface {
	serverStats(ctx context.Context) (ServerStats, error)
	// databaseSizes returns the data and index bytes of each site database,
	// by site ID. A site database without tables is present with size 0.
	databaseSizes(ctx context.Context) (map[string]int64, error)
}

// ErrStatsUnsupported means the manager's server can't report statistics.
// It only happens with test fakes.
var ErrStatsUnsupported = errors.New("mariadb: server statistics not supported")

// ServerStats returns the server's connection, slow query, and buffer pool
// figures, read through the admin connection the manager already has.
func (m *Manager) ServerStats(ctx context.Context) (ServerStats, error) {
	s, ok := m.server.(statsServer)
	if !ok {
		return ServerStats{}, ErrStatsUnsupported
	}
	return s.serverStats(ctx)
}

// DatabaseSizes returns the size in bytes (data plus indexes) of every
// site database, by site ID. One query covers all sites, so a collector
// doesn't ask the server once per site.
func (m *Manager) DatabaseSizes(ctx context.Context) (map[string]int64, error) {
	s, ok := m.server.(statsServer)
	if !ok {
		return nil, ErrStatsUnsupported
	}
	return s.databaseSizes(ctx)
}

var _ statsServer = sqlServer{}

// serverStats reads SHOW GLOBAL STATUS and SHOW GLOBAL VARIABLES, which any
// user can run. The buffer pool size comes from the page counters rather
// than innodb_buffer_pool_size, which can change at run time in chunks.
func (s sqlServer) serverStats(ctx context.Context) (ServerStats, error) {
	status, err := s.nameValues(ctx, "SHOW GLOBAL STATUS WHERE Variable_name IN "+
		"('Threads_connected','Slow_queries','Innodb_buffer_pool_pages_total','Innodb_buffer_pool_pages_free','Innodb_page_size')")
	if err != nil {
		return ServerStats{}, fmt.Errorf("reading server status: %w", err)
	}
	vars, err := s.nameValues(ctx, "SHOW GLOBAL VARIABLES WHERE Variable_name = 'max_connections'")
	if err != nil {
		return ServerStats{}, fmt.Errorf("reading server variables: %w", err)
	}
	return statsFrom(status, vars)
}

// statsFrom builds ServerStats from the raw status and variable maps.
func statsFrom(status, vars map[string]string) (ServerStats, error) {
	var firstErr error
	num := func(m map[string]string, name string) int64 {
		v, ok := m[name]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("server did not report %s", name)
			}
			return 0
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("server reported %s as %q", name, v)
		}
		return n
	}
	out := ServerStats{
		ThreadsConnected: num(status, "Threads_connected"),
		SlowQueries:      num(status, "Slow_queries"),
		MaxConnections:   num(vars, "max_connections"),
	}
	total, free, page := num(status, "Innodb_buffer_pool_pages_total"), num(status, "Innodb_buffer_pool_pages_free"), num(status, "Innodb_page_size")
	out.BufferPoolSizeBytes = total * page
	out.BufferPoolUsedBytes = (total - free) * page
	if firstErr != nil {
		return ServerStats{}, firstErr
	}
	return out, nil
}

// nameValues runs a SHOW query that returns Variable_name, Value rows. The
// statements are constants.
func (s sqlServer) nameValues(ctx context.Context, query string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// databaseSizes sums information_schema.tables per site database. The
// LEFT JOIN keeps empty databases, so a freshly created site reports 0
// rather than nothing. In LIKE, "\_" is a literal underscore. Names that
// aren't "pf_" plus a site ID are skipped.
func (s sqlServer) databaseSizes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.schema_name, CAST(COALESCE(SUM(t.data_length + t.index_length), 0) AS UNSIGNED)
FROM information_schema.schemata s
LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name
WHERE s.schema_name LIKE 'pf\_%'
GROUP BY s.schema_name`)
	if err != nil {
		return nil, fmt.Errorf("reading database sizes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var (
			name string
			size sql.NullInt64
		)
		if err := rows.Scan(&name, &size); err != nil {
			return nil, fmt.Errorf("reading database sizes: %w", err)
		}
		siteID, ok := strings.CutPrefix(name, namePrefix)
		if !ok || !id.Valid(siteID) {
			continue
		}
		out[siteID] = size.Int64
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading database sizes: %w", err)
	}
	return out, nil
}
