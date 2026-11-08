package cli

import (
	"os"
	"path/filepath"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/driver"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
)

// hostStorageCollectors returns the host and storage collectors (§7.1
// Storage, §7.2 Host and Restic repositories).
//
// The procfs collector is for a platform on one host. On a cluster the
// driver reports its nodes instead, without disk metrics.
func (a *app) hostStorageCollectors(rt *driver.Runtime, repos *repository.Manager) []metrics.Collector {
	var host metrics.Collector = metrics.HostCollector{ProcDir: hostProcDir(), Mountpoints: []string{a.cfg.DataDir}}
	if rt.NodeStats != nil {
		host = metrics.NodeCollector{Nodes: rt.NodeStats}
	}
	return []metrics.Collector{
		host,
		metrics.WPContentCollector{Tasks: rt.Driver},
		metrics.RepositoryCollector{Restic: repos},
	}
}

// hostProcDir is the host's procfs: the mount the platform's Compose
// template provides when running in the control plane container, else the
// process's own /proc (development, or a control plane run on the host).
func hostProcDir() string {
	if _, err := os.Stat(filepath.Join(metrics.HostProcMount, "stat")); err == nil {
		return metrics.HostProcMount
	}
	return metrics.DefaultProcDir
}
