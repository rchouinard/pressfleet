package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestPlatformCapacityEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newMetricsEnv(t, db) // two sites, 128 MiB reserved each by default
		get := func() gen.PlatformCapacity {
			t.Helper()
			w := e.get(t, "/api/v1/platform/capacity")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			return decodeJSON[gen.PlatformCapacity](t, w.Body.Bytes())
		}
		host := func(mib float64) {
			e.q.Result = metrics.Result{Type: metrics.Vector, Series: []metrics.Series{{
				Labels: map[string]string{},
				Points: []metrics.Point{{Time: time.Now(), Value: mib * 1024 * 1024}},
			}}}
		}

		// No host memory data: reservations known, level unknown.
		got := get()
		if got.ReservedMemoryMb != 256 || got.CountedSites != 2 || got.Level != "unknown" || got.HostMemoryMb != nil || got.Ratio != nil {
			t.Fatalf("no host data = %+v", got)
		}

		for _, tc := range []struct {
			hostMiB float64
			level   gen.PlatformCapacityLevel
		}{
			{1024, "ok"}, {284, "caution"}, {256, "caution"}, {255, "exceeded"},
		} {
			host(tc.hostMiB)
			got = get()
			if got.Level != tc.level || got.HostMemoryMb == nil || *got.HostMemoryMb != tc.hostMiB || got.Ratio == nil || *got.Ratio != 256/tc.hostMiB {
				t.Errorf("host %v MiB = %+v, want level %s", tc.hostMiB, got, tc.level)
			}
		}

		// Stopped and deleting sites hold no reservation.
		sites := store.NewSites(db)
		for _, st := range []store.SiteStatus{store.SiteStopped, store.SiteDeleting} {
			if _, err := sites.SetStatus(ctx, e.a.ID, st, ""); err != nil {
				t.Fatal(err)
			}
			if got = get(); got.ReservedMemoryMb != 128 || got.CountedSites != 1 {
				t.Errorf("with a %s site = %+v", st, got)
			}
		}

		// An unreachable metrics store leaves the host's memory unknown.
		e.q.Err = metrics.ErrUnavailable
		if got = get(); got.Level != "unknown" || got.ReservedMemoryMb != 128 {
			t.Errorf("metrics unavailable = %+v", got)
		}
	})
}
