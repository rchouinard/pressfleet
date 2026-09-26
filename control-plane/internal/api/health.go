package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// GetHealth reports liveness. It checks nothing: if the process can answer,
// it is alive.
func (h *Handlers) GetHealth(context.Context, gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	return gen.GetHealth200JSONResponse{Status: gen.HealthStatusOk}, nil
}

// GetReadiness runs the registered readiness checks.
func (h *Handlers) GetReadiness(ctx context.Context, _ gen.GetReadinessRequestObject) (gen.GetReadinessResponseObject, error) {
	results := h.readiness.Check(ctx)
	body := gen.Readiness{Status: gen.ReadinessStatusOk, Checks: make([]gen.ReadinessCheck, 0, len(results))}
	for _, res := range results {
		check := gen.ReadinessCheck{Name: res.Name, Status: gen.ReadinessCheckStatusOk}
		if res.Err != nil {
			msg := res.Err.Error()
			check.Status, check.Error = gen.ReadinessCheckStatusFailed, &msg
			body.Status = gen.ReadinessStatusUnavailable
			h.log.Warn("readiness check failed", "check", res.Name, "error", res.Err)
		}
		body.Checks = append(body.Checks, check)
	}
	if body.Status != gen.ReadinessStatusOk {
		return gen.GetReadiness503JSONResponse(body), nil
	}
	return gen.GetReadiness200JSONResponse(body), nil
}
