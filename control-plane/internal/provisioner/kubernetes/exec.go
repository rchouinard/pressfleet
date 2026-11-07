package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// PHPContainer is the name of the site pod's PHP container, in which every
// exec operation but the Traefik metrics runs.
const PHPContainer = "php"

const (
	// stderrKept bounds the standard error kept for an ExecError. The end
	// is kept: WP-CLI prints its "Error:" line last, after PHP notices.
	stderrKept = 64 << 10
	// killAfter is how long a command gets to exit after timeout(1) sends
	// it SIGTERM before it is killed.
	killAfter = 10 * time.Second
	// execMaxOutput bounds the standard output of the operations that read
	// a short answer.
	execMaxOutput = 64 << 10
)

// ErrSiteNotRunning is returned by operations that need the site's pod
// when it has none running. It is provisioner.ErrSiteNotRunning, so
// driver-agnostic callers can test for it.
var ErrSiteNotRunning = provisioner.ErrSiteNotRunning

// ErrOutputTruncated is returned by ExecWPCLI, together with the output
// that was kept, when the command wrote more than the configured maximum.
var ErrOutputTruncated = errors.New("kubernetes: command output truncated")

// ErrInvalidCommand means a command can't be run as given.
var ErrInvalidCommand = errors.New("kubernetes: invalid command")

// ErrUnexpectedUser means the site's pod does not run as a site UID, so no
// command is run in it rather than risk running one as root.
var ErrUnexpectedUser = errors.New("kubernetes: the site's pod does not run as a site UID")

// ErrExecFailed means a command could not be run, or ended without an exit
// status.
var ErrExecFailed = errors.New("kubernetes: exec failed")

// execTarget names a container to run a command in.
type execTarget struct {
	Namespace, Pod, Container string
}

// executor runs a command in a container over the exec API. A command that
// ran and exited non-zero yields an error that satisfies
// [utilexec.ExitError]. It is the seam unit tests fake: the fake clientset
// can't serve the exec API's streams.
type executor interface {
	Exec(ctx context.Context, target execTarget, command []string, stdout, stderr io.Writer) error
}

// remoteExecutor runs commands against a cluster, over WebSocket where the
// API server supports it and SPDY where it doesn't.
type remoteExecutor struct {
	client rest.Interface
	cfg    *rest.Config
}

func newRemoteExecutor(cs kubernetes.Interface, cfg *rest.Config) *remoteExecutor {
	return &remoteExecutor{client: cs.CoreV1().RESTClient(), cfg: rest.CopyConfig(cfg)}
}

// Exec implements executor.
func (r *remoteExecutor) Exec(ctx context.Context, t execTarget, command []string, stdout, stderr io.Writer) error {
	u := r.client.Post().Namespace(t.Namespace).Resource("pods").Name(t.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: t.Container, Command: command, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec).URL()
	ws, err := remotecommand.NewWebSocketExecutor(r.cfg, "GET", u.String())
	if err != nil {
		return redactQuery(err, u)
	}
	spdy, err := remotecommand.NewSPDYExecutor(r.cfg, "POST", u)
	if err != nil {
		return redactQuery(err, u)
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return redactQuery(err, u)
	}
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
	return redactQuery(err, u)
}

// redactQuery keeps the command line, which holds the command's arguments
// and so may hold secrets, out of an error that names the request's URL.
func redactQuery(err error, u *url.URL) error {
	if err == nil || u.RawQuery == "" {
		return err
	}
	var ee utilexec.ExitError
	if errors.As(err, &ee) {
		return err // The exit error doesn't name the URL.
	}
	if msg := err.Error(); strings.Contains(msg, u.RawQuery) {
		return errors.New(strings.ReplaceAll(msg, u.RawQuery, "[redacted]"))
	}
	return err
}

// execResult is what a command wrote.
type execResult struct {
	Stdout    []byte
	Stderr    string
	Truncated bool // Stdout was cut at the limit.
}

// run runs command in the target. Standard output beyond stdoutMax is
// dropped and reported in Truncated; of standard error the last stderrKept
// bytes are kept. A command that exits non-zero yields a
// *provisioner.ExecError, and ctx ending yields its error.
func (p *Provisioner) run(ctx context.Context, t execTarget, command []string, stdoutMax int) (execResult, error) {
	if p.exec == nil {
		return execResult{}, fmt.Errorf("%w: the driver has no exec connection", ErrExecFailed)
	}
	stdout := &headBuffer{max: stdoutMax + 1}
	stderr := &tailBuffer{max: stderrKept}
	err := p.exec.Exec(ctx, t, command, stdout, stderr)
	res := execResult{Stdout: stdout.buf, Stderr: stderr.String()}
	if len(res.Stdout) > stdoutMax {
		res.Stdout, res.Truncated = res.Stdout[:stdoutMax], true
	}
	if err == nil {
		return res, nil
	}
	// The caller's deadline or cancellation, whatever the stream made of it.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var ee utilexec.ExitError
	if errors.As(err, &ee) && ee.ExitStatus() > 0 {
		return res, &provisioner.ExecError{ExitCode: ee.ExitStatus(), Stdout: string(res.Stdout), Stderr: res.Stderr}
	}
	return res, fmt.Errorf("%w: %w", ErrExecFailed, err)
}

// timed wraps command in timeout(1), which ends it just past the deadline
// of ctx. Closing an exec stream does not stop the process the kubelet
// started, so the process has to end itself, as it does under Compose.
func timed(ctx context.Context, command ...string) []string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return command
	}
	secs := int(math.Ceil(max(time.Until(deadline), 0).Seconds())) + 1
	argv := []string{"timeout", "--kill-after=" + strconv.Itoa(int(killAfter.Seconds())) + "s", strconv.Itoa(secs) + "s"}
	return append(argv, command...)
}

// sitePod returns the site's running pod, with its PHP container running,
// or provisioner.ErrSiteNotFound if the site doesn't exist and
// ErrSiteNotRunning if it has no such pod (it is stopped, starting, or
// stopping).
func (p *Provisioner) sitePod(ctx context.Context, siteID string) (execTarget, error) {
	t, err := p.runningSitePod(ctx, siteID)
	if err != nil {
		return execTarget{}, err
	}
	if t.Pod == "" {
		return execTarget{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotRunning)
	}
	return t, nil
}

// runningSitePod is sitePod, except that a site without a running pod
// yields an empty target and no error.
func (p *Provisioner) runningSitePod(ctx context.Context, siteID string) (execTarget, error) {
	if _, err := p.deployment(ctx, siteID); err != nil {
		return execTarget{}, err
	}
	ns := p.SiteNamespace(siteID)
	pods, err := p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
	if err != nil {
		return execTarget{}, fmt.Errorf("site %s: %w", siteID, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !containerRunning(pod, PHPContainer) {
			continue
		}
		sc := pod.Spec.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser < provisioner.MinSiteUID || *sc.RunAsUser > provisioner.MaxSiteUID {
			return execTarget{}, fmt.Errorf("site %s: %w", siteID, ErrUnexpectedUser)
		}
		return execTarget{Namespace: ns, Pod: pod.Name, Container: PHPContainer}, nil
	}
	return execTarget{}, nil
}

// containerRunning reports whether pod is alive and its container runs.
func containerRunning(pod *corev1.Pod, container string) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container {
			return cs.State.Running != nil
		}
	}
	return false
}

// headBuffer keeps the first max bytes written to it and discards the rest.
type headBuffer struct {
	buf []byte
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf     []byte
	max     int
	dropped int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	// Trim only once the buffer holds twice the limit, so trimming costs
	// amortized constant time per byte.
	if len(b.buf) > 2*b.max {
		b.trim()
	}
	return len(p), nil
}

func (b *tailBuffer) trim() {
	if cut := len(b.buf) - b.max; cut > 0 {
		b.dropped += cut
		b.buf = append(b.buf[:0], b.buf[cut:]...)
	}
}

// String returns the kept bytes, prefixed with a note if some were dropped.
func (b *tailBuffer) String() string {
	b.trim()
	if b.dropped == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("[%d bytes truncated]\n%s", b.dropped, bytes.ToValidUTF8(b.buf, nil))
}
