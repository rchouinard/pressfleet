package metrics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

type taskFunc func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error)

func (f taskFunc) RunTask(_ context.Context, siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	return f(siteID, task)
}

type resticFunc func(group string, args []string) ([]byte, error)

func (f resticFunc) Restic(_ context.Context, group string, args ...string) ([]byte, error) {
	return f(group, args)
}

func target(site, group string, status store.SiteStatus) Target {
	return Target{SiteID: site, ClientGroupID: group, Site: store.Site{ID: site, ClientGroupID: group, Status: status}}
}

func TestWPContentCollector(t *testing.T) {
	var ran []string
	c := WPContentCollector{Tasks: taskFunc(func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
		ran = append(ran, siteID)
		if task.WPContent != provisioner.MountReadOnly || !slices.Equal(task.Command, []string{"du", "-sk", provisioner.TaskWPContentPath}) {
			t.Errorf("task = %+v", task)
		}
		switch siteID {
		case "s1":
			return &provisioner.TaskResult{Stdout: "2048\t/site/wp-content\n"}, nil
		case "s2":
			return nil, errors.New("boom")
		case "s3":
			return &provisioner.TaskResult{Stdout: "garbage"}, nil
		}
		return &provisioner.TaskResult{Stdout: "1\t/site/wp-content\n"}, nil
	})}
	samples, err := c.Collect(context.Background(), []Target{
		target("s1", "g1", store.SiteRunning),
		target("s2", "g1", store.SiteRunning),
		target("s3", "g1", store.SiteRunning),
		target("s4", "g2", store.SiteStopped),
		target("s5", "g2", store.SiteProvisioning),
		target("s6", "g2", store.SiteFailed),
	})
	if err == nil {
		t.Error("want the failures of s2 and s3 reported")
	}
	if !slices.Equal(ran, []string{"s1", "s2", "s3", "s4"}) {
		t.Errorf("measured %v, want running and stopped sites only", ran)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %+v", samples)
	}
	s := samples[0]
	if s.Name != "pressfleet_site_wp_content_size_bytes" || s.Value != 2048*1024 ||
		s.Labels[LabelSiteID] != "s1" || s.Labels[LabelClientGroupID] != "g1" {
		t.Errorf("sample = %+v", s)
	}
	if samples[1].Labels[LabelSiteID] != "s4" || samples[1].Value != 1024 {
		t.Errorf("sample = %+v", samples[1])
	}
}

func TestWPContentCollectorStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := WPContentCollector{Tasks: taskFunc(func(string, provisioner.Task) (*provisioner.TaskResult, error) {
		calls++
		cancel()
		return &provisioner.TaskResult{Stdout: "1\tx"}, nil
	})}
	_, err := c.Collect(ctx, []Target{target("s1", "g", store.SiteRunning), target("s2", "g", store.SiteRunning)})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Errorf("calls = %d, err = %v", calls, err)
	}
}

func TestRepositoryCollector(t *testing.T) {
	var asked []string
	c := RepositoryCollector{Restic: resticFunc(func(group string, args []string) ([]byte, error) {
		asked = append(asked, group)
		if !slices.Equal(args, []string{"stats", "--mode", "raw-data", "--json"}) {
			t.Errorf("args = %v", args)
		}
		switch group {
		case "g1":
			return []byte(`{"total_size":5000,"total_file_count":3}`), nil
		case "g2":
			return nil, fmt.Errorf("group g2: %w", repository.ErrNoRepository)
		case "g3":
			return nil, errors.New("restic failed")
		}
		return []byte(`{}`), nil
	})}
	samples, err := c.Collect(context.Background(), []Target{
		target("s1", "g1", store.SiteRunning),
		target("s2", "g1", store.SiteRunning),
		target("s3", "g2", store.SiteRunning),
		target("s4", "g3", store.SiteRunning),
		target("s5", "g4", store.SiteRunning),
	})
	if err == nil {
		t.Error("want g3's failure and g4's bad output reported, but not g2's missing repository")
	}
	if !slices.Equal(asked, []string{"g1", "g2", "g3", "g4"}) {
		t.Errorf("asked %v, want each group once", asked)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %+v", samples)
	}
	s := samples[0]
	if s.Name != "pressfleet_restic_repository_size_bytes" || s.Value != 5000 ||
		s.Labels[LabelClientGroupID] != "g1" || s.Labels[LabelSiteID] != "" {
		t.Errorf("sample = %+v", s)
	}
}

func TestStorageCollectorIntervals(t *testing.T) {
	if got := (WPContentCollector{}).Interval(); got != DefaultWPContentInterval {
		t.Errorf("wp-content interval = %v", got)
	}
	if got := (RepositoryCollector{Every: time.Hour}).Interval(); got != time.Hour {
		t.Errorf("repository interval = %v", got)
	}
	if got := (RepositoryCollector{}).Interval(); got != DefaultRepositoryInterval {
		t.Errorf("repository interval = %v", got)
	}
}
