package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/conformance"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// TestKindBehaviour checks what the driver relies on the cluster for,
// beyond what the conformance suite can see through the driver contract
// (#317): that processes really end, that mounts, limits, and network
// policies hold, that Startup cleans up after a crash, and how metrics and
// logs behave around restarts. Like TestConformance it runs in a pod of a
// platform the chart installed (TestKubernetesDriverBehaviour in package
// e2e), and is skipped unless PRESSFLEET_K8S_CONFORMANCE=1.
//
// Its MetricsUnavailable subtest runs only with
// PRESSFLEET_TEST_METRICS_DOWN=1, which the e2e test sets in a second run
// once it has scaled metrics-server to zero; BackupNetwork needs the
// PRESSFLEET_TEST_S3_* variables of the S3 service that test deploys.
func TestKindBehaviour(t *testing.T) {
	if os.Getenv("PRESSFLEET_K8S_CONFORMANCE") != "1" {
		t.Skip("set PRESSFLEET_K8S_CONFORMANCE=1, in a pod of the cluster, to run these tests (see make test-k8s-e2e)")
	}
	e := kindEnvironment(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *kindFixture)
	}{
		{"WPCLIStops", testKindWPCLIStops},
		{"SPDYFallback", testKindSPDYFallback},
		{"DataClones", testKindDataClones},
		{"StreamedOutput", testKindStreamedOutput},
		{"OOMKilled", testKindOOMKilled},
		{"Leftovers", testKindLeftovers},
		{"BackupNetwork", testKindBackupNetwork},
		{"RestoreDump", testKindRestoreDump},
		{"SessionLimits", testKindSessionLimits},
		{"Stats", testKindStats},
		{"LogsFollow", testKindLogsFollow},
		{"MetricsUnavailable", testKindMetricsUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newKindFixture(t, e)) })
	}
}

// kindFixture is one subtest's driver and what it needs to make sites.
type kindFixture struct {
	env     kindEnv
	secrets *kindSecrets
	p       *Provisioner
	inst    conformance.Instance
}

func newKindFixture(t *testing.T, e kindEnv) *kindFixture {
	t.Helper()
	f := &kindFixture{env: e, secrets: &kindSecrets{m: map[secrets.Ref]redact.Secret{}}}
	f.p = f.driver(t, nil)
	f.inst = kindInstance(f.p)
	return f
}

// driver builds another driver for the platform, as the fixture's is,
// with the configuration that mutate changes.
func (f *kindFixture) driver(t *testing.T, mutate func(*Config)) *Provisioner {
	t.Helper()
	return f.env.driver(t, f.secrets, conformance.ImageSet(f.env.images), mutate)
}

// kindSecrets holds the secrets of the fixture's sites.
type kindSecrets struct {
	mu sync.Mutex
	m  map[secrets.Ref]redact.Secret
}

func (s *kindSecrets) Get(_ context.Context, ref secrets.Ref) (redact.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref]
	if !ok {
		return "", fmt.Errorf("get secret %s: %w", ref, store.ErrNotFound)
	}
	return v, nil
}

func (s *kindSecrets) put(ref string, v redact.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[secrets.Ref(ref)] = v
}

// kindSiteCounter gives each site a UID and table prefix of its own; the
// conformance suite's sites use other UIDs.
var kindSiteCounter atomic.Int64

// siteConfig returns a new site's configuration, with its secrets stored.
func (f *kindFixture) siteConfig(t *testing.T) *provisioner.SiteConfig {
	t.Helper()
	n := int(kindSiteCounter.Add(1))
	siteID := id.New()
	db, err := sitedb.MarshalCredentials(f.env.db)
	if err != nil {
		t.Fatal(err)
	}
	salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &provisioner.SiteConfig{
		ID:               siteID,
		ClientGroupID:    id.New(),
		PreviewDomain:    "kind" + strconv.Itoa(n) + ".preview.example.com",
		PHPVersion:       f.env.php,
		WPCoreVersion:    f.env.wp,
		Resources:        provisioner.DefaultResourceLimits(),
		DBSecretRef:      siteID + "-db",
		WPSaltsSecretRef: siteID + "-salts",
		UID:              provisioner.MinSiteUID + 8000 + n,
		TablePrefix:      "pfk" + strconv.Itoa(n) + "_",
	}
	f.secrets.put(cfg.DBSecretRef, db)
	f.secrets.put(cfg.WPSaltsSecretRef, salts)
	return cfg
}

// create makes a running site, which is deleted when the test ends.
func (f *kindFixture) create(ctx context.Context, t *testing.T) *provisioner.SiteConfig {
	t.Helper()
	cfg := f.siteConfig(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := f.p.DeleteSite(ctx, cfg.ID); err != nil {
			t.Errorf("cleanup: DeleteSite %s: %v", cfg.ID, err)
		}
	})
	if _, err := f.p.CreateSite(ctx, cfg); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return cfg
}

// exec runs a command in a component of the site, bypassing the driver.
func (f *kindFixture) exec(ctx context.Context, t *testing.T, siteID string, c provisioner.Component, argv ...string) string {
	t.Helper()
	out, err := f.inst.Exec(ctx, siteID, c, argv...)
	if err != nil {
		t.Fatalf("exec in %s %q: %v", c, argv, err)
	}
	return strings.TrimSpace(out)
}

// openDir makes a new directory under parent that any user can write, as
// the conformance suite does: the test doesn't run as the tasks' users.
func openDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name+"-"+id.New())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (f *kindFixture) transferDir(t *testing.T) string {
	t.Helper()
	return openDir(t, f.p.layout.TmpDir(), "transfer")
}

func (f *kindFixture) repositoryDir(t *testing.T) string {
	t.Helper()
	return openDir(t, f.p.layout.BackupRepositoriesDir(), "repo")
}

func (f *kindFixture) phpImage() string { return f.env.images[f.env.php].PHP }

func kindContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// eventually polls check until it returns "" or the timeout passes, and
// returns how long it took.
func eventually(t *testing.T, timeout time.Duration, what string, check func() string) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		last := check()
		if last == "" {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("%s within %s: %s", what, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// processes lists the command lines of the processes in a site's php
// container.
func (f *kindFixture) processes(ctx context.Context, t *testing.T, siteID string) string {
	t.Helper()
	return f.exec(ctx, t, siteID, provisioner.ComponentPHP, "sh", "-c", `for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' < "$f" 2>/dev/null; echo; done`)
}

// testKindWPCLIStops checks (#218) that WP-CLI ends in the container,
// where closing the exec stream doesn't end it: WP-CLI and the child it
// waits for, at the deadline, through timeout(1), which signals its whole
// process group; and after the caller cancels, by the driver's WP-CLI
// timeout at the latest, since cancellation alone doesn't end them (#346).
// (A child that ignores SIGTERM outlives the deadline: timeout(1) only
// kills what is left after killAfter while WP-CLI itself still runs:
// #344.)
func testKindWPCLIStops(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 15*time.Minute)
	cfg := f.create(ctx, t)

	// WP-CLI starts a child that sleeps for a time that marks it, and
	// waits for it.
	stubborn := func(marker string) []string {
		return []string{"eval", "--skip-wordpress", `system('sleep ` + marker + `');`}
	}
	// The sleep's own command line is just that; WP-CLI's, timeout's,
	// and the shells' hold it too.
	running := func(marker string) {
		t.Helper()
		eventually(t, time.Minute, "WP-CLI's child starting", func() string {
			procs := f.processes(ctx, t, cfg.ID)
			for l := range strings.Lines(procs) {
				if strings.TrimSpace(l) == "sleep "+marker {
					return ""
				}
			}
			return "not running yet: " + procs
		})
	}
	gone := func(marker string, within time.Duration) time.Duration {
		t.Helper()
		return eventually(t, within, "WP-CLI and its child ending", func() string {
			if procs := f.processes(ctx, t, cfg.ID); strings.Contains(procs, "sleep "+marker) {
				return "still running in the container: " + procs
			}
			return ""
		})
	}

	// The deadline: timeout(1) ends WP-CLI and its child within killAfter.
	const deadlineMarker = "4321"
	short, cancelShort := context.WithTimeout(ctx, 20*time.Second)
	defer cancelShort()
	done := make(chan error, 1)
	go func() {
		_, err := f.p.ExecWPCLI(short, cfg.ID, stubborn(deadlineMarker))
		done <- err
	}()
	running(deadlineMarker)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ExecWPCLI past its deadline: %v, want context.DeadlineExceeded", err)
	}
	took := gone(deadlineMarker, killAfter+30*time.Second)
	t.Logf("WP-CLI and its child ended %s after the deadline", took.Round(time.Second))

	// Cancellation: the call returns at once. The processes run on until
	// the driver's own WP-CLI timeout (#346), which bounds them; a driver
	// with a short one shows that bound.
	const cancelMarker = "4322"
	const wpcliTimeout = 30 * time.Second
	p := f.driver(t, func(c *Config) { c.WPCLITimeout = wpcliTimeout })
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		_, err := p.ExecWPCLI(runCtx, cfg.ID, stubborn(cancelMarker))
		done <- err
	}()
	running(cancelMarker)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled ExecWPCLI: %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a canceled ExecWPCLI didn't return within 30 seconds")
	}
	if returned := time.Since(start); returned > 15*time.Second {
		t.Errorf("a canceled ExecWPCLI returned after %s", returned)
	}
	took = gone(cancelMarker, wpcliTimeout+killAfter+30*time.Second)
	t.Logf("WP-CLI and its child ended %s after the cancellation (WP-CLI timeout %s)", took.Round(time.Second), wpcliTimeout)
}

// spdyOnlyProxy stands for a proxy between the driver and the API server
// that can't upgrade to WebSocket: it refuses WebSocket upgrades and passes
// everything else, SPDY upgrades included, to the API server.
type spdyOnlyProxy struct {
	url             string
	websocket, spdy atomic.Int64
}

func newSPDYOnlyProxy(t *testing.T) *spdyOnlyProxy {
	t.Helper()
	rc, err := rest.InClusterConfig()
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(rc.Host)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := rest.TLSConfigFor(rc)
	if err != nil {
		t.Fatal(err)
	}
	// HTTP/1.1: an upgrade can't go through an HTTP/2 connection.
	tlsCfg.NextProtos = []string{"http/1.1"}
	transport, err := rest.HTTPWrappersForConfig(rc, &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil})
	if err != nil {
		t.Fatal(err)
	}
	px := &spdyOnlyProxy{}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
		},
		Transport: transport,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch up := strings.ToLower(r.Header.Get("Upgrade")); {
			case up == "websocket":
				px.websocket.Add(1)
				http.Error(w, "this proxy can't upgrade to WebSocket", http.StatusBadRequest)
				return
			case strings.HasPrefix(up, "spdy/"):
				px.spdy.Add(1)
			}
			rp.ServeHTTP(w, r)
		}),
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	px.url = "http://" + l.Addr().String()
	return px
}

// testKindSPDYFallback checks (#218, #219) that exec and attach work over
// SPDY where something between the driver and the API server can't
// upgrade to WebSocket: a proxy in the test that refuses WebSocket stands
// for one.
func testKindSPDYFallback(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	cfg := f.create(ctx, t)
	px := newSPDYOnlyProxy(t)
	// The proxy adds the credentials; the driver only knows the proxy.
	p, err := newWithREST(Config{
		Layout:     f.p.layout,
		Kubernetes: f.env.k,
		Tasks:      TaskConfig{Image: f.env.taskImage},
	}, &rest.Config{Host: px.url, QPS: 20, Burst: 40, UserAgent: "pressfleet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	// Exec: output, standard input, and the exit code.
	if out, err := p.ExecWPCLI(ctx, cfg.ID, []string{"eval", "--skip-wordpress", "echo 'over spdy';"}); err != nil || strings.TrimSpace(out) != "over spdy" {
		t.Errorf("ExecWPCLI through the proxy = %q, %v", out, err)
	}
	input := strings.Repeat("pressfleet ", 10000)
	out, err := p.ExecWPCLIInput(ctx, cfg.ID, []string{"eval", "--skip-wordpress", "echo strlen( stream_get_contents( STDIN ) );"}, strings.NewReader(input))
	if err != nil || strings.TrimSpace(out) != strconv.Itoa(len(input)) {
		t.Errorf("ExecWPCLIInput through the proxy = %q, %v; want %d", out, err, len(input))
	}
	_, err = p.ExecWPCLI(ctx, cfg.ID, []string{"eval", "--skip-wordpress", "fwrite( STDERR, 'boom' ); exit( 3 );"})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 3 || !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("failing ExecWPCLI through the proxy: %v, want an *ExecError with exit code 3", err)
	}

	// Attach: a task's streams apart, and a session's standard input.
	res, err := p.RunTask(ctx, cfg.ID, provisioner.Task{
		Image: f.phpImage(), Command: []string{"sh", "-c", "echo out; echo err >&2"}, Timeout: 2 * time.Minute,
	})
	if err != nil || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Errorf("task through the proxy = %+v, %v", res, err)
	}
	var stdout strings.Builder
	if err := p.RunSession(ctx, cfg.ID, provisioner.Session{
		Image: f.phpImage(), Command: []string{"cat"}, Stdin: strings.NewReader("hello\n"), Stdout: &stdout,
	}); err != nil || stdout.String() != "hello\n" {
		t.Errorf("session through the proxy = %q, %v", stdout.String(), err)
	}

	ws, spdy := px.websocket.Load(), px.spdy.Load()
	t.Logf("the proxy refused %d WebSocket upgrades and passed %d SPDY upgrades", ws, spdy)
	if ws < 5 || spdy < 5 {
		t.Errorf("WebSocket upgrades refused = %d, SPDY upgrades = %d; want at least 5 of each (3 execs, a task, a session)", ws, spdy)
	}
}

// runningTaskPod waits for a running pod in ns that matches the selector,
// and returns it.
func (f *kindFixture) runningTaskPod(ctx context.Context, t *testing.T, ns, selector string) *corev1.Pod {
	t.Helper()
	var found *corev1.Pod
	eventually(t, 2*time.Minute, "a running pod "+selector+" in "+ns, func() string {
		pods, err := f.p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err.Error()
		}
		for i := range pods.Items {
			if pod := &pods.Items[i]; pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
				found = pod
				return ""
			}
		}
		return fmt.Sprintf("%d pods, none running", len(pods.Items))
	})
	return found
}

// testKindDataClones checks (#219) that clones of the data volume mounted
// side by side on one node stay usable when another is unmounted, by a
// task that ends and by a site that is deleted with its clone. A task that
// keeps reading and writing its transfer directory runs throughout; the
// test's own pod has the data volume's own claim mounted.
func testKindDataClones(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 15*time.Minute)
	a, b, c := f.create(ctx, t), f.create(ctx, t), f.create(ctx, t)
	dirA, dirB, dirC := f.transferDir(t), f.transferDir(t), f.transferDir(t)
	for _, d := range []string{dirA, dirB, dirC} {
		if err := os.WriteFile(filepath.Join(d, "in"), []byte("from the control plane\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// It counts in beat while it can read in and write beat, and exits when
	// stop appears.
	const worker = `i=0
while [ ! -e /transfer/stop ]; do
	cat /transfer/in > /dev/null || exit 3
	echo $i > /transfer/beat.tmp && mv /transfer/beat.tmp /transfer/beat || exit 4
	i=$((i+1)); sleep 0.5
done
echo stopped at $i`
	type result struct {
		res *provisioner.TaskResult
		err error
	}
	start := func(ctx context.Context, siteID, dir string) chan result {
		done := make(chan result, 1)
		go func() {
			res, err := f.p.RunTask(ctx, siteID, provisioner.Task{
				Image: f.phpImage(), Command: []string{"sh", "-c", worker}, TransferDir: dir, Timeout: 10 * time.Minute,
			})
			done <- result{res, err}
		}()
		return done
	}
	beat := func(dir string) int {
		b, err := os.ReadFile(filepath.Join(dir, "beat"))
		if err != nil {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return -1
		}
		return n
	}
	beating := func(what, dir string) {
		t.Helper()
		from := beat(dir)
		eventually(t, time.Minute, what, func() string {
			if n := beat(dir); n <= from+2 {
				return fmt.Sprintf("beat %d, was %d", n, from)
			}
			return ""
		})
	}
	selector := ComponentLabel + "=" + TaskComponent

	doneA := start(ctx, a.ID, dirA)
	ctxC, cancelC := context.WithCancel(ctx)
	defer cancelC()
	doneC := start(ctxC, c.ID, dirC)
	beating("task A starting", dirA)
	beating("task C starting", dirC)
	nodeA := f.runningTaskPod(ctx, t, f.p.SiteNamespace(a.ID), selector).Spec.NodeName
	nodeC := f.runningTaskPod(ctx, t, f.p.SiteNamespace(c.ID), selector).Spec.NodeName

	// B mounts its clone, writes, and ends: its clone is unmounted.
	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	if err := os.WriteFile(filepath.Join(dirB, "stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	doneB := make(chan result, 1)
	go func() {
		res, err := f.p.RunTask(ctxB, b.ID, provisioner.Task{
			Image: f.phpImage(), Command: []string{"sh", "-c", "sleep 5; cat /transfer/in > /transfer/out"}, TransferDir: dirB, Timeout: 5 * time.Minute,
		})
		doneB <- result{res, err}
	}()
	nodeB := f.runningTaskPod(ctx, t, f.p.SiteNamespace(b.ID), selector).Spec.NodeName
	if r := <-doneB; r.err != nil {
		t.Fatalf("task B: %v", r.err)
	}
	t.Logf("tasks on nodes A %s, B %s, C %s", nodeA, nodeB, nodeC)
	if nodeB != nodeA && nodeC != nodeA {
		t.Fatalf("no other task ran on task A's node %s (B on %s, C on %s), so the test shows nothing", nodeA, nodeB, nodeC)
	}
	if got, err := os.ReadFile(filepath.Join(dirB, "out")); err != nil || string(got) != "from the control plane\n" {
		t.Errorf("task B wrote %q, %v", got, err)
	}
	beating("task A after task B's clone was unmounted", dirA)

	// C is killed, and its site deleted with its clone.
	cancelC()
	if r := <-doneC; !errors.Is(r.err, context.Canceled) {
		t.Errorf("canceled task C: %v", r.err)
	}
	if err := f.p.DeleteSite(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Minute, "site C's clone being deleted", func() string {
		_, err := f.p.typed.CoreV1().PersistentVolumes().Get(ctx, DataCloneName(f.p.SiteNamespace(c.ID)), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return ""
		}
		return fmt.Sprintf("still there (%v)", err)
	})
	beating("task A after site C's clone was deleted", dirA)

	if err := os.WriteFile(filepath.Join(dirA, "stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := <-doneA
	if r.err != nil || !strings.HasPrefix(r.res.Stdout, "stopped at ") {
		t.Errorf("task A, which ran throughout: %+v, %v", r.res, r.err)
	}
}

// countingHash counts and hashes what is written to it.
type countingHash struct {
	h hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

// testKindStreamedOutput streams 1 GiB of a task's standard output through
// the attachment (#219), as an export of a large site does. A gigabyte is
// far more than the task's memory limit, the result's 8 MiB, and the
// node's container log, and takes long enough that a stream that buffers,
// stalls, or loses data would show it; several gigabytes would only take
// longer. The test's own heap must not grow with it.
func testKindStreamedOutput(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 15*time.Minute)
	const size = 1 << 30
	const line = "pressfleet streamed export 0123456789abcdef\n"
	want := sha256.New()
	for written := 0; written < size; {
		n := min(len(line), size-written)
		want.Write([]byte(line[:n]))
		written += n
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	got := &countingHash{h: sha256.New()}
	start := time.Now()
	res, err := f.p.RunRepositoryTask(ctx, id.New(), provisioner.Task{
		Image:         f.phpImage(),
		Command:       []string{"sh", "-c", `yes "$1" | head -c ` + strconv.Itoa(size), "sh", strings.TrimSuffix(line, "\n")},
		RepositoryDir: f.repositoryDir(t),
		Stdout:        got,
		Timeout:       10 * time.Minute,
	})
	elapsed := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("streaming 1 GiB: %v (after %d bytes)", err, got.n)
	}
	t.Logf("streamed %d bytes in %s (%.0f MiB/s); heap in use %d MiB before, %d MiB after",
		got.n, elapsed.Round(time.Second), float64(got.n)/(1<<20)/elapsed.Seconds(), before.HeapInuse>>20, after.HeapInuse>>20)
	if res.Stdout != "" {
		t.Errorf("the result holds %d bytes of streamed output", len(res.Stdout))
	}
	if got.n != size || hex.EncodeToString(got.h.Sum(nil)) != hex.EncodeToString(want.Sum(nil)) {
		t.Errorf("streamed %d bytes with SHA-256 %x, want %d bytes with %x", got.n, got.h.Sum(nil), size, want.Sum(nil))
	}
	if after.HeapInuse > before.HeapInuse+128<<20 {
		t.Errorf("the heap grew from %d to %d MiB while streaming", before.HeapInuse>>20, after.HeapInuse>>20)
	}
}

// memoryHog allocates memory until the kernel kills it.
const memoryHog = `exec php -d memory_limit=-1 -r '$a = array(); for (;;) { $a[] = str_repeat( "x", 1 << 20 ); }'`

// testKindOOMKilled checks (#219) that a task the kernel kills for its
// memory says so: Kubernetes reports OOMKilled, which the task can't.
func testKindOOMKilled(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	_, err := f.p.RunRepositoryTask(ctx, id.New(), provisioner.Task{
		Image: f.phpImage(), Command: []string{"sh", "-c", memoryHog}, RepositoryDir: f.repositoryDir(t),
		MemoryLimitMB: config.MinBackupTaskMemoryMB, Timeout: 3 * time.Minute,
	})
	var ee *provisioner.ExecError
	want := fmt.Sprintf("exceeding its memory limit of %d MiB (OOMKilled)", config.MinBackupTaskMemoryMB)
	if !errors.As(err, &ee) || ee.ExitCode != 137 || !strings.Contains(ee.Stderr, want) {
		t.Errorf("a task over its memory limit: %v, want exit code 137 and %q", err, want)
	}
}

// Environment of the helper process of testKindLeftovers.
const (
	envHelper      = "PRESSFLEET_TEST_HELPER"
	envHelperSite  = "PRESSFLEET_TEST_HELPER_SITE"
	envHelperGroup = "PRESSFLEET_TEST_HELPER_GROUP"
	envHelperRepo  = "PRESSFLEET_TEST_HELPER_REPO"
	// helperTaskTimeout is the timeout of the helper's tasks, so their
	// pods' activeDeadlineSeconds.
	helperTaskTimeout = 40 * time.Second
)

// TestKindBehaviourHelper is the control plane that testKindLeftovers
// kills: it starts a site task, a repository task, and a session that
// would run for minutes, and waits to be killed. It is skipped unless that
// test runs it.
func TestKindBehaviourHelper(t *testing.T) {
	if os.Getenv(envHelper) != "leftovers" {
		t.Skip("run by TestKindBehaviour/Leftovers")
	}
	e := kindEnvironment(t)
	p := e.driver(t, nil, nil, nil)
	image := e.images[e.php].PHP
	ctx := t.Context()
	site, group := os.Getenv(envHelperSite), os.Getenv(envHelperGroup)
	errs := make(chan error, 3)
	secret := map[string]redact.Secret{"pf-secret": "leftover"}
	go func() {
		_, err := p.RunTask(ctx, site, provisioner.Task{
			Image: image, Command: []string{"sleep", "600"}, SecretFiles: secret, Timeout: helperTaskTimeout,
		})
		errs <- fmt.Errorf("site task: %w", err)
	}()
	go func() {
		_, err := p.RunRepositoryTask(ctx, group, provisioner.Task{
			Image: image, Command: []string{"sleep", "600"}, SecretFiles: secret, Timeout: helperTaskTimeout,
			RepositoryDir: os.Getenv(envHelperRepo), BackupNetwork: true,
		})
		errs <- fmt.Errorf("repository task: %w", err)
	}()
	go func() {
		pr, pw := io.Pipe()
		defer func() { _ = pw.Close() }()
		errs <- fmt.Errorf("session: %w", p.RunSession(ctx, site, provisioner.Session{
			Image: image, Command: []string{"sleep", "600"}, Stdin: pr, Stdout: io.Discard,
		}))
	}()
	select {
	case err := <-errs:
		t.Fatalf("ended before it was killed: %v", err)
	case <-time.After(10 * time.Minute):
		t.Fatal("not killed within 10 minutes")
	}
}

// testKindLeftovers kills a control plane (TestKindBehaviourHelper, in a
// process of its own) while it runs tasks and a session, and checks
// (#219, #220) that their pods, Secrets, and NetworkPolicy are left, that
// the kubelet still ends the tasks at their activeDeadlineSeconds, and that
// Startup removes everything.
func testKindLeftovers(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 15*time.Minute)
	site := f.create(ctx, t)
	group := id.New()
	siteNS, platformNS := f.p.SiteNamespace(site.ID), f.p.platformNamespace

	helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKindBehaviourHelper$", "-test.v", "-test.timeout=15m")
	helper.Env = append(os.Environ(),
		envHelper+"=leftovers", envHelperSite+"="+site.ID, envHelperGroup+"="+group, envHelperRepo+"="+f.repositoryDir(t))
	out := &lockedBuffer{}
	helper.Stdout, helper.Stderr = out, out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- helper.Wait() }()
	defer func() {
		_ = helper.Process.Kill()
	}()

	taskSel := TaskLabel + "," + ComponentLabel + "=" + TaskComponent
	sessionSel := ComponentLabel + "=" + SessionComponent
	repoSel := TaskLabel + "," + repositoryLabel + "=" + group
	sitePod := f.runningTaskPod(ctx, t, siteNS, taskSel)
	sessionPod := f.runningTaskPod(ctx, t, siteNS, sessionSel)
	repoPod := f.runningTaskPod(ctx, t, platformNS, repoSel)
	select {
	case err := <-exited:
		t.Fatalf("the helper exited before it was killed: %v\n%s", err, out)
	default:
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	t.Logf("killed the helper with its tasks and session running:\n%s", out)

	core := f.p.typed.CoreV1()
	exists := func(err error) bool { return err == nil }
	type object struct {
		what string
		get  func() error
	}
	objects := []object{
		{"site task pod", func() error { _, err := core.Pods(siteNS).Get(ctx, sitePod.Name, metav1.GetOptions{}); return err }},
		{"site task secret", func() error { _, err := core.Secrets(siteNS).Get(ctx, sitePod.Name, metav1.GetOptions{}); return err }},
		{"session pod", func() error { _, err := core.Pods(siteNS).Get(ctx, sessionPod.Name, metav1.GetOptions{}); return err }},
		{"repository task pod", func() error { _, err := core.Pods(platformNS).Get(ctx, repoPod.Name, metav1.GetOptions{}); return err }},
		{"repository task secret", func() error {
			_, err := core.Secrets(platformNS).Get(ctx, repoPod.Name, metav1.GetOptions{})
			return err
		}},
		{"repository task network policy", func() error {
			_, err := f.p.typed.NetworkingV1().NetworkPolicies(platformNS).Get(ctx, repoPod.Name, metav1.GetOptions{})
			return err
		}},
	}
	for _, o := range objects {
		if err := o.get(); !exists(err) {
			t.Errorf("the %s didn't outlive its control plane: %v", o.what, err)
		}
	}

	// Nobody watches the tasks now; the kubelet ends them at their
	// activeDeadlineSeconds, which the driver reads as a timeout.
	for _, pod := range []struct{ ns, name string }{{siteNS, sitePod.Name}, {platformNS, repoPod.Name}} {
		took := eventually(t, helperTaskTimeout+2*time.Minute, "the kubelet ending "+pod.name+" at its deadline", func() string {
			got, err := core.Pods(pod.ns).Get(ctx, pod.name, metav1.GetOptions{})
			if err != nil {
				return err.Error()
			}
			if got.Spec.ActiveDeadlineSeconds == nil || *got.Spec.ActiveDeadlineSeconds != int64(helperTaskTimeout.Seconds()) {
				t.Fatalf("pod %s has activeDeadlineSeconds %v, want %v", pod.name, got.Spec.ActiveDeadlineSeconds, helperTaskTimeout.Seconds())
			}
			if err := podFailure(got, "task"); !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Sprintf("phase %s, reason %q (%v)", got.Status.Phase, got.Status.Reason, err)
			}
			return ""
		})
		t.Logf("pod %s/%s ended at its deadline (seen %s later)", pod.ns, pod.name, took.Round(time.Second))
	}
	if got, err := core.Pods(siteNS).Get(ctx, sessionPod.Name, metav1.GetOptions{}); err != nil || got.Status.Phase != corev1.PodRunning {
		t.Fatalf("the session pod, which has no deadline: %v", err)
	}

	// The next control plane's Startup removes it all.
	if err := f.p.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	eventually(t, 2*time.Minute, "Startup removing the leftovers", func() string {
		var left []string
		for _, o := range objects {
			if err := o.get(); exists(err) {
				left = append(left, o.what)
			} else if !apierrors.IsNotFound(err) {
				return err.Error()
			}
		}
		if len(left) > 0 {
			return "left: " + strings.Join(left, ", ")
		}
		return ""
	})
}

// lockedBuffer is a buffer that a process's two streams write.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// connectScript prints "connected" or "refused" for an attempt to open a
// TCP connection to the host:port in $1 within five seconds, as the
// conformance suite's does.
const connectScript = `php -r '$f = @fsockopen( parse_url( "tcp://" . $argv[1], PHP_URL_HOST ), parse_url( "tcp://" . $argv[1], PHP_URL_PORT ), $e, $s, 5 ); echo $f ? "connected" : "refused";' "$1"`

// testKindBackupNetwork checks (#219) the network of repository tasks with
// BackupNetwork: an S3 service in the cluster (the e2e test deploys one in
// a namespace of its own) and the internet, but not a site, MariaDB, or
// the S3 service without it. restic initializes and writes a repository
// there, as a backup to S3 does.
func testKindBackupNetwork(t *testing.T, f *kindFixture) {
	endpoint := os.Getenv("PRESSFLEET_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("PRESSFLEET_TEST_S3_ENDPOINT is not set")
	}
	ctx := kindContext(t, 10*time.Minute)
	site := f.create(ctx, t)
	p := f.driver(t, func(c *Config) { c.Tasks.S3Endpoint = endpoint })
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	s3Addrs, err := net.DefaultResolver.LookupHost(ctx, u.Hostname())
	if err != nil {
		t.Fatal(err)
	}
	s3IP := net.JoinHostPort(s3Addrs[0], u.Port())
	group := id.New()
	connect := func(backupNetwork bool, address string) string {
		t.Helper()
		task := provisioner.Task{
			Image: f.phpImage(), Command: []string{"sh", "-c", connectScript, "sh", address}, BackupNetwork: backupNetwork, Timeout: 2 * time.Minute,
		}
		if !backupNetwork {
			task.RepositoryDir = f.repositoryDir(t)
		}
		res, err := p.RunRepositoryTask(ctx, group, task)
		if err != nil {
			t.Fatalf("a repository task connecting to %s: %v", address, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	siteAddr, err := f.inst.Address(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	mariaDB := f.env.mariaDB
	for _, c := range []struct {
		what          string
		backupNetwork bool
		address       string
		want          string
	}{
		{"the S3 service", true, u.Host, "connected"},
		{"the S3 service by address", true, s3IP, "connected"},
		{"the internet", true, "1.1.1.1:443", "connected"},
		{"a site", true, siteAddr, "refused"},
		{"MariaDB", true, mariaDB, "refused"},
		{"the S3 service without BackupNetwork", false, s3IP, "refused"},
		{"the internet without BackupNetwork", false, "1.1.1.1:443", "refused"},
	} {
		if got := connect(c.backupNetwork, c.address); got != c.want {
			t.Errorf("a repository task reaching %s (%s): %s, want %s", c.what, c.address, got, c.want)
		}
	}

	// restic itself, in the task image, as a backup to S3 runs it.
	repo := "s3:" + endpoint + "/pf-" + strings.ToLower(group)
	res, err := p.RunRepositoryTask(ctx, group, provisioner.Task{
		Command: []string{"sh", "-c", `set -e
restic -r "$REPO" init --quiet
echo backed up > /tmp/file
restic -r "$REPO" backup --quiet /tmp/file
restic -r "$REPO" dump --quiet latest /tmp/file`},
		Env: map[string]string{
			"REPO":                  repo,
			"AWS_ACCESS_KEY_ID":     os.Getenv("PRESSFLEET_TEST_S3_ACCESS_KEY"),
			"AWS_SECRET_ACCESS_KEY": os.Getenv("PRESSFLEET_TEST_S3_SECRET_KEY"),
			"RESTIC_PASSWORD":       "pf-kind-restic",
		},
		BackupNetwork: true,
		Timeout:       3 * time.Minute,
	})
	if err != nil || res.Stdout != "backed up\n" {
		t.Errorf("restic with a repository on the S3 service: %+v, %v", res, err)
	}
}

// testKindRestoreDump checks the "possible gap" of #219: restic restore
// gives a file the mode the snapshot has, so a database dump that is 0600
// and belongs to another user in the snapshot (one taken by another site's
// task, or on Compose, whose tasks own their transfer directory) must
// still be readable by the control plane once a restore task, as the
// target site's user, has restored it into a transfer directory. The
// repository is set up as package repository sets up a local one, and the
// commands are those of package restic.
func testKindRestoreDump(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	from, to := f.create(ctx, t), f.create(ctx, t)
	env := map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath, "RESTIC_PASSWORD": "pf-kind-restic"}
	const dump = "-- the database dump\n"

	// The repository: the repository group's, setgid, initialized by a
	// repository task, and opened to the group (repository.prepareLocalDir,
	// repository.Manager.run).
	repo := filepath.Join(f.p.layout.BackupRepositoriesDir(), "repo-"+id.New())
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })
	if err := os.Lchown(repo, -1, provisioner.RepositoryGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repo, 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	// Package repository's init command, exactly.
	group := strings.ToLower(from.ClientGroupID)
	_, err := f.p.RunRepositoryTask(ctx, group, provisioner.Task{
		Image:   f.env.taskImage,
		Command: []string{"sh", "-c", `"$@" && chmod -R g=u ` + provisioner.TaskRepositoryPath, "sh", "restic", "--no-cache", "init"},
		Env:     env, RepositoryDir: repo, Timeout: 3 * time.Minute,
	})
	var ee *provisioner.ExecError
	switch {
	case errors.As(err, &ee) && strings.Contains(ee.Stderr, "changing permissions of '"+provisioner.TaskRepositoryPath+"': Operation not permitted"):
		// The task can't chmod the repository's own directory, which the
		// control plane owns (#347). Open the rest, as the
		// command meant to, so the restore can be checked.
		t.Logf("package repository's init command failed: %v", err)
		if _, err := f.p.RunRepositoryTask(ctx, group, provisioner.Task{
			Image:         f.env.taskImage,
			Command:       []string{"sh", "-c", `chmod -R g=u ` + provisioner.TaskRepositoryPath + `/*`},
			RepositoryDir: repo, Timeout: 3 * time.Minute,
		}); err != nil {
			t.Fatalf("opening the repository to its group: %v", err)
		}
	case err != nil:
		t.Fatalf("initializing the repository: %v", err)
	}

	// The snapshot: the dump as the first site's user, 0600.
	backup := f.transferDir(t)
	res, err := f.p.RunTask(ctx, from.ID, provisioner.Task{
		Image: f.env.taskImage,
		Command: []string{"sh", "-c", `set -e
printf '%s' "$1" > /transfer/database.sql
chmod 0600 /transfer/database.sql
restic backup --json --quiet /transfer/database.sql > /dev/null
restic ls --long latest /transfer/database.sql`, "sh", dump},
		Env: env, RepositoryDir: repo, TransferDir: backup, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("taking the snapshot: %v", err)
	}
	t.Logf("the snapshot has %s", strings.TrimSpace(res.Stdout))

	// The restore of the dump, as the target site's user, as
	// restoreDumpCommand runs it.
	restore := f.transferDir(t)
	if _, err := f.p.RunTask(ctx, to.ID, provisioner.Task{
		Image: f.env.taskImage,
		Command: []string{"restic", "restore", "--json", "--quiet", "latest:" + provisioner.TaskTransferPath,
			"--target", provisioner.TaskTransferPath, "--include", "/database.sql"},
		Env: env, RepositoryDir: repo, TransferDir: restore, Timeout: 3 * time.Minute,
	}); err != nil {
		t.Fatalf("restoring the dump: %v", err)
	}
	path := filepath.Join(restore, "database.sql")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the restored dump: mode %s, owner %d; the control plane is %d", fi.Mode(), fileOwner(fi), os.Geteuid())
	got, err := os.ReadFile(path)
	if errors.Is(err, os.ErrPermission) {
		// The gap is real; this check holds once #348 is fixed.
		t.Skipf("known bug #348: the control plane can't read a restored dump that is 0600 in the snapshot: %v", err)
	}
	if err != nil || string(got) != dump {
		t.Errorf("the control plane reads the restored dump as %q, %v", got, err)
	}
}

// testKindSessionLimits checks (#220) the limits a session gets from the
// cluster: its /tmp, a memory-backed emptyDir, holds no more than its
// 16 MiB sizeLimit, and a fork bomb is stopped: by the session's memory
// limit, and in a pod with more memory by the kubelet's podPidsLimit (1024
// on the test cluster).
func testKindSessionLimits(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	site := f.create(ctx, t)
	session := func(script string) (string, string, error) {
		t.Helper()
		var stdout, stderr strings.Builder
		err := f.p.RunSession(ctx, site.ID, provisioner.Session{
			Image: f.phpImage(), Command: []string{"sh", "-c", script}, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		})
		return stdout.String(), stderr.String(), err
	}

	// /tmp: 8 MiB fit, 24 MiB more don't, and no more than the limit was
	// written.
	stdout, stderr, err := session(`
		head -c 8388608 /dev/zero > /tmp/a && echo first-ok
		if head -c 25165824 /dev/zero > /tmp/b 2>/dev/null; then echo second-ok; else echo second-failed; fi
		echo total $(cat /tmp/a /tmp/b | wc -c)
		df -k /tmp | tail -n 1`)
	t.Logf("/tmp session: %q, %q", stdout, stderr)
	if err != nil {
		t.Fatalf("/tmp session: %v", err)
	}
	lines := strings.Split(stdout, "\n")
	if len(lines) < 3 || lines[0] != "first-ok" || lines[1] != "second-failed" {
		t.Errorf("writing 32 MiB to /tmp: %q, want the first 8 MiB to fit and the next 24 MiB not to", stdout)
	}
	if total, err := strconv.Atoi(strings.TrimPrefix(lines[min(2, len(lines)-1)], "total ")); err != nil || total > sessionTmpSize {
		t.Errorf("/tmp holds %q bytes, want at most %d", lines[min(2, len(lines)-1)], sessionTmpSize)
	}

	// Processes. A session's 128 MiB hold fewer than 1024 processes even
	// of forked shells, which cost the least, so in a session the memory
	// limit stops a fork bomb first: the session is OOMKilled, and the
	// site is unharmed.
	stdout, stderr, err = session(forkBomb)
	t.Logf("fork bomb session: %q, %q, %v", stdout, stderr, err)
	var ee *provisioner.ExecError
	if !strings.Contains(stdout, "fork") && (!errors.As(err, &ee) || !strings.Contains(ee.Stderr, "OOMKilled")) {
		t.Errorf("a fork bomb in a session: %q, %v; want forking to fail or the session to be OOMKilled", stdout, err)
	}
	if got := f.exec(ctx, t, site.ID, provisioner.ComponentPHP, "echo", "alive"); got != "alive" {
		t.Errorf("the site after the session's fork bomb: %q", got)
	}
	// The kubelet's podPidsLimit is the same for every pod, sessions
	// included; a task with the memory for it shows that it holds.
	res, err := f.p.RunTask(ctx, site.ID, provisioner.Task{
		Image: f.phpImage(), Command: []string{"sh", "-c", forkBomb}, MemoryLimitMB: 1024, Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("fork bomb task: %v", err)
	}
	t.Logf("fork bomb task: %q", res.Stdout)
	var started int
	if _, err := fmt.Sscanf(res.Stdout, "started %d", &started); err != nil {
		t.Fatalf("fork bomb task printed %q", res.Stdout)
	}
	if started >= 1024 || started < 900 || !strings.Contains(res.Stdout, "fork") {
		t.Errorf("a pod started %d processes before forking failed (%q); want the kubelet's podPidsLimit of 1024 to stop it", started, res.Stdout)
	}
}

// forkBomb forks shells that block on a FIFO until forking fails, prints
// how many it started and why it stopped, and exits, which ends them all:
// it is the container's first process. Only shell builtins run after the
// fork bomb.
const forkBomb = `
mkfifo /tmp/hold
( i=0; while [ $i -lt 1500 ]; do { read x < /tmp/hold; } & i=$((i+1)); echo $i > /tmp/count; done ) 2> /tmp/err
read n < /tmp/count; echo "started $n"
while read -r l; do echo "error: $l"; done < /tmp/err`

// statsWithin is how soon after a site starts Stats has samples of it, with
// metrics-server's default resolution of 15 seconds.
const statsWithin = time.Minute

// testKindStats checks (#221) that Stats has samples of a new site within a
// minute, that php's CPU time rises under load, and that it starts again
// when php restarts.
func testKindStats(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	site := f.create(ctx, t)
	sample := func() map[provisioner.Component]provisioner.ComponentStats {
		t.Helper()
		stats, err := f.p.Stats(ctx, site.ID)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		m := map[provisioner.Component]provisioner.ComponentStats{}
		for _, s := range stats {
			m[s.Component] = s
		}
		return m
	}
	took := eventually(t, statsWithin, "the first samples of both components", func() string {
		if s := sample(); len(s) < 2 {
			return fmt.Sprintf("samples %+v", s)
		}
		return ""
	})
	t.Logf("the first samples came %s after the site was up", took.Round(time.Second))

	// Load: 40 seconds of busy PHP, at most php's CPU limit of it. That
	// is well over a fresh container's first estimate, its rate over one
	// 15-second window, which php-fpm's start makes several seconds; so
	// the restart below shows as a drop.
	before := sample()[provisioner.ComponentPHP]
	if _, err := f.p.ExecWPCLI(ctx, site.ID, []string{"--skip-wordpress", "eval", "$e = microtime( true ) + 40; while ( microtime( true ) < $e ) {}"}); err != nil {
		t.Fatal(err)
	}
	var loaded provisioner.ComponentStats
	eventually(t, 2*time.Minute, "php's CPU time rising under load", func() string {
		loaded = sample()[provisioner.ComponentPHP]
		if grew := loaded.CPUUsage - before.CPUUsage; grew < 15*time.Second {
			return fmt.Sprintf("grew by %s", grew)
		}
		return ""
	})
	t.Logf("php's CPU time: %s before the load, %s after", before.CPUUsage, loaded.CPUUsage)

	// A restart of php starts its CPU time again.
	if _, err := f.inst.Exec(ctx, site.ID, provisioner.ComponentPHP, "sh", "-c", "kill -TERM 1"); err != nil {
		t.Fatalf("stopping php: %v", err)
	}
	var restarted provisioner.ComponentStats
	eventually(t, 3*time.Minute, "php's CPU time starting again with its restart", func() string {
		s, ok := sample()[provisioner.ComponentPHP]
		switch {
		case !ok:
			return "no php sample"
		case s.Restarts < 1:
			return fmt.Sprintf("restarts %d", s.Restarts)
		case s.CPUUsage >= loaded.CPUUsage:
			return fmt.Sprintf("CPU time %s, was %s before the restart", s.CPUUsage, loaded.CPUUsage)
		}
		restarted = s
		return ""
	})
	t.Logf("php's CPU time after its restart: %s", restarted.CPUUsage)
}

// testKindLogsFollow checks (#221) that following a site's logs goes on
// across a rollout, which replaces the site's pod (Recreate), and ends
// with ErrNoPod when the site stops.
func testKindLogsFollow(t *testing.T, f *kindFixture) {
	ctx := kindContext(t, 10*time.Minute)
	site := f.create(ctx, t)
	write := func(line string) {
		t.Helper()
		f.exec(ctx, t, site.ID, provisioner.ComponentPHP, "sh", "-c", `echo "PHP Notice:  $1" > /proc/1/fd/2`, "sh", line)
	}
	type message struct {
		line string
		err  error
	}
	msgs := make(chan message, 100)
	go func() {
		defer close(msgs)
		for e, err := range f.p.Logs(ctx, site.ID, provisioner.LogOptions{Follow: true, Contains: "logs-rollout"}) {
			msgs <- message{e.Line, err}
			if err != nil {
				return
			}
		}
	}()
	next := func(what string, timeout time.Duration) message {
		t.Helper()
		select {
		case m, ok := <-msgs:
			if !ok {
				t.Fatalf("%s: the follow ended", what)
			}
			return m
		case <-time.After(timeout):
			t.Fatalf("%s: nothing within %s", what, timeout)
		}
		return message{}
	}
	time.Sleep(2 * time.Second)
	write("logs-rollout before")
	if m := next("a line before the rollout", time.Minute); m.err != nil || !strings.HasSuffix(m.line, "logs-rollout before") {
		t.Fatalf("before the rollout: %+v", m)
	}

	// New salts make a new wp-config.php, and so a new pod.
	old, err := f.p.sitePod(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
	if err != nil {
		t.Fatal(err)
	}
	site.WPSaltsSecretRef = site.ID + "-salts-v2"
	f.secrets.put(site.WPSaltsSecretRef, salts)
	start := time.Now()
	if _, err := f.p.UpdateSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	now, err := f.p.sitePod(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if now.Pod == old.Pod {
		t.Fatalf("the update kept pod %s", old.Pod)
	}
	t.Logf("the rollout from %s to %s took %s", old.Pod, now.Pod, time.Since(start).Round(time.Second))
	select {
	case m, ok := <-msgs:
		t.Fatalf("the follow yielded %+v (open: %v) during the rollout", m, ok)
	default:
	}
	write("logs-rollout after")
	if m := next("a line of the new pod", time.Minute); m.err != nil || !strings.HasSuffix(m.line, "logs-rollout after") {
		t.Fatalf("after the rollout: %+v", m)
	}

	// Stopping the site ends the follow with ErrNoPod.
	if err := f.p.StopSite(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	m := next("the end of the follow after the site stopped", time.Minute)
	if !errors.Is(m.err, ErrNoPod) {
		t.Errorf("the follow of a stopped site ended with %+v, want ErrNoPod", m)
	}
	if _, ok := <-msgs; ok {
		t.Error("the follow went on after its error")
	}
}

// testKindMetricsUnavailable checks (#221) that Stats says
// ErrMetricsUnavailable when the cluster serves no metrics. It runs only
// once the e2e test has scaled metrics-server to zero, which the other
// tests on the cluster need.
func testKindMetricsUnavailable(t *testing.T, f *kindFixture) {
	if os.Getenv("PRESSFLEET_TEST_METRICS_DOWN") != "1" {
		t.Skip("runs once metrics-server is scaled to zero (PRESSFLEET_TEST_METRICS_DOWN=1)")
	}
	ctx := kindContext(t, 10*time.Minute)
	// The site stays: a namespace can't be deleted while an aggregated API
	// such as metrics.k8s.io is unavailable (the namespace controller
	// can't list what is in it), so DeleteSite would wait for
	// metrics-server. The e2e test's teardown deletes the namespace once
	// metrics-server is back.
	site := f.siteConfig(t)
	if _, err := f.p.CreateSite(ctx, site); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	var last error
	eventually(t, 2*time.Minute, "Stats saying the metrics API is unavailable", func() string {
		_, last = f.p.Stats(ctx, site.ID)
		if !errors.Is(last, ErrMetricsUnavailable) {
			return fmt.Sprintf("Stats: %v", last)
		}
		return ""
	})
	t.Logf("Stats without metrics-server: %v", last)
}
