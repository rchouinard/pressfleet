package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Sessions (design doc §5.8, §4.8) are the SFTP gateway's per-session
// workers: a pod in the site's namespace, as the site's user, on the node of
// the site's pod, with the site's wp-content claim and nothing else, that
// the driver attaches to through the API for as long as the client stays.
// They are task pods in all but their streams and their network: the waits,
// the cleanup, and Startup's removal of leftovers are the tasks' (task.go).

// ErrInvalidSession is returned by RunSession for a session it can't run
// as given.
var ErrInvalidSession = errors.New("kubernetes: invalid session")

const (
	// SessionComponent is the ComponentLabel value of session pods. None of
	// the site's NetworkPolicies but default-deny selects it, so a session
	// has no network at all: the gateway relays everything over its
	// streams.
	SessionComponent = "session"

	// denyAllPolicy is the site namespace's NetworkPolicy that isolates
	// every pod in it (templates/k8s/network-policies.yaml.tmpl).
	denyAllPolicy = "default-deny"

	// sessionMemoryMB is a session's memory limit, as on Compose: it serves
	// one SFTP client, which needs little memory (the server streams files
	// in 32 KiB packets).
	sessionMemoryMB = 128
	// sessionTmpSize bounds the session's /tmp, a memory-backed emptyDir
	// that counts against its memory limit, like the tmpfs on Compose.
	sessionTmpSize = 16 << 20
	// sessionMaxStderr bounds the standard error RunSession keeps for its
	// error when Session.Stderr is nil; the end is kept.
	sessionMaxStderr = 16 << 10
)

// sessionShim holds the command back until the driver has attached, as
// taskShim does, but leaves it the session's standard input: the line it
// waits for comes first on that stream, and sh's read takes no more than
// the line from a pipe.
const sessionShim = `IFS= read -r _ || exit 125; exec "$@"`

// RunSession runs a developer session in a pod in the site's namespace and
// waits for it to end (provisioner.SessionRunner).
//
// The pod runs s.Command as the site's UID and GID with the site's
// wp-content claim read-write at provisioner.TaskWPContentPath and no other
// volume but a small memory-backed /tmp, a read-only root filesystem, no
// capabilities, no service account token, and a memory limit. It is
// scheduled with the site's pod, if one runs, because the claim is
// ReadWriteOnce. The namespace's default-deny NetworkPolicy, which no other
// policy of the site widens for session pods, leaves it no network;
// RunSession refuses to start a session in a namespace without it.
//
// The driver attaches to the pod (stdin and stdinOnce), so the end of
// s.Stdin closes the command's standard input. The session ends when the
// command exits or ctx ends, and the pod is deleted whatever happened,
// which kills the command.
//
// Kubernetes has no per-pod process limit in the pod spec; the kubelet's
// podPidsLimit bounds a session's processes, as it does every pod's.
func (p *Provisioner) RunSession(ctx context.Context, siteID string, s provisioner.Session) error {
	if err := p.validSiteNamespace(siteID); err != nil {
		return fmt.Errorf("run session: %w", err)
	}
	image, err := p.sessionImage(s)
	if err != nil {
		return fmt.Errorf("run session on site %s: %w", siteID, err)
	}
	d, err := p.deployment(ctx, siteID)
	if err != nil {
		return fmt.Errorf("run session: %w", err)
	}
	fail := func(err error) error { return fmt.Errorf("run session on site %s: %w", siteID, err) }
	uid, err := siteUID(d.Spec.Template.Spec.SecurityContext)
	if err != nil {
		return fail(err)
	}
	ns := p.SiteNamespace(siteID)
	if err := p.checkDenyAll(ctx, ns); err != nil {
		return fail(err)
	}
	affinity, err := p.siteAffinity(ctx, ns)
	if err != nil {
		return fail(err)
	}
	token, err := taskToken()
	if err != nil {
		return fail(err)
	}
	spec := taskSpec{
		namespace: ns,
		name:      "session-" + token,
		token:     token,
		labels: map[string]string{
			managedByLabel: FieldManager,
			siteIDLabel:    siteID,
			// Startup removes leftovers by it, as it does tasks'.
			TaskLabel:      token,
			ComponentLabel: SessionComponent,
		},
		uid:       uid,
		image:     image,
		memoryMB:  sessionMemoryMB,
		wpContent: provisioner.MountReadWrite,
		affinity:  affinity,
		session:   true,
	}

	log := p.log.With("site_id", siteID)
	start := time.Now()
	defer p.removeTask(ctx, log, spec, false)
	if _, err := p.typed.CoreV1().Pods(ns).Create(ctx, p.sessionPod(spec, s.Command), metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
		return fail(fmt.Errorf("creating its pod: %w", err))
	}
	err = p.attachSession(ctx, spec, s)
	log.InfoContext(ctx, "session worker finished", "pod", spec.name, "namespace", ns,
		"duration", time.Since(start), "error", err)
	if err != nil {
		return fail(err)
	}
	return nil
}

// sessionImage validates s and returns its image.
func (p *Provisioner) sessionImage(s provisioner.Session) (string, error) {
	var errs []error
	image := s.Image
	if image == "" {
		image = p.tasks.image
	}
	if image == "" {
		errs = append(errs, errors.New("no image given and no task image configured"))
	} else if strings.ContainsFunc(image, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		errs = append(errs, fmt.Errorf("session image %q is not an image reference", image))
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		errs = append(errs, errors.New("a command is required"))
	}
	for _, a := range s.Command {
		if strings.IndexByte(a, 0) >= 0 {
			errs = append(errs, errors.New("command arguments can't contain NUL bytes"))
			break
		}
	}
	if s.Stdin == nil || s.Stdout == nil {
		errs = append(errs, errors.New("standard input and output are required"))
	}
	if err := errors.Join(errs...); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSession, err)
	}
	return image, nil
}

// checkDenyAll makes sure the namespace's default-deny NetworkPolicy is
// there and denies everything: a session pod relies on it alone for having
// no network.
func (p *Provisioner) checkDenyAll(ctx context.Context, ns string) error {
	pol, err := p.typed.NetworkingV1().NetworkPolicies(ns).Get(ctx, denyAllPolicy, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("the site's namespace has no %s NetworkPolicy to isolate the session", denyAllPolicy)
	} else if err != nil {
		return fmt.Errorf("reading the site's %s NetworkPolicy: %w", denyAllPolicy, err)
	}
	sel := pol.Spec.PodSelector
	if len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0 ||
		!slices.Contains(pol.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) ||
		!slices.Contains(pol.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) ||
		len(pol.Spec.Ingress) > 0 || len(pol.Spec.Egress) > 0 {
		return fmt.Errorf("the site's %s NetworkPolicy doesn't deny all traffic to and from every pod", denyAllPolicy)
	}
	return nil
}

// sessionPod is a session's pod: a task pod for spec, with the session's
// shim, host name, and /tmp.
func (p *Provisioner) sessionPod(spec taskSpec, command []string) *corev1.Pod {
	pod := p.taskPod(spec, provisioner.Task{Command: command, WPContent: provisioner.MountReadWrite})
	pod.Spec.Hostname = "pf-session-" + spec.token
	c := &pod.Spec.Containers[0]
	c.Command = slices.Concat([]string{"sh", "-c", sessionShim, "pressfleet-session"}, command)
	size := resource.NewQuantity(sessionTmpSize, resource.BinarySI)
	for i := range pod.Spec.Volumes {
		if v := &pod.Spec.Volumes[i]; v.Name == "tmp" {
			v.EmptyDir = &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: size}
		}
	}
	return pod
}

// attachSession waits for the session's container to start, attaches the
// session's streams to it until it exits or ctx ends, and turns its exit
// into RunSession's result.
func (p *Provisioner) attachSession(ctx context.Context, spec taskSpec, s provisioner.Session) error {
	early, err := p.waitTaskStart(ctx, spec)
	if err != nil {
		return err
	}
	tail := &tailBuffer{max: sessionMaxStderr}
	var stderr io.Writer = tail
	if s.Stderr != nil {
		stderr = s.Stderr
	}
	if early != nil {
		// The command never ran: the shim itself failed. Its words are in
		// the logs.
		logs, _ := p.typed.CoreV1().Pods(spec.namespace).GetLogs(spec.name, &corev1.PodLogOptions{Container: taskContainer}).DoRaw(ctx)
		_, _ = stderr.Write(logs)
		return sessionExit(spec, early, tail)
	}

	// A failing writer (the client has gone) ends the attachment; the pod
	// is then deleted, which kills the command.
	stdout := &recordingWriter{w: s.Stdout}
	attachErr := p.attacher()(ctx, spec.namespace, spec.name, taskContainer, remotecommand.StreamOptions{
		Stdin: io.MultiReader(strings.NewReader("\n"), s.Stdin), Stdout: stdout, Stderr: stderr,
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stdout.Err(); err != nil {
		return fmt.Errorf("writing its output: %w", err)
	}
	term, err := p.waitTaskEnd(ctx, spec, attachErr)
	if err != nil {
		return err
	}
	return sessionExit(spec, term, tail)
}

// sessionExit is the error for how the session's container ended, nil for
// a zero exit.
func sessionExit(spec taskSpec, term *corev1.ContainerStateTerminated, stderr *tailBuffer) error {
	if term.ExitCode == 0 {
		return nil
	}
	ee := &provisioner.ExecError{ExitCode: int(term.ExitCode), Stderr: stderr.String()}
	if term.Reason == "OOMKilled" {
		ee.Stderr += fmt.Sprintf("the session was killed for exceeding its memory limit of %d MiB (OOMKilled)\n", spec.memoryMB)
	}
	return fmt.Errorf("session pod %s/%s: %w", spec.namespace, spec.name, ee)
}
