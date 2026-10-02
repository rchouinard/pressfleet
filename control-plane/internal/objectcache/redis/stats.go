package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/resp"
)

// ServerStats are the platform-wide figures of the cache server that the
// metrics collector (design doc §7.2) reports.
type ServerStats struct {
	// UsedMemoryBytes is the memory the server has allocated for data.
	UsedMemoryBytes int64
	// KeyspaceHits and KeyspaceMisses are cumulative lookup counts since
	// the server started.
	KeyspaceHits   int64
	KeyspaceMisses int64
	// EvictedKeys is the cumulative number of keys removed to stay under
	// maxmemory.
	EvictedKeys int64
}

// SiteUsage is what one site's logical database holds.
type SiteUsage struct {
	// Keys is the number of keys in the site's database.
	Keys int64
	// MemoryBytes estimates the memory the keys use, from a sample.
	MemoryBytes int64
}

// memorySampleSize is how many random keys of a site's database are
// measured with MEMORY USAGE to estimate its memory. The server has no
// per-database memory figure, and measuring every key would walk the whole
// keyspace of every site on every run. A sample of this size is cheap (one
// round trip each, on the platform network) and good enough for a trend.
const memorySampleSize = 32

// ErrStatsUnsupported means the manager's server can't report statistics.
// It only happens with test fakes.
var ErrStatsUnsupported = errors.New("redis: server statistics not supported")

// statsServer is the read-only monitoring side of the server; see the
// MariaDB manager for why it is a separate interface from server.
type statsServer interface {
	// info returns the reply of INFO.
	info(ctx context.Context) (string, error)
	// sampleMemory returns the average MEMORY USAGE, in bytes, of up to n
	// random keys of db, and how many keys it measured (0 for an empty
	// database).
	sampleMemory(ctx context.Context, db, n int) (avg float64, sampled int, err error)
}

// ServerStats returns the server's memory, hit, miss, and eviction
// figures, from INFO over the admin connection.
func (m *Manager) ServerStats(ctx context.Context) (ServerStats, error) {
	s, ok := m.server.(statsServer)
	if !ok {
		return ServerStats{}, ErrStatsUnsupported
	}
	info, err := s.info(ctx)
	if err != nil {
		return ServerStats{}, err
	}
	return parseServerStats(info)
}

// SiteUsage returns the key count and estimated memory of every site that
// has a cache user, by site ID. A site whose database can't be sampled is
// reported with its key count only, and its memory as 0.
//
// Hits and misses are not per site: the server counts them for the whole
// instance, and a database doesn't have counters of its own.
func (m *Manager) SiteUsage(ctx context.Context) (map[string]SiteUsage, error) {
	s, ok := m.server.(statsServer)
	if !ok {
		return nil, ErrStatsUnsupported
	}
	recs, err := listRecords(ctx, m.state)
	if err != nil {
		return nil, err
	}
	info, err := s.info(ctx)
	if err != nil {
		return nil, err
	}
	keys := parseKeyspace(info)
	out := make(map[string]SiteUsage, len(recs))
	for _, r := range recs {
		u := SiteUsage{Keys: keys[r.DB]}
		if u.Keys > 0 {
			avg, sampled, err := s.sampleMemory(ctx, r.DB, memorySampleSize)
			if err != nil {
				return nil, fmt.Errorf("sampling memory of database %d: %w", r.DB, err)
			}
			if sampled > 0 {
				u.MemoryBytes = int64(avg * float64(u.Keys))
			}
		}
		out[r.SiteID] = u
	}
	return out, nil
}

// parseServerStats reads the fields of INFO that ServerStats reports.
func parseServerStats(info string) (ServerStats, error) {
	var out ServerStats
	for _, f := range []struct {
		name string
		dst  *int64
	}{
		{"used_memory", &out.UsedMemoryBytes},
		{"keyspace_hits", &out.KeyspaceHits},
		{"keyspace_misses", &out.KeyspaceMisses},
		{"evicted_keys", &out.EvictedKeys},
	} {
		v := infoField(info, f.name)
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return ServerStats{}, fmt.Errorf("reading server info: %w: %s is %q", resp.ErrProtocol, f.name, v)
		}
		*f.dst = n
	}
	return out, nil
}

// parseKeyspace returns the key count of each database listed in INFO's
// keyspace section ("db3:keys=10,expires=0,avg_ttl=0"). Empty databases
// aren't listed.
func parseKeyspace(info string) map[int]int64 {
	out := map[int]int64{}
	for line := range strings.Lines(info) {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "db")
		if !ok {
			continue
		}
		num, fields, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		db, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(fields, ",") {
			if v, ok := strings.CutPrefix(kv, "keys="); ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					out[db] = n
				}
			}
		}
	}
	return out
}

var _ statsServer = respServer{}

func (s respServer) info(ctx context.Context) (string, error) {
	var info string
	err := s.session(ctx, func(c *resp.Conn) error {
		v, err := c.Do(ctx, "INFO")
		if err != nil {
			return fmt.Errorf("reading server info: %w", err)
		}
		var ok bool
		if info, ok = resp.String(v); !ok {
			return fmt.Errorf("reading server info: %w: unexpected reply", resp.ErrProtocol)
		}
		return nil
	})
	return info, err
}

// sampleMemory selects the site's database and measures n random keys.
// RANDOMKEY can return the same key twice; for an estimate that is fine.
// A key that expires between the two commands makes MEMORY USAGE reply
// nil, and is skipped.
func (s respServer) sampleMemory(ctx context.Context, db, n int) (float64, int, error) {
	if db <= 0 {
		return 0, 0, fmt.Errorf("sampling database %d: not a site database", db)
	}
	var total, sampled int64
	err := s.session(ctx, func(c *resp.Conn) error {
		if _, err := c.Do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
			return fmt.Errorf("selecting database %d: %w", db, err)
		}
		for range n {
			k, err := c.Do(ctx, "RANDOMKEY")
			if err != nil {
				return fmt.Errorf("choosing a key: %w", err)
			}
			key, ok := resp.String(k)
			if !ok {
				return nil // nil reply: the database is empty
			}
			m, err := c.Do(ctx, "MEMORY", "USAGE", key)
			if err != nil {
				return fmt.Errorf("measuring a key: %w", err)
			}
			if bytes, ok := m.(int64); ok {
				total += bytes
				sampled++
			}
		}
		return nil
	})
	if err != nil || sampled == 0 {
		return 0, 0, err
	}
	return float64(total) / float64(sampled), int(sampled), nil
}
