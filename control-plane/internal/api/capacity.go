package api

import (
	"context"
	"errors"
	"math"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Capacity thresholds (design doc §4.1) live here and nowhere else; clients
// show the level the API reports.
const (
	// capacityCautionRatio is the share of host memory the reservations may
	// reach before the level is "caution", leaving room to act first.
	capacityCautionRatio = 0.9
	// capacityExceededRatio is the share above which the level is
	// "exceeded": reservations must not exceed host memory.
	capacityExceededRatio = 1.0
)

const bytesPerMiB = 1024 * 1024

// GetPlatformCapacity compares the sites' memory reservations with the
// host's memory.
func (h *Handlers) GetPlatformCapacity(ctx context.Context, _ gen.GetPlatformCapacityRequestObject) (gen.GetPlatformCapacityResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	// A stopped site's containers are down and a deleting site is going
	// away. A failed site may be half-built, so it counts, which errs toward
	// warning early.
	reserved, counted, err := store.NewSites(h.db).MemoryReservations(ctx, store.SiteStopped, store.SiteDeleting)
	if err != nil {
		return nil, err
	}
	hostBytes, err := h.hostMemoryBytes(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetPlatformCapacity200JSONResponse(capacity(reserved, counted, hostBytes)), nil
}

// hostMemoryBytes returns the host's total memory from the metrics store, or
// 0 if it isn't known: no metrics store, or no collector data yet. The
// capacity is still useful then (the reservations are known), so that is not
// an error.
func (h *Handlers) hostMemoryBytes(ctx context.Context) (float64, error) {
	res, err := h.metrics.Summary(ctx, metricsapi.Request{Metrics: []string{"host_memory"}})
	if errors.Is(err, metrics.ErrUnavailable) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, r := range res.Results {
		for _, s := range r.Series {
			if s.Labels["type"] == "total" && len(s.Points) > 0 {
				return s.Points[len(s.Points)-1].Value, nil
			}
		}
	}
	return 0, nil
}

// capacity builds the response. hostBytes <= 0 (or not finite) means the
// host's memory is unknown.
func capacity(reservedMB int64, counted int, hostBytes float64) gen.PlatformCapacity {
	out := gen.PlatformCapacity{ReservedMemoryMb: reservedMB, CountedSites: counted, Level: "unknown"}
	if hostBytes <= 0 || math.IsNaN(hostBytes) || math.IsInf(hostBytes, 0) {
		return out
	}
	hostMB := hostBytes / bytesPerMiB
	ratio := float64(reservedMB) / hostMB
	out.HostMemoryMb, out.Ratio = &hostMB, &ratio
	switch {
	case ratio > capacityExceededRatio:
		out.Level = "exceeded"
	case ratio >= capacityCautionRatio:
		out.Level = "caution"
	default:
		out.Level = "ok"
	}
	return out
}
