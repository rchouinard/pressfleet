package cli

import (
	"os"
	"path/filepath"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// hostStorageCollectors returns the host and storage collectors (§7.1
// Storage, §7.2 Host and Restic repositories).
func (a *app) hostStorageCollectors(tasks provisioner.TaskRunner, repos *repository.Manager) []metrics.Collector {
	return []metrics.Collector{
		metrics.HostCollector{ProcDir: hostProcDir(), Mountpoints: []string{a.cfg.DataDir}},
		metrics.WPContentCollector{Tasks: tasks},
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
