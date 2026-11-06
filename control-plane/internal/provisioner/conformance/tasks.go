package conformance

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// memoryHog allocates memory until the kernel kills it.
const memoryHog = `exec php -d memory_limit=-1 -r '$a = array(); for (;;) { $a[] = str_repeat( "x", 1 << 20 ); }'`

// connectScript prints "connected" or "refused" for an attempt to open a
// TCP connection to the host:port in $1, within five seconds, with the PHP
// CLI.
const connectScript = `php -r '$f = @fsockopen( parse_url( "tcp://" . $argv[1], PHP_URL_HOST ), parse_url( "tcp://" . $argv[1], PHP_URL_PORT ), $e, $s, 5 ); echo $f ? "connected" : "refused";' "$1"`

// connect runs the connection attempt in a task, and in a session, to
// see what each can reach.
type connector func(address string) string

// taskConnector connects from a site's task.
func (f *fixture) taskConnector(ctx context.Context, t *testing.T, siteID string) connector {
	return func(address string) string {
		t.Helper()
		res, err := f.d.RunTask(ctx, siteID, provisioner.Task{
			Image: f.phpImage(), Command: []string{"sh", "-c", connectScript, "sh", address}, Timeout: 2 * time.Minute,
		})
		if err != nil {
			t.Fatalf("task connecting to %s: %v", address, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
}

// secretsLeft lists the files of the data directory that hold the secret.
func secretsLeft(t *testing.T, layout config.Layout, secrets ...string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(layout.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return fs.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := readFile(path)
		if err != nil {
			return nil //nolint:nilerr // unreadable files are the task user's, and not ours to check
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				found = append(found, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// testTasks checks site tasks: what a task sees (wp-content in each mount
// mode, the transfer directory, environment, secrets, the site's user),
// that its output, exit code, timeout, memory limit, and cancellation are
// reported, that it works on a stopped site, and that it leaves nothing
// behind.
func testTasks(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	siteID := cfg.ID
	f.putFile(ctx, t, cfg, "/var/www/wordpress/wp-content/pf-task.txt", "hello-from-wp-content\n")

	transfer := f.transferDir(t)
	const secret = "pf-task-secret-Value-9" //nolint:gosec // G101: a test value
	run := func(ctx context.Context, mode provisioner.MountMode, script string) (*provisioner.TaskResult, error) {
		return f.d.RunTask(ctx, siteID, provisioner.Task{
			Image:       f.phpImage(),
			Command:     []string{"sh", "-c", script},
			Env:         map[string]string{"PF_PLAIN": "plain value"},
			SecretFiles: map[string]redact.Secret{"pf-secret": secret},
			WPContent:   mode,
			TransferDir: transfer,
			Timeout:     2 * time.Minute,
		})
	}

	// Read-only: reads wp-content, can't write it, shares the transfer
	// directory, and sees its environment and secrets.
	res, err := run(ctx, provisioner.MountReadOnly, `
		cat `+provisioner.TaskWPContentPath+`/pf-task.txt
		if touch `+provisioner.TaskWPContentPath+`/pf-task-write 2>/dev/null; then echo wp-content is writable; exit 1; fi
		cp `+provisioner.TaskWPContentPath+`/pf-task.txt `+provisioner.TaskTransferPath+`/copy.txt
		cat `+provisioner.TaskSecretPath("pf-secret")+` > `+provisioner.TaskTransferPath+`/secret.txt
		if [ -w `+provisioner.TaskSecretPath("pf-secret")+` ]; then echo the secret is writable; exit 1; fi
		echo "uid=$(id -u) gid=$(id -g) plain=$PF_PLAIN"
		echo to-stderr >&2`)
	if err != nil {
		t.Fatalf("read-only task: %v", err)
	}
	uid := strconv.Itoa(cfg.UID)
	want := "hello-from-wp-content\nuid=" + uid + " gid=" + uid + " plain=plain value\n"
	if res.Stdout != want || res.Stderr != "to-stderr\n" {
		t.Errorf("read-only task = %+v, want stdout %q", res, want)
	}
	if b, err := readFile(filepath.Join(transfer, "copy.txt")); err != nil || string(b) != "hello-from-wp-content\n" {
		t.Errorf("transfer copy = %q, %v", b, err)
	}
	if b, err := readFile(filepath.Join(transfer, "secret.txt")); err != nil || string(b) != secret {
		t.Errorf("secret in the task = %q, %v", b, err)
	}
	f.wantNoLeftovers(ctx, t, siteID, false)
	if found := secretsLeft(t, f.setup.Layout, secret); len(found) > 0 {
		// The task copied it into the transfer directory itself.
		for _, p := range found {
			if filepath.Dir(p) != transfer {
				t.Errorf("the secret file outlived its task: %s", p)
			}
		}
	}

	// Read-write: the site sees what the task writes, as its own.
	if _, err := run(ctx, provisioner.MountReadWrite, `echo restored > `+provisioner.TaskWPContentPath+`/pf-task-rw.txt`); err != nil {
		t.Fatalf("read-write task: %v", err)
	}
	if got := f.exec(ctx, t, cfg, provisioner.ComponentPHP, "sh", "-c",
		"cat /var/www/wordpress/wp-content/pf-task-rw.txt; stat -c %u /var/www/wordpress/wp-content/pf-task-rw.txt"); got != "restored\n"+uid {
		t.Errorf("site sees %q after a read-write task", got)
	}

	// No mount: no wp-content.
	if _, err := run(ctx, provisioner.MountNone, `test ! -e `+provisioner.TaskWPContentPath); err != nil {
		t.Errorf("wp-content visible without a mount: %v", err)
	}

	// A non-zero exit is an ExecError with the code and the output.
	_, err = run(ctx, provisioner.MountNone, `echo partial; echo oops >&2; exit 4`)
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 4 || ee.Stderr != "oops\n" {
		t.Errorf("failing task error = %v", err)
	}
	f.wantNoLeftovers(ctx, t, siteID, false)

	// The platform's services are what the site reaches the same way: a
	// task has no more network than the site's own.
	if addr := f.address(ctx, t, siteID); addr != "" {
		if got := f.taskConnector(ctx, t, siteID)(addr); got != "connected" {
			t.Errorf("a task reaching its own site's web server: %s, want connected", got)
		}
	}

	// The memory limit is the task's own, and the kernel enforces it.
	_, err = f.d.RunTask(ctx, siteID, provisioner.Task{
		Image: f.phpImage(), Command: []string{"sh", "-c", memoryHog}, MemoryLimitMB: config.MinBackupTaskMemoryMB, Timeout: 2 * time.Minute,
	})
	if err == nil {
		t.Error("a task over its memory limit succeeded")
	} else if errors.As(err, &ee) && (strings.Contains(ee.Stdout, "survived") || ee.ExitCode == 0) {
		t.Errorf("task over its memory limit: %v, want it killed", err)
	}
	f.wantNoLeftovers(ctx, t, siteID, false)

	// A timeout kills the task and says so.
	start := time.Now()
	_, err = f.d.RunTask(ctx, siteID, provisioner.Task{
		Image: f.phpImage(), Command: []string{"sleep", "300"}, Timeout: 3 * time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("task past its timeout: %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Errorf("a task with a 3 second timeout ran %s", elapsed)
	}
	f.wantNoLeftovers(ctx, t, siteID, false)

	// Canceling the context kills the task.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := run(runCtx, provisioner.MountReadOnly, `echo started > `+provisioner.TaskTransferPath+`/started; sleep 300`)
		done <- err
	}()
	eventually(t, 2*time.Minute, "the task starting", func() string {
		if _, err := os.Stat(filepath.Join(transfer, "started")); err != nil {
			return err.Error()
		}
		return ""
	})
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled task error = %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("canceled task didn't return")
	}
	f.wantNoLeftovers(ctx, t, siteID, false)

	t.Run("output", func(t *testing.T) {
		testTaskOutput(t, func(task provisioner.Task) (*provisioner.TaskResult, error) {
			task.Image = f.phpImage()
			return f.d.RunTask(ctx, siteID, task)
		})
	})

	// A stopped site's data is reachable too.
	if err := f.d.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if res, err := run(ctx, provisioner.MountReadOnly, `cat `+provisioner.TaskWPContentPath+`/pf-task.txt`); err != nil || res.Stdout != "hello-from-wp-content\n" {
		t.Errorf("task on a stopped site = %+v, %v", res, err)
	}
	f.wantNoLeftovers(ctx, t, siteID, false)
}

// testTaskOutput checks the streaming of a task's standard output.
func testTaskOutput(t *testing.T, run func(provisioner.Task) (*provisioner.TaskResult, error)) {
	t.Helper()

	// Output beyond what a result would hold arrives whole, and the result
	// holds none of it.
	var big bytes.Buffer
	res, err := run(provisioner.Task{Command: []string{"seq", "1", "400000"}, Stdout: &big})
	if err != nil {
		t.Fatalf("streaming task: %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("TaskResult.Stdout holds %d bytes although Task.Stdout was set", len(res.Stdout))
	}
	if lines := strings.Count(big.String(), "\n"); lines != 400000 || !strings.HasSuffix(big.String(), "\n400000\n") {
		t.Errorf("streamed %d lines (%d bytes), want 400000", lines, big.Len())
	}

	// It arrives while the task runs, not when it ends.
	w := &signalWriter{first: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := run(provisioner.Task{Command: []string{"sh", "-c", "echo first; sleep 10; echo second"}, Stdout: w})
		done <- err
	}()
	select {
	case <-w.first:
		select {
		case err := <-done:
			t.Errorf("the task ended (%v) before its first line was streamed", err)
		default:
		}
	case err := <-done:
		t.Fatalf("task ended without streaming (%v): %q", err, w.String())
	case <-time.After(time.Minute):
		t.Fatal("no output streamed within a minute")
	}
	if err := <-done; err != nil {
		t.Errorf("streaming task: %v", err)
	}
	if got := w.String(); got != "first\nsecond\n" {
		t.Errorf("streamed %q", got)
	}

	// A write error stops the task.
	start := time.Now()
	_, err = run(provisioner.Task{Command: []string{"yes"}, Stdout: &failingWriter{after: 1 << 16}, Timeout: 2 * time.Minute})
	if err == nil {
		t.Error("a task whose output can't be written succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Errorf("the task ran %s after its output failed", elapsed)
	}
}

type signalWriter struct {
	mu    sync.Mutex
	b     bytes.Buffer
	first chan struct{}
	once  sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.b.Write(p)
	w.mu.Unlock()
	w.once.Do(func() { close(w.first) })
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type failingWriter struct{ after, n int }

var errWriter = errors.New("conformance: the output can't be written")

func (w *failingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	if w.n > w.after {
		return 0, errWriter
	}
	return len(p), nil
}

// address returns the site's address for peers, or "" if the driver can't
// say, which skips the check that asked.
func (f *fixture) address(ctx context.Context, t *testing.T, siteID string) string {
	t.Helper()
	if f.inst.Address == nil {
		return ""
	}
	addr, err := f.inst.Address(ctx, siteID)
	if err != nil {
		t.Fatalf("the site's address: %v", err)
	}
	return addr
}

// testRepositoryTasks checks tasks that run against a backup repository
// with no site: the group's user instead of a site's, no wp-content, no
// network unless BackupNetwork, a host name of its own for each run, the
// state repository, and the same secrets, output, timeouts, and memory
// limit as site tasks.
func testRepositoryTasks(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	group := id.New()
	repo := f.repositoryDir(t)
	site := f.create(ctx, t)

	run := func(task provisioner.Task) (*provisioner.TaskResult, error) {
		task.Image = f.phpImage()
		if task.RepositoryDir == "" && !task.BackupNetwork {
			task.RepositoryDir = repo
		}
		if task.Timeout == 0 {
			task.Timeout = 3 * time.Minute
		}
		return f.d.RunRepositoryTask(ctx, group, task)
	}
	script := func(s string) string {
		t.Helper()
		res, err := run(provisioner.Task{Command: []string{"sh", "-c", s}})
		if err != nil {
			t.Fatalf("repository task %q: %v", s, err)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// The task is the repository's group, not root and not a site.
	gid := strconv.Itoa(provisioner.RepositoryGID)
	if got := script(`echo "$(id -u):$(id -g)"`); got != gid+":"+gid {
		t.Errorf("task user = %s, want %s:%s", got, gid, gid)
	}
	// It has the repository, and writes to it as the group.
	if got := script(`echo data > ` + provisioner.TaskRepositoryPath + `/pf-repo.txt && cat ` + provisioner.TaskRepositoryPath + `/pf-repo.txt`); got != "data" {
		t.Errorf("writing to the repository: %q", got)
	}
	if b, err := readFile(filepath.Join(repo, "pf-repo.txt")); err != nil || string(b) != "data\n" {
		t.Errorf("the repository on the host: %q, %v", b, err)
	}
	// No wp-content and no site files.
	if got := script(`test ! -e ` + provisioner.TaskWPContentPath + ` && test ! -e /site && echo ok`); got != "ok" {
		t.Errorf("filesystem checks = %q", got)
	}

	// Each run has a host name of its own, also runs at the same time:
	// restic records it in its locks.
	names := make([]string, 4)
	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := run(provisioner.Task{Command: []string{"sh", "-c", "hostname; sleep 5"}})
			if err != nil {
				t.Errorf("concurrent repository task #%d: %v", i, err)
				return
			}
			names[i] = strings.TrimSpace(res.Stdout)
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			t.Errorf("host names of concurrent repository tasks are not distinct and non-empty: %q", names)
			break
		}
		seen[n] = true
	}
	if a, b := script(`hostname`), script(`hostname`); a == b || a == "" {
		t.Errorf("host names %q and %q of two runs, want distinct names", a, b)
	}

	// The state repository is a repository like any other.
	stateDir := f.writeTransfer(t, "state.db", "state database")
	res, err := f.d.RunRepositoryTask(ctx, provisioner.StateRepository, provisioner.Task{
		Image: f.phpImage(), Command: []string{"cat", provisioner.TaskTransferPath + "/state.db"}, RepositoryDir: repo, TransferDir: stateDir,
	})
	if err != nil || res.Stdout != "state database" {
		t.Errorf("state repository task = %+v, %v", res, err)
	}

	// No network at all unless the repository is on S3: not the platform
	// (the database, the control plane), not the sites, not the internet.
	if addr := f.address(ctx, t, site.ID); addr != "" {
		if got := script(strings.ReplaceAll(connectScript, `"$1"`, addr)); got != "refused" {
			t.Errorf("a repository task reaching a site's web server at %s: %s, want refused", addr, got)
		}
	}
	for _, addr := range []string{"1.1.1.1:443", "1.1.1.1:53"} {
		if got := script(strings.ReplaceAll(connectScript, `"$1"`, addr)); got != "refused" {
			t.Errorf("a repository task reaching %s without BackupNetwork: %s, want refused", addr, got)
		}
	}
	// With BackupNetwork it has a network of its own, but still can't
	// reach a site.
	if addr := f.address(ctx, t, site.ID); addr != "" {
		res, err := run(provisioner.Task{
			Command:       []string{"sh", "-c", strings.ReplaceAll(connectScript, `"$1"`, addr)},
			BackupNetwork: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "refused" {
			t.Errorf("a repository task with BackupNetwork reaching a site's web server at %s: %s, want refused", addr, got)
		}
	}

	// Secrets are files, readable by the task, gone with it, and an
	// environment variable points at them.
	const secret = "pf-repo-task-secret-Value-4" //nolint:gosec // G101: a test value
	secretDir := f.writeTransfer(t, "want-secret", secret)
	if _, err := run(provisioner.Task{
		Command:       []string{"sh", "-c", `cmp "$PF_SECRET_FILE" ` + provisioner.TaskTransferPath + `/want-secret && touch ` + provisioner.TaskTransferPath + `/read-ok`},
		Env:           map[string]string{"PF_SECRET_FILE": provisioner.TaskSecretPath("pf-secret")},
		SecretFiles:   map[string]redact.Secret{"pf-secret": secret},
		RepositoryDir: repo,
		TransferDir:   secretDir,
	}); err != nil {
		t.Errorf("a repository task reading its secret file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, "read-ok")); err != nil {
		t.Errorf("the task did not find its secret file: %v", err)
	}
	for _, p := range secretsLeft(t, f.setup.Layout, secret) {
		if filepath.Base(p) != "want-secret" {
			t.Errorf("the secret file outlived its repository task: %s", p)
		}
	}

	// A non-zero exit is an ExecError.
	_, err = run(provisioner.Task{Command: []string{"sh", "-c", "echo oops >&2; exit 12"}})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 12 || ee.Stderr != "oops\n" {
		t.Errorf("failing repository task: %v, want exit code 12", err)
	}

	// The memory limit is enforced.
	_, err = run(provisioner.Task{Command: []string{"sh", "-c", memoryHog}, MemoryLimitMB: config.MinBackupTaskMemoryMB})
	if err == nil {
		t.Error("a repository task over its memory limit succeeded")
	} else if errors.As(err, &ee) && (strings.Contains(ee.Stdout, "survived") || ee.ExitCode == 0) {
		t.Errorf("repository task over its memory limit: %v, want it killed", err)
	}

	// A timeout and cancellation end it.
	_, err = run(provisioner.Task{Command: []string{"sleep", "300"}, Timeout: 3 * time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("repository task past its timeout: %v, want context.DeadlineExceeded", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	cancelDir := f.transferDir(t)
	go func() {
		_, err := f.d.RunRepositoryTask(runCtx, group, provisioner.Task{
			Image: f.phpImage(), Command: []string{"sh", "-c", "touch " + provisioner.TaskTransferPath + "/started; sleep 300"},
			RepositoryDir: repo, TransferDir: cancelDir,
		})
		done <- err
	}()
	eventually(t, 2*time.Minute, "the repository task starting", func() string {
		if _, err := os.Stat(filepath.Join(cancelDir, "started")); err != nil {
			return err.Error()
		}
		return ""
	})
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled repository task error = %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("canceled repository task didn't return")
	}
	f.wantNoLeftovers(ctx, t, group, false)

	t.Run("output", func(t *testing.T) {
		testTaskOutput(t, func(task provisioner.Task) (*provisioner.TaskResult, error) {
			task.Image = f.phpImage()
			task.RepositoryDir = repo
			return f.d.RunRepositoryTask(ctx, group, task)
		})
	})
}

// writeTransfer makes a transfer directory holding a file.
func (f *fixture) writeTransfer(t *testing.T, name, content string) string {
	t.Helper()
	dir := f.transferDir(t)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // G306: read by the task's user
		t.Fatal(err)
	}
	return dir
}

// readFile reads a file of the test's own directories.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: inside the test's data directory
}
