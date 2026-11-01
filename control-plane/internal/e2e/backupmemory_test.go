package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

const (
	// envBackupMemory turns on TestBackupMemory, which writes tens of GiB and
	// so isn't part of the regular suite.
	envBackupMemory = "PRESSFLEET_TEST_BACKUP_MEMORY"
	// backupMemoryJobTimeout bounds one job on the largest upload.
	backupMemoryJobTimeout = 30 * time.Minute
	// backupMemoryFile is the upload the test replaces at each size.
	backupMemoryFile = wpContent + "/uploads/pf-e2e-memory.bin"
)

// backupMemorySizesMiB are the upload sizes, smallest first. The first is the
// one seen killed (exit 137) on Docker Desktop. restic's memory shouldn't grow
// with a file's size, so a larger size failing where a smaller one passed says
// something too.
var backupMemorySizesMiB = []int{256, 1024, 4096}

// TestBackupMemory checks that a sync backup and the restore after it fit the
// default task memory limit (backup.task_memory_mb) with a large upload. A
// staging clone and then full refreshes copy a production site whose upload
// grows at each step, and the copy must arrive intact.
//
// A killed task doesn't fail its job at once: the job goes back to the queue,
// and a retry may pass. So the test fails at the first retry.
//
// The test logs each task's memory by kind, sampled from its cgroup. A task's
// page cache ("file") is reclaimed under pressure and normally sits near the
// limit without harm; what can't be reclaimed without swap is "anon" (restic's
// heap) and "shmem" (its tmpfs).
//
// See "Backup memory test" in docs/testing.md.
func TestBackupMemory(t *testing.T) {
	if os.Getenv(envBackupMemory) != "1" {
		t.Skipf("set %s=1 to run: it writes tens of GiB (make test-backup-memory)", envBackupMemory)
	}
	checkBackupMemoryHost(t)

	projectDir := t.TempDir()
	var projects []compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, proj := range projects {
			if res, err := proj.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", proj.Name)
				if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", proj.Name, err)
				}
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)
	e2e.EnsureStagingImages(t, images.PHP)

	groupID := createGroup(t, p, "E2E Backup Memory")
	created := createSite(t, p, groupID, "E2E Backup Memory", php)
	prod := compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
	projects = append(projects, prod)
	waitJob(t, p, created.Job.ID)

	watch := watchTaskMemory(t)
	watch.addSite(created.Site.ID)

	var stag compose.Project
	var stagID string
	for _, mib := range backupMemorySizesMiB {
		t.Logf("--- upload of %d MiB", mib)
		sum := siteSh(t, prod, fmt.Sprintf("head -c %dM /dev/urandom > %s && sha256sum %s", mib, backupMemoryFile, backupMemoryFile))

		var job client.Job
		if stagID == "" {
			resp, err := p.Client.CreateStagingSiteWithResponse(ctx, created.Site.ID, client.CreateStagingSiteJSONRequestBody{})
			if err != nil {
				t.Fatal(err)
			}
			if resp.JSON202 == nil {
				t.Fatalf("create staging site: status %d: %s", resp.StatusCode(), resp.Body)
			}
			stagID = resp.JSON202.Site.ID
			stag = compose.Project{Name: compose.SiteProjectName(stagID), Dir: projectDir}
			projects = append(projects, stag)
			watch.addSite(stagID)
			job = resp.JSON202.Job
		} else {
			resp, err := p.Client.RefreshStagingSiteWithResponse(ctx, stagID,
				client.RefreshStagingSiteJSONRequestBody{Files: client.StagingCopyFilesAll, Database: true})
			if err != nil {
				t.Fatal(err)
			}
			if resp.JSON202 == nil {
				t.Fatalf("refresh: status %d: %s", resp.StatusCode(), resp.Body)
			}
			job = resp.JSON202.Job
		}
		if job.AfterJobID == nil {
			t.Fatalf("%s job %s waits for no backup", job.Type, job.ID)
		}
		start := time.Now()
		waitBackupMemoryJob(t, p, watch, *job.AfterJobID)
		t.Logf("sync backup: %s", time.Since(start).Round(time.Second))
		start = time.Now()
		waitBackupMemoryJob(t, p, watch, job.ID)
		t.Logf("%s: %s", job.Type, time.Since(start).Round(time.Second))

		if got := siteSh(t, stag, "sha256sum "+backupMemoryFile); got != sum {
			t.Errorf("the %d MiB upload changed on the way: production %q, staging %q", mib, sum, got)
		}
		watch.report(t)
	}
	if watch.sampled() == 0 {
		t.Error("the memory watch saw no task: it needs cgroup v2, and its numbers above mean nothing without it")
	}
}

// waitBackupMemoryJob waits for a job to succeed, and fails the test as soon as
// it fails or goes back to the queue with an error, with what the watch saw.
func waitBackupMemoryJob(t *testing.T, p *e2e.Platform, watch *taskMemoryWatch, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), backupMemoryJobTimeout)
	defer cancel()
	for {
		resp, err := p.Client.GetJobWithResponse(ctx, jobID)
		if err != nil {
			watch.report(t)
			t.Fatalf("job %s: %v", jobID, err)
		}
		if resp.JSON200 == nil {
			watch.report(t)
			t.Fatalf("job %s: status %d: %s", jobID, resp.StatusCode(), resp.Body)
		}
		job := resp.JSON200
		switch {
		case job.State == client.JobStateSucceeded && job.Attempts <= 1:
			return
		case job.State == client.JobStateSucceeded,
			job.State == client.JobStateFailed,
			job.State == client.JobStateRollbackFailed,
			job.State == client.JobStateQueued && job.Error != nil:
			watch.report(t)
			t.Fatalf("%s job %s: %s after %d attempt(s): %s", job.Type, job.ID, job.State, job.Attempts, deref(job.Error))
		}
		select {
		case <-ctx.Done():
			watch.report(t)
			t.Fatalf("%s job %s still %s", job.Type, job.ID, job.State)
		case <-time.After(2 * time.Second):
		}
	}
}

// checkBackupMemoryHost logs what decides whether a run answers the question,
// and refuses a temporary directory on tmpfs. The harness keeps the backup
// repository below it, and tmpfs pages written by a task are charged to the
// task and can't be reclaimed without swap, so such a run could fail where a
// real install, with its repository on disk, wouldn't.
func checkBackupMemoryHost(t *testing.T) {
	t.Helper()
	dir := os.TempDir()
	fsType, err := mountFSType(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fsType == "tmpfs" {
		t.Fatalf("%s is on tmpfs, unlike a real repository: set TMPDIR to a directory on disk (make test-backup-memory uses /var/tmp)", dir)
	}
	t.Logf("temporary directory %s: %s", dir, fsType)
	out, err := compose.DockerExec(t.Context(), nil, "info", "--format",
		"{{.OperatingSystem}}, kernel {{.KernelVersion}}, cgroup v{{.CgroupVersion}} ({{.CgroupDriver}}), storage {{.Driver}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes of memory")
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	t.Logf("Docker host: %s", strings.TrimSpace(string(out)))
	if strings.Contains(string(out), "Docker Desktop") {
		t.Log("this is Docker Desktop: it runs in a VM, so this run doesn't stand in for native Linux")
	}
}

// mountFSType returns the file system type of the mount holding path.
func mountFSType(path string) (string, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	best, fsType := -1, ""
	for line := range strings.Lines(string(data)) {
		// "36 35 98:0 /root /mount/point rw - ext4 /dev/sda1 rw"
		before, after, ok := strings.Cut(line, " - ")
		fields, types := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(types) == 0 {
			continue
		}
		mount := fields[4]
		if path != mount && mount != "/" && !strings.HasPrefix(path, mount+"/") {
			continue
		}
		// >=: of two mounts on one point, the later one is on top.
		if len(mount) >= best {
			best, fsType = len(mount), types[0]
		}
	}
	if best < 0 {
		return "", fmt.Errorf("no mount holds %s", path)
	}
	return fsType, nil
}

// taskMemoryKeys are the cgroup values the report shows, in order.
var taskMemoryKeys = []string{"anon", "shmem", "file", "file_dirty", "file_writeback", "memory.peak"}

// taskMemoryWatch samples the memory of the sites' task containers from their
// cgroups while they run, keeping the highest value of each key.
type taskMemoryWatch struct {
	mu    sync.Mutex
	sites []string
	tasks map[string]*taskMemorySample // by container name
	seen  int
}

type taskMemorySample struct {
	samples int
	max     map[string]int64
}

// watchTaskMemory starts a watch that ends with the test.
func watchTaskMemory(t *testing.T) *taskMemoryWatch {
	t.Helper()
	w := &taskMemoryWatch{tasks: map[string]*taskMemorySample{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			w.sample(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return w
}

func (w *taskMemoryWatch) addSite(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sites = append(w.sites, id)
}

// sample reads every running task once. A task may end at any moment, so
// failing to read one means nothing.
func (w *taskMemoryWatch) sample(ctx context.Context) {
	w.mu.Lock()
	args := []string{"ps", "--format", "{{.Names}}"}
	for _, id := range w.sites {
		args = append(args, "--filter", "name=pf-"+id+"-task-")
	}
	empty := len(w.sites) == 0
	w.mu.Unlock()
	if empty {
		return
	}
	out, err := compose.DockerExec(ctx, nil, args...)
	if err != nil {
		return
	}
	const script = `cd /sys/fs/cgroup && for f in memory.max memory.peak; do echo "$f $(cat $f 2>/dev/null)"; done; cat memory.events memory.stat`
	for _, name := range strings.Fields(string(out)) {
		stats, err := compose.DockerExec(ctx, nil, "exec", name, "sh", "-c", script)
		if err != nil {
			continue
		}
		w.record(name, string(stats))
	}
}

// record keeps the highest of each value in one read of a task's cgroup.
func (w *taskMemoryWatch) record(name, stats string) {
	values := map[string]int64{}
	for line := range strings.Lines(stats) {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			// memory.max is the word "max" when there is no limit.
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				values[key] = n
			}
		}
	}
	if _, ok := values["anon"]; !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	task := w.tasks[name]
	if task == nil {
		task = &taskMemorySample{max: map[string]int64{}}
		w.tasks[name] = task
	}
	task.samples++
	w.seen++
	for key, n := range values {
		task.max[key] = max(task.max[key], n)
	}
}

// report logs the tasks seen since the last report, and forgets them.
func (w *taskMemoryWatch) report(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.tasks))
	for name := range w.tasks {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		task := w.tasks[name]
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %d samples, limit %d MiB;", name, task.samples, task.max["memory.max"]>>20)
		for _, key := range taskMemoryKeys {
			fmt.Fprintf(&b, " %s %d MiB", key, task.max[key]>>20)
		}
		fmt.Fprintf(&b, "; hit the limit %d times, OOM kills %d", task.max["max"], task.max["oom_kill"])
		t.Log(b.String())
	}
	if len(names) == 0 {
		t.Log("no task was sampled since the last report")
	}
	clear(w.tasks)
}

// sampled returns how many reads of a task's cgroup succeeded.
func (w *taskMemoryWatch) sampled() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}
