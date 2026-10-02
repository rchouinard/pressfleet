package redis

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const sampleInfo = "# Server\r\nrun_id:abc\r\n# Memory\r\nused_memory:1048576\r\n" +
	"# Stats\r\nkeyspace_hits:900\r\nkeyspace_misses:100\r\nevicted_keys:7\r\n" +
	"# Keyspace\r\ndb1:keys=10,expires=2,avg_ttl=0\r\ndb3:keys=4,expires=0,avg_ttl=0\r\n"

func TestParseServerStats(t *testing.T) {
	got, err := parseServerStats(sampleInfo)
	if err != nil {
		t.Fatal(err)
	}
	want := ServerStats{UsedMemoryBytes: 1048576, KeyspaceHits: 900, KeyspaceMisses: 100, EvictedKeys: 7}
	if got != want {
		t.Errorf("parseServerStats = %+v, want %+v", got, want)
	}
	if _, err := parseServerStats("# Memory\r\nused_memory:1\r\n"); err == nil {
		t.Error("missing fields: want an error")
	}
}

func TestParseKeyspace(t *testing.T) {
	got := parseKeyspace(sampleInfo)
	if len(got) != 2 || got[1] != 10 || got[3] != 4 {
		t.Errorf("parseKeyspace = %v", got)
	}
}

func TestStatsUnsupportedOnFake(t *testing.T) {
	m := newManager(newFakeServer(), nil, secrets.Store{}, 16, storetest.Logger())
	if _, err := m.ServerStats(context.Background()); !errors.Is(err, ErrStatsUnsupported) {
		t.Errorf("ServerStats error = %v, want ErrStatsUnsupported", err)
	}
	if _, err := m.SiteUsage(context.Background()); !errors.Is(err, ErrStatsUnsupported) {
		t.Errorf("SiteUsage error = %v, want ErrStatsUnsupported", err)
	}
}

func TestSiteUsage(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		base := newFakeServer()
		srv := &monitoredServer{fakeServer: base, infoText: sampleInfo, avg: 100, sampled: 5}
		m := newManager(srv, db, secrets.NewStore(db, secretstest.Keyring(t, 1)), 16, storetest.Logger())
		// Sites get databases 1 and 2; the info has keys in 1 and 3 only.
		for _, id := range []string{testSiteID, otherSiteID} {
			if _, err := m.CreateUser(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		got, err := m.SiteUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := (SiteUsage{Keys: 10, MemoryBytes: 1000}); got[testSiteID] != want {
			t.Errorf("site 1 usage = %+v, want %+v", got[testSiteID], want)
		}
		if want := (SiteUsage{}); got[otherSiteID] != want {
			t.Errorf("empty site usage = %+v, want %+v", got[otherSiteID], want)
		}
		if srv.sampledDBs[0] != 1 || len(srv.sampledDBs) != 1 {
			t.Errorf("sampled databases = %v, want only [1]: an empty database isn't sampled", srv.sampledDBs)
		}
		stats, err := m.ServerStats(ctx)
		if err != nil || stats.EvictedKeys != 7 {
			t.Errorf("ServerStats = %+v, %v", stats, err)
		}
	})
}

// monitoredServer is the fake server with the monitoring methods.
type monitoredServer struct {
	*fakeServer
	infoText   string
	avg        float64
	sampled    int
	sampledDBs []int
}

func (s *monitoredServer) info(context.Context) (string, error) { return s.infoText, nil }

func (s *monitoredServer) sampleMemory(_ context.Context, db, _ int) (float64, int, error) {
	s.sampledDBs = append(s.sampledDBs, db)
	return s.avg, s.sampled, nil
}
