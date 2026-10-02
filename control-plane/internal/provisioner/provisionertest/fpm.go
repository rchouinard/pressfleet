package provisionertest

import (
	"context"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// FPMStatuses is an in-memory [provisioner.FPMStatusReader]. It is separate
// from [Fake] because the interface is not part of provisioner.Driver. A
// site without a status is not found. It is safe for concurrent use.
type FPMStatuses struct {
	mu     sync.Mutex
	status map[string]provisioner.FPMStatus
	err    map[string]error
}

var _ provisioner.FPMStatusReader = (*FPMStatuses)(nil)

// Set makes FPMStatus return st for the site.
func (f *FPMStatuses) Set(siteID string, st provisioner.FPMStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[string]provisioner.FPMStatus{}
	}
	f.status[siteID] = st
}

// SetErr makes FPMStatus fail for the site with err.
func (f *FPMStatuses) SetErr(siteID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = map[string]error{}
	}
	f.err[siteID] = err
}

// FPMStatus implements provisioner.FPMStatusReader.
func (f *FPMStatuses) FPMStatus(_ context.Context, siteID string) (*provisioner.FPMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err[siteID]; err != nil {
		return nil, err
	}
	st, ok := f.status[siteID]
	if !ok {
		return nil, provisioner.ErrSiteNotFound
	}
	return &st, nil
}
