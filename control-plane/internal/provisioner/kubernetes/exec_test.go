package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// fakeCall is one command the fake executor ran.
type fakeCall struct {
	Target  execTarget
	Command []string
	Stdin   *string // nil when no input stream was requested
}

// fakeExec is an executor that answers from a function and records what it
// was asked to run.
type fakeExec struct {
	mu    sync.Mutex
	calls []fakeCall
	// do writes the command's output and returns its error.
	do func(ctx context.Context, c fakeCall, stdout, stderr io.Writer) error
}

func (f *fakeExec) Exec(ctx context.Context, t execTarget, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := fakeCall{Target: t, Command: command}
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		in := string(b)
		c.Stdin = &in
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.do == nil {
		return nil
	}
	return f.do(ctx, c, stdout, stderr)
}

func (f *fakeExec) last(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no command was run")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// answering is a fakeExec whose commands write out to standard output and
// return err.
func answering(out string, err error) *fakeExec {
	return &fakeExec{do: func(_ context.Context, _ fakeCall, stdout, stderr io.Writer) error {
		if err != nil {
			_, _ = io.WriteString(stderr, out)
			return err
		}
		_, _ = io.WriteString(stdout, out)
		return nil
	}}
}

func exitCode(n int) error {
	return utilexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", n), Code: n}
}

// sitePodObject is a site's pod with its containers in the given state.
func sitePodObject(running bool, uid int64) *corev1.Pod {
	state := corev1.ContainerState{}
	phase := corev1.PodPending
	if running {
		state.Running = &corev1.ContainerStateRunning{}
		phase = corev1.PodRunning
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "site-abc", Namespace: "pf-" + testSiteID,
			Labels: map[string]string{ComponentLabel: SiteComponent},
		},
		Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "php", State: state}, {Name: "nginx", State: state},
		}},
	}
}

func siteDeploymentObject(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: SiteDeployment, Namespace: "pf-" + testSiteID},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// execCluster is the driver with a fake executor, on a cluster holding objs.
func execCluster(t *testing.T, ex *fakeExec, objs ...runtime.Object) *Provisioner {
	t.Helper()
	p := cluster(t, nil, objs...)
	p.exec = ex
	return p
}

const testUID = 20001

func runningSite() []runtime.Object {
	return []runtime.Object{siteDeploymentObject(1), sitePodObject(true, testUID)}
}

func TestExecWPCLI(t *testing.T) {
	ex := answering("https://example.com\n", nil)
	p := execCluster(t, ex, runningSite()...)
	out, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"option", "get", "--", "home"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "https://example.com\n" {
		t.Errorf("output = %q", out)
	}
	c := ex.last(t)
	want := execTarget{Namespace: "pf-" + testSiteID, Pod: "site-abc", Container: "php"}
	if c.Target != want {
		t.Errorf("target = %+v, want %+v", c.Target, want)
	}
	// timeout(1) first, then WP-CLI with the arguments verbatim.
	if len(c.Command) < 5 || c.Command[0] != "timeout" || !strings.HasPrefix(c.Command[1], "--kill-after=") || !strings.HasSuffix(c.Command[2], "s") {
		t.Fatalf("command = %q", c.Command)
	}
	if got := strings.Join(c.Command[3:], " "); got != "/usr/local/bin/wp option get -- home" {
		t.Errorf("command after timeout = %q", got)
	}
}

func TestExecWPCLIErrors(t *testing.T) {
	tests := map[string]struct {
		objs []runtime.Object
		do   func(context.Context, fakeCall, io.Writer, io.Writer) error
		args []string
		want error
		ran  bool
	}{
		"missing site": {want: provisioner.ErrSiteNotFound},
		"scaled to zero": {
			objs: []runtime.Object{siteDeploymentObject(0)},
			want: ErrSiteNotRunning,
		},
		"pod not running": {
			objs: []runtime.Object{siteDeploymentObject(1), sitePodObject(false, testUID)},
			want: ErrSiteNotRunning,
		},
		"root pod": {
			objs: []runtime.Object{siteDeploymentObject(1), sitePodObject(true, 0)},
			want: ErrUnexpectedUser,
		},
		"NUL byte": {
			objs: runningSite(), args: []string{"a\x00b"}, want: ErrInvalidCommand,
		},
		"exec failure": {
			objs: runningSite(), ran: true, want: ErrExecFailed,
			do: func(context.Context, fakeCall, io.Writer, io.Writer) error {
				return errors.New("connection refused")
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExec{do: tc.do}
			p := execCluster(t, ex, tc.objs...)
			_, err := p.ExecWPCLI(t.Context(), testSiteID, append([]string{"user", "create"}, tc.args...))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if ran := ex.count() > 0; ran != tc.ran {
				t.Errorf("command run = %t, want %t", ran, tc.ran)
			}
		})
	}
}

func TestExecWPCLIExitCode(t *testing.T) {
	ex := &fakeExec{do: func(_ context.Context, _ fakeCall, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "partial")
		_, _ = io.WriteString(stderr, "Error: nope\n")
		return exitCode(3)
	}}
	p := execCluster(t, ex, runningSite()...)
	_, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"fail"})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v, want *provisioner.ExecError", err)
	}
	if ee.ExitCode != 3 || ee.Stdout != "partial" || ee.Stderr != "Error: nope\n" {
		t.Errorf("ExecError = %+v", ee)
	}
}

func TestExecWPCLIOutputLimit(t *testing.T) {
	p := execCluster(t, answering(strings.Repeat("x", 100), nil), runningSite()...)
	p.wpcliMaxOutput = 10
	out, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"big"})
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("error = %v, want ErrOutputTruncated", err)
	}
	if out != strings.Repeat("x", 10) {
		t.Errorf("output = %q", out)
	}
}

func TestExecWPCLIStderrTail(t *testing.T) {
	ex := &fakeExec{do: func(_ context.Context, _ fakeCall, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, strings.Repeat("n", 3*stderrKept))
		_, _ = io.WriteString(stderr, "Error: last\n")
		return exitCode(1)
	}}
	p := execCluster(t, ex, runningSite()...)
	_, err := p.ExecWPCLI(t.Context(), testSiteID, nil)
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ee.Stderr, "Error: last\n") || len(ee.Stderr) > stderrKept+64 {
		t.Errorf("stderr has %d bytes, ends %q", len(ee.Stderr), ee.Stderr[max(0, len(ee.Stderr)-20):])
	}
}

func TestExecWPCLITimeoutAndCancel(t *testing.T) {
	block := func(ctx context.Context, _ fakeCall, _, _ io.Writer) error {
		<-ctx.Done()
		return errors.New("stream closed") // What the stream makes of it.
	}
	t.Run("configured timeout", func(t *testing.T) {
		ex := &fakeExec{do: block}
		p := execCluster(t, ex, runningSite()...)
		p.wpcliTimeout = 50 * time.Millisecond
		_, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"slow"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want DeadlineExceeded", err)
		}
		// The in-container limit sits just past the deadline.
		if c := ex.last(t); c.Command[0] != "timeout" || c.Command[2] != "2s" {
			t.Errorf("command = %q", c.Command)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		p := execCluster(t, &fakeExec{do: block}, runningSite()...)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, err := p.ExecWPCLI(ctx, testSiteID, []string{"slow"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want Canceled", err)
		}
	})
}

func TestRedactQuery(t *testing.T) {
	u, err := url.Parse("https://k8s/api/v1/namespaces/n/pods/p/exec?command=wp&command=--user_pass%3Dsecret")
	if err != nil {
		t.Fatal(err)
	}
	got := redactQuery(fmt.Errorf("Get %q: refused", u.String()), u)
	if strings.Contains(got.Error(), "secret") {
		t.Errorf("error leaks arguments: %v", got)
	}
	exit := exitCode(2)
	if got := redactQuery(exit, u); !errors.Is(got, exit) {
		t.Errorf("exit error changed: %v", got)
	}
}

func TestExecWPCLIInput(t *testing.T) {
	ex := answering("ok", nil)
	p := execCluster(t, ex, runningSite()...)
	if _, err := p.ExecWPCLIInput(t.Context(), testSiteID, []string{"user", "update", "1", "--prompt=user_pass"}, strings.NewReader("pw\n")); err != nil {
		t.Fatal(err)
	}
	if c := ex.last(t); c.Stdin == nil || *c.Stdin != "pw\n" {
		t.Errorf("stdin = %v, want %q", c.Stdin, "pw\n")
	}
	// ExecWPCLI requests no input stream.
	if _, err := p.ExecWPCLI(t.Context(), testSiteID, []string{"option", "get", "home"}); err != nil {
		t.Fatal(err)
	}
	if c := ex.last(t); c.Stdin != nil {
		t.Errorf("ExecWPCLI sent stdin %q", *c.Stdin)
	}
}

// TestSiteInstallExecURL runs a site install, the way package sites does,
// through the driver's real exec client against a stand-in API server, and
// checks the requests' URLs, which the API server's audit log records: the
// admin password travels on standard input and appears in none of them.
func TestSiteInstallExecURL(t *testing.T) {
	const password = "Sup3r-s3cret-admin-pw"
	var (
		mu   sync.Mutex
		uris []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.RequestURI)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`)
	}))
	t.Cleanup(srv.Close)
	rc := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	p := cluster(t, nil, runningSite()...)
	p.exec = newRemoteExecutor(cs, rc)

	args := []string{
		"core", "install", "--url=https://example.com", "--title=Example",
		"--admin_user=owner", "--admin_email=owner@example.com", "--skip-email", "--prompt=admin_password",
	}
	_, err = p.ExecWPCLIInput(t.Context(), testSiteID, args, strings.NewReader(password+"\n"))
	if err == nil {
		t.Fatal("the stand-in API server refuses every exec, but the install succeeded")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error leaks the password: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(uris) == 0 {
		t.Fatal("no exec request reached the API server")
	}
	for _, uri := range uris {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if !strings.HasSuffix(u.Path, "/pods/site-abc/exec") || !slices.Contains(q["command"], "--prompt=admin_password") {
			t.Errorf("request %q isn't the install's exec", uri)
		}
		if q.Get("stdin") != "true" {
			t.Errorf("request %q opens no standard input", uri)
		}
		if decoded, _ := url.QueryUnescape(uri); strings.Contains(decoded, password) || strings.Contains(uri, password) {
			t.Errorf("request URL holds the password: %q", uri)
		}
	}
}

func TestPurgeCache(t *testing.T) {
	ex := &fakeExec{}
	p := execCluster(t, ex, runningSite()...)
	if err := p.PurgeCache(t.Context(), testSiteID); err != nil {
		t.Fatal(err)
	}
	c := ex.last(t)
	if got := strings.Join(c.Command[3:], " "); got != "find /var/cache/pressfleet/fastcgi -mindepth 1 -delete" || c.Target.Container != "php" {
		t.Errorf("command = %q in %s", c.Command, c.Target.Container)
	}

	// A stopped site has no cache; a missing one is an error.
	ex = &fakeExec{}
	p = execCluster(t, ex, siteDeploymentObject(0))
	if err := p.PurgeCache(t.Context(), testSiteID); err != nil || ex.count() != 0 {
		t.Errorf("stopped site: error %v after %d commands", err, ex.count())
	}
	p = execCluster(t, &fakeExec{})
	if err := p.PurgeCache(t.Context(), testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: %v", err)
	}

	p = execCluster(t, answering("find: oops", exitCode(1)), runningSite()...)
	var ee *provisioner.ExecError
	if err := p.PurgeCache(t.Context(), testSiteID); !errors.As(err, &ee) {
		t.Errorf("failing find: %v", err)
	}
}

func TestReloadCode(t *testing.T) {
	ex := &fakeExec{}
	p := execCluster(t, ex, runningSite()...)
	if err := p.ReloadCode(t.Context(), testSiteID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ex.last(t).Command, " "); got != "sh -c kill -USR2 1" {
		t.Errorf("command = %q", got)
	}

	ex = &fakeExec{}
	p = execCluster(t, ex, siteDeploymentObject(0))
	if err := p.ReloadCode(t.Context(), testSiteID); err != nil || ex.count() != 0 {
		t.Errorf("stopped site: error %v after %d commands", err, ex.count())
	}
	p = execCluster(t, &fakeExec{})
	if err := p.ReloadCode(t.Context(), testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: %v", err)
	}
}

func TestProbeHTTP(t *testing.T) {
	req := provisioner.ProbeRequest{Host: "example.com", Path: "/healthz?a=b"}
	ex := answering("200", nil)
	p := execCluster(t, ex, runningSite()...)
	resp, err := p.ProbeHTTP(t.Context(), testSiteID, req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("ProbeHTTP = %+v, %v", resp, err)
	}
	// php -r SCRIPT -- host path, after timeout(1).
	cmd := ex.last(t).Command
	n := len(cmd)
	if got := strings.Join(cmd[n-3:], " "); got != "-- example.com /healthz?a=b" {
		t.Errorf("command tail = %q", got)
	}
	if script := cmd[n-4]; !strings.Contains(script, "X-Forwarded-Proto: https") || !strings.Contains(script, "127.0.0.1") {
		t.Errorf("script doesn't send the forwarded scheme over loopback: %s", script)
	}

	tests := map[string]struct {
		ex   *fakeExec
		req  provisioner.ProbeRequest
		objs []runtime.Object
		want error
	}{
		"no response": {answering("nginx sent no HTTP response", exitCode(1)), req, runningSite(), ErrProbeFailed},
		"garbage":     {answering("oops", nil), req, runningSite(), ErrProbeFailed},
		"bad host":    {answering("200", nil), provisioner.ProbeRequest{Host: "a b", Path: "/"}, runningSite(), ErrInvalidCommand},
		"bad path":    {answering("200", nil), provisioner.ProbeRequest{Host: "a", Path: "x"}, runningSite(), ErrInvalidCommand},
		"not running": {&fakeExec{}, req, []runtime.Object{siteDeploymentObject(0)}, ErrSiteNotRunning},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := execCluster(t, tc.ex, tc.objs...)
			if _, err := p.ProbeHTTP(t.Context(), testSiteID, tc.req); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFPMStatus(t *testing.T) {
	out := "Status: 200 OK\r\nContent-type: application/json\r\n\r\n" +
		`{"idle processes":3,"active processes":1,"max children reached":2,"slow requests":4}` + "\nmax_children=8\n"
	p := execCluster(t, answering(out, nil), runningSite()...)
	st, err := p.FPMStatus(t.Context(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	want := provisioner.FPMStatus{ActiveProcesses: 1, IdleProcesses: 3, MaxChildren: 8, MaxChildrenReached: 2, SlowRequests: 4}
	if *st != want {
		t.Errorf("FPMStatus = %+v, want %+v", *st, want)
	}

	p = execCluster(t, answering("nothing", nil), runningSite()...)
	if _, err := p.FPMStatus(t.Context(), testSiteID); !errors.Is(err, ErrFPMStatus) {
		t.Errorf("garbage: %v", err)
	}
	p = execCluster(t, &fakeExec{}, siteDeploymentObject(0))
	if _, err := p.FPMStatus(t.Context(), testSiteID); !errors.Is(err, ErrSiteNotRunning) {
		t.Errorf("stopped: %v", err)
	}
}

func traefikPod(name string, running bool) *corev1.Pod {
	pod := sitePodObject(running, 65532)
	pod.Name, pod.Namespace = name, "pressfleet"
	pod.Labels = map[string]string{"app.kubernetes.io/name": "pressfleet", "app.kubernetes.io/component": "traefik"}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "traefik", State: pod.Status.ContainerStatuses[0].State}}
	return pod
}

func TestTraefikMetrics(t *testing.T) {
	ex := answering("traefik_service_requests_total 1\n", nil)
	p := execCluster(t, ex, traefikPod("traefik-a", false), traefikPod("traefik-b", true))
	out, err := p.TraefikMetrics(t.Context())
	if err != nil || string(out) != "traefik_service_requests_total 1\n" {
		t.Fatalf("TraefikMetrics = %q, %v", out, err)
	}
	c := ex.last(t)
	if c.Target != (execTarget{Namespace: "pressfleet", Pod: "traefik-b", Container: "traefik"}) {
		t.Errorf("target = %+v", c.Target)
	}
	if got := strings.Join(c.Command, " "); got != "wget -q -O- http://127.0.0.1:8082/metrics" {
		t.Errorf("command = %q", got)
	}

	p = execCluster(t, &fakeExec{}, traefikPod("traefik-a", false))
	if _, err := p.TraefikMetrics(t.Context()); !errors.Is(err, ErrTraefikMetrics) {
		t.Errorf("no running pod: %v", err)
	}
	p = execCluster(t, answering("wget: can't connect", exitCode(1)), traefikPod("traefik-a", true))
	if _, err := p.TraefikMetrics(t.Context()); !errors.Is(err, ErrTraefikMetrics) {
		t.Errorf("wget failure: %v", err)
	}
}
