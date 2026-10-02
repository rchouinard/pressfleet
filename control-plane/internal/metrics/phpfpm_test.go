package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
)

func TestPHPFPMCollector(t *testing.T) {
	status := &provisionertest.FPMStatuses{}
	status.Set("s1", provisioner.FPMStatus{
		ActiveProcesses: 3, IdleProcesses: 2, MaxChildren: 8, MaxChildrenReached: 4, SlowRequests: 7,
	})
	status.Set("s2", provisioner.FPMStatus{ActiveProcesses: 1}) // limit unknown
	boom := errors.New("boom")
	status.SetErr("s3", boom)
	// s4 is not found: it is skipped without an error.
	c := metrics.PHPFPMCollector{Status: status}
	var targets []metrics.Target
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		targets = append(targets, metrics.Target{SiteID: id, ClientGroupID: "g"})
	}
	got, err := c.Collect(context.Background(), targets)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	have := map[string]map[string]float64{}
	for _, s := range got {
		site := s.Labels[metrics.LabelSiteID]
		if have[site] == nil {
			have[site] = map[string]float64{}
		}
		have[site][s.Name] = s.Value
		if s.Labels[metrics.LabelClientGroupID] != "g" {
			t.Errorf("sample %v lacks the client group", s)
		}
	}
	want1 := map[string]float64{
		metrics.MetricPHPFPMActive: 3, metrics.MetricPHPFPMIdle: 2, metrics.MetricPHPFPMMaxChildren: 8,
		metrics.MetricPHPFPMMaxChildrenReached: 4, metrics.MetricPHPFPMSlowRequests: 7,
	}
	for name, v := range want1 {
		if have["s1"][name] != v {
			t.Errorf("s1 %s = %v, want %v", name, have["s1"][name], v)
		}
	}
	if _, ok := have["s2"][metrics.MetricPHPFPMMaxChildren]; ok {
		t.Error("s2 reported a max_children of 0")
	}
	if len(have["s2"]) != 4 || len(have) != 2 {
		t.Errorf("samples = %v", have)
	}
}
