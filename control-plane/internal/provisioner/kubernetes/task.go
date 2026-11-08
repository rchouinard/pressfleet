package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/outbuf"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Tasks (design doc §4.4, §4.8) are pods: a site task in the site's
// namespace, as the site's user, on the node of the site's pod; a
// repository task in the platform namespace, as provisioner.RepositoryGID,
// behind a NetworkPolicy of its own. Each pod has a unique host name, for
// restic's locks, its memory limit as the container's request and limit,
// Task.Timeout as activeDeadlineSeconds, and its secret files in a Secret
// volume. The driver attaches to the pod for its output (stdout and stderr
// apart, which the pod logs API would merge), reads the exit code from its
// status, and deletes the pod and its Secret and NetworkPolicy whatever
// happened; Startup deletes what a crash of the control plane left.

var (
	// ErrInvalidTask is returned by RunTask and RunRepositoryTask for a
	// task they can't run as given.
	ErrInvalidTask = errors.New("kubernetes: invalid task")
)

const (
	// TaskLabel is on every task's pod, Secret, and NetworkPolicy, with
	// the task's own token as the value: Startup finds leftovers by it,
	// and a repository task's NetworkPolicy selects its pod by it.
	TaskLabel = "pressfleet.io/task"
	// TaskComponent is the ComponentLabel value of task pods. The site's
	// egress NetworkPolicy selects it, so a site task reaches what the site
	// reaches; it is not app.kubernetes.io/component, which the chart's
	// policies select platform services by.
	TaskComponent = "task"
	// repositoryLabel names a repository task's repository.
	repositoryLabel = "pressfleet.io/repository"

	taskContainer = "task"
	// taskHome is HOME in a task, an emptyDir: the root filesystem is
	// read-only, and restic keeps its cache under HOME.
	taskHome = "/tmp"
	// taskMaxOutput bounds the standard output a result keeps, as on
	// Compose; taskMaxStderr the end of standard error it keeps.
	taskMaxOutput = 8 << 20
	taskMaxStderr = 64 << 10
	// taskCleanupTimeout bounds deleting a task's objects, which happens
	// even after ctx has ended.
	taskCleanupTimeout = time.Minute
	// taskEndTimeout bounds waiting for the pod's status to show that the
	// container exited, once its streams have ended.
	taskEndTimeout = time.Minute
	// taskLostStreamTimeout is how long a task whose streams failed gets
	// to show that it exited after all.
	taskLostStreamTimeout = 10 * time.Second
	// taskPollInterval is how often the driver looks at a task's pod.
	taskPollInterval = 250 * time.Millisecond
)

// taskShim holds the command back until the driver has attached: attaching
// shows only what is written after it, and the logs API would merge
// standard output and standard error. The driver sends one line on standard
// input once attached; the command then replaces the shell, with no
// standard input, as on Compose. Both task images have sh.
const taskShim = `IFS= read -r _ || exit 125; exec "$@" </dev/null`

var (
	envName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// repositoryName matches client group IDs, which go into pod names and
	// label values.
	repositoryName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,58}[a-z0-9])?$`)
)

// TaskConfig configures the driver's tasks.
type TaskConfig struct {
	// Image is the image of a task that names none (task_image).
	Image string
	// MemoryLimitMB is the memory limit of a task that sets none
	// (backup.task_memory_mb); zero means config.DefaultBackupTaskMemoryMB.
	MemoryLimitMB int
	// S3Endpoint is backup.s3.endpoint. A repository task with
	// BackupNetwork reaches the internet, but not the cluster or private
	// ranges, unless the endpoint is there: a Service of the cluster, or a
	// private address.
	S3Endpoint string
}

// taskSettings are the driver's settings for tasks.
type taskSettings struct {
	image     string
	memoryMB  int
	dataClaim string
	// s3 is where a repository task with BackupNetwork may go besides the
	// internet; nil when the internet rule covers the S3 endpoint.
	s3 *s3Peer
	// warnReclaim logs once that the data volume isn't Retain.
	warnReclaim *sync.Once
}

func newTaskSettings(cfg Config) taskSettings {
	s := taskSettings{
		image:       cfg.Tasks.Image,
		memoryMB:    cfg.Tasks.MemoryLimitMB,
		dataClaim:   cfg.Kubernetes.DataClaim,
		warnReclaim: &sync.Once{},
	}
	if s.memoryMB == 0 {
		s.memoryMB = config.DefaultBackupTaskMemoryMB
	}
	if s.dataClaim == "" {
		s.dataClaim = config.DefaultDataClaim
	}
	s.s3 = resolveS3Peer(cfg.Tasks.S3Endpoint, cfg.Kubernetes.PlatformNamespace)
	return s
}

// s3Peer is an S3 service that a repository task's NetworkPolicy allows
// by address or namespace, because the internet rule leaves it out.
type s3Peer struct {
	// Namespace is set for a Service of the cluster, CIDR for an address.
	Namespace string
	CIDR      string
	Port      int
}

// resolveS3Peer works out where backup.s3.endpoint is, for the
// NetworkPolicy of repository tasks: nil for a host name outside the
// cluster or a public address, which the internet rule reaches.
func resolveS3Peer(endpoint, platformNamespace string) *s3Peer {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p, err := strconv.Atoi(u.Port()); err == nil {
		port = p
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !slices.ContainsFunc(nonInternet, func(n netip.Prefix) bool { return n.Contains(ip) }) {
			return nil
		}
		return &s3Peer{CIDR: netip.PrefixFrom(ip, ip.BitLen()).String(), Port: port}
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	switch {
	case len(labels) == 1:
		// A bare Service name, as the control plane resolves it.
		return &s3Peer{Namespace: platformNamespace, Port: port}
	case len(labels) >= 3 && labels[2] == "svc":
		return &s3Peer{Namespace: labels[1], Port: port}
	}
	return nil
}

// taskSpec is a validated task, and where and as whom it runs.
type taskSpec struct {
	namespace string
	// name is the pod's, and its Secret's and NetworkPolicy's.
	name   string
	token  string
	labels map[string]string
	uid    int64
	groups []int64
	image  string
	// memoryMB is the memory limit.
	memoryMB  int
	wpContent provisioner.MountMode
	// dataClaim is the claim of the data volume in the task's namespace,
	// for transferDir and repositoryDir: their paths on the volume.
	dataClaim     string
	transferDir   string
	repositoryDir string
	affinity      *corev1.Affinity
	policy        *networkingv1.NetworkPolicy
	// session marks a session's pod (session.go), which shares the waits
	// and the cleanup of tasks; messages name it.
	session bool
}

// noun is what messages call the pod's workload.
func (s taskSpec) noun() string {
	if s.session {
		return "session"
	}
	return "task"
}

// taskTarget says who a task is for, as errors and log lines name it.
type taskTarget struct {
	what string
	log  *slog.Logger
}

// RunTask runs task in a pod in the site's namespace and waits for it to
// finish (provisioner.TaskRunner).
//
// The pod runs as the site's UID and GID, with provisioner.RepositoryGID
// as a supplementary group when it has a repository, a read-only root
// filesystem, no capabilities, and no service account token. It mounts the
// site's wp-content claim as the task asks, and the transfer and
// repository directories from the site namespace's clone of the data
// volume (ensureDataClone). A task that mounts wp-content gets an affinity
// to the site's pod, if one is running, because the claim is
// ReadWriteOnce; for a stopped site, the volume's own topology places it.
// It is covered by the site's NetworkPolicies, so it reaches what the site
// reaches.
//
// The pod sets no fsGroup: fsGroup changes the group of a whole volume, and
// the data volume holds every site's files. wp-content already belongs to
// the site's group (the site pod's fsGroup); the transfer directory is
// shared with the task through the control plane's group
// (shareTransferDir), which the task gets as a supplementary group.
//
// The site may be running or stopped. A non-zero exit yields a
// *provisioner.ExecError; the pod and its Secret are always deleted.
func (p *Provisioner) RunTask(ctx context.Context, siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
	if err := p.validSiteNamespace(siteID); err != nil {
		return nil, fmt.Errorf("run task: %w", err)
	}
	target := taskTarget{what: "site " + siteID, log: p.log.With("site_id", siteID)}
	spec, err := p.taskSpec(task)
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", target.what, err)
	}
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}
	d, err := p.deployment(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("run task: %w", err)
	}
	uid, err := siteUID(d.Spec.Template.Spec.SecurityContext)
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", target.what, err)
	}
	spec.namespace = p.SiteNamespace(siteID)
	spec.uid = uid
	if task.RepositoryDir != "" {
		// Still the site's user, in the group that owns the repositories,
		// which the client group's sites share.
		spec.groups = append(spec.groups, provisioner.RepositoryGID)
	}
	if task.WPContent != provisioner.MountNone {
		if spec.affinity, err = p.siteAffinity(ctx, spec.namespace); err != nil {
			return nil, fmt.Errorf("run task on %s: %w", target.what, err)
		}
	}
	if spec.transferDir != "" || spec.repositoryDir != "" {
		if spec.dataClaim, err = p.ensureDataClone(ctx, siteID); err != nil {
			return nil, fmt.Errorf("run task on %s: %w", target.what, err)
		}
	}
	spec.labels = map[string]string{siteIDLabel: siteID}
	return p.runTaskPod(ctx, target, spec, task)
}

// RunRepositoryTask runs task in a pod of the platform namespace, tied to
// a backup repository instead of a site
// (provisioner.RepositoryTaskRunner).
//
// The pod has the hardening of a site task and the memory limit, but none
// of a site: it runs as provisioner.RepositoryGID, has no wp-content, and
// mounts the transfer and repository directories from the data volume's
// own claim. A NetworkPolicy of its own denies it all traffic, or, with
// Task.BackupNetwork (the repository is on S3), allows DNS, the internet
// but not the cluster's private ranges, and the S3 endpoint where it is
// in them (TaskConfig.S3Endpoint). Its labels are none of the chart's, so
// none of the chart's policies take it for a platform service.
func (p *Provisioner) RunRepositoryTask(ctx context.Context, repository string, task provisioner.Task) (*provisioner.TaskResult, error) {
	if repository != provisioner.StateRepository && !repositoryName.MatchString(repository) {
		return nil, fmt.Errorf("run repository task: %w: repository %q must be a client group ID or %q",
			ErrInvalidTask, repository, provisioner.StateRepository)
	}
	target := taskTarget{what: "repository " + repository, log: p.log.With("repository", repository)}
	spec, err := p.taskSpec(task)
	if err == nil {
		err = checkRepositoryTask(task)
	}
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", target.what, err)
	}
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}
	spec.namespace = p.platformNamespace
	spec.uid = provisioner.RepositoryGID
	if spec.transferDir != "" || spec.repositoryDir != "" {
		spec.dataClaim = p.tasks.dataClaim
	}
	spec.labels = map[string]string{repositoryLabel: repository}
	spec.policy = p.repositoryTaskPolicy(task.BackupNetwork)
	return p.runTaskPod(ctx, target, spec, task)
}

// checkRepositoryTask refuses what belongs to sites.
func checkRepositoryTask(task provisioner.Task) error {
	var errs []error
	if task.WPContent != provisioner.MountNone {
		errs = append(errs, fmt.Errorf("a repository task has no wp-content (mount mode %d)", task.WPContent))
	}
	if task.RepositoryDir == "" && !task.BackupNetwork {
		errs = append(errs, errors.New("a repository task needs RepositoryDir or BackupNetwork to say where the repository is"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTask, err)
	}
	return nil
}

// siteUID returns the site's UID from its pod template, which exists
// whether or not the site runs.
func siteUID(sc *corev1.PodSecurityContext) (int64, error) {
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser < provisioner.MinSiteUID {
		return 0, errors.New("the site's pod template has no site UID")
	}
	return *sc.RunAsUser, nil
}

// siteAffinity places a task with the site's pod, if one is there: the
// wp-content claim is ReadWriteOnce, so only pods on that node can mount
// it. Without one, there is nothing to be near, and a required affinity
// to nothing would never schedule.
func (p *Provisioner) siteAffinity(ctx context.Context, ns string) (*corev1.Affinity, error) {
	pods, err := p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
	if err != nil {
		return nil, fmt.Errorf("listing the site's pods: %w", err)
	}
	if !slices.ContainsFunc(pods.Items, func(pod corev1.Pod) bool { return pod.DeletionTimestamp == nil }) {
		return nil, nil
	}
	return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: SiteComponent}},
			TopologyKey:   corev1.LabelHostname,
		}},
	}}, nil
}

// taskSpec validates task and resolves its image, memory limit, and
// directories.
func (p *Provisioner) taskSpec(task provisioner.Task) (taskSpec, error) {
	var errs []error
	spec := taskSpec{image: task.Image, wpContent: task.WPContent, memoryMB: task.MemoryLimitMB}
	switch {
	case task.MemoryLimitMB == 0:
		spec.memoryMB = p.tasks.memoryMB
	case task.MemoryLimitMB < config.MinBackupTaskMemoryMB || task.MemoryLimitMB > config.MaxBackupTaskMemoryMB:
		errs = append(errs, fmt.Errorf("memory limit must be from %d to %d MiB, got %d",
			config.MinBackupTaskMemoryMB, config.MaxBackupTaskMemoryMB, task.MemoryLimitMB))
	}
	if spec.image == "" {
		spec.image = p.tasks.image
	}
	if spec.image == "" {
		errs = append(errs, errors.New("no image given and no task image configured"))
	} else if strings.ContainsFunc(spec.image, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		errs = append(errs, fmt.Errorf("task image %q is not an image reference", spec.image))
	}
	if len(task.Command) == 0 || task.Command[0] == "" {
		errs = append(errs, errors.New("a command is required"))
	}
	for _, a := range task.Command {
		if strings.IndexByte(a, 0) >= 0 {
			errs = append(errs, errors.New("command arguments can't contain NUL bytes"))
			break
		}
	}
	for name, v := range task.Env {
		if !envName.MatchString(name) {
			errs = append(errs, fmt.Errorf("environment variable name %q is invalid", name))
		}
		if strings.IndexByte(v, 0) >= 0 {
			errs = append(errs, fmt.Errorf("environment variable %s can't contain NUL bytes", name))
		}
	}
	for name := range task.SecretFiles {
		if !secretFileName.MatchString(name) {
			errs = append(errs, fmt.Errorf("secret file name %q is invalid", name))
		}
	}
	switch task.WPContent {
	case provisioner.MountNone, provisioner.MountReadOnly, provisioner.MountReadWrite:
	default:
		errs = append(errs, fmt.Errorf("unknown wp-content mount mode %d", task.WPContent))
	}
	if task.Timeout < 0 {
		errs = append(errs, errors.New("timeout must not be negative"))
	}
	var err error
	if task.TransferDir != "" {
		if spec.transferDir, err = dataSubPath(p.layout.Root, p.layout.TmpDir(), task.TransferDir); err != nil {
			errs = append(errs, fmt.Errorf("transfer directory: %w", err))
		}
	}
	if task.RepositoryDir != "" {
		if spec.repositoryDir, err = dataSubPath(p.layout.Root, p.layout.Root, task.RepositoryDir); err != nil {
			errs = append(errs, fmt.Errorf("repository directory: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return taskSpec{}, fmt.Errorf("%w: %w", ErrInvalidTask, err)
	}
	return spec, nil
}

// taskToken returns a random string that makes a task's names unique.
func taskToken() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// runTaskPod creates the task's objects from spec, runs the pod attached,
// and returns its result. The objects are deleted afterwards whatever
// happened, also when ctx ends.
func (p *Provisioner) runTaskPod(ctx context.Context, target taskTarget, spec taskSpec, task provisioner.Task) (*provisioner.TaskResult, error) {
	token, err := taskToken()
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", target.what, err)
	}
	spec.token, spec.name = token, "task-"+token
	spec.labels[managedByLabel] = FieldManager
	spec.labels[TaskLabel] = token
	spec.labels[ComponentLabel] = TaskComponent
	if spec.transferDir != "" {
		// Before the pod exists: the transfer directory is the control
		// plane's until it hands it over.
		gid := os.Getegid()
		if err := shareTransferDir(task.TransferDir, gid); err != nil {
			return nil, fmt.Errorf("run task on %s: %w", target.what, err)
		}
		if gid != 0 && int64(gid) != spec.uid && !slices.Contains(spec.groups, int64(gid)) {
			spec.groups = append(spec.groups, int64(gid))
		}
	}

	start := time.Now()
	defer p.removeTask(ctx, target.log, spec, len(task.SecretFiles) > 0)
	core := p.typed.CoreV1()
	opts := metav1.CreateOptions{FieldManager: FieldManager}
	if spec.policy != nil {
		// First: the pod must never run without it.
		pol := spec.policy.DeepCopy()
		pol.Name, pol.Namespace, pol.Labels = spec.name, spec.namespace, spec.labels
		pol.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{TaskLabel: token}}
		if _, err := p.typed.NetworkingV1().NetworkPolicies(spec.namespace).Create(ctx, pol, opts); err != nil {
			return nil, fmt.Errorf("run task on %s: creating its network policy: %w", target.what, err)
		}
	}
	if len(task.SecretFiles) > 0 {
		data := make(map[string][]byte, len(task.SecretFiles))
		for name, v := range task.SecretFiles {
			data[name] = []byte(v.Reveal())
		}
		immutable := true
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: spec.name, Namespace: spec.namespace, Labels: spec.labels},
			Type:       corev1.SecretTypeOpaque,
			Immutable:  &immutable,
			Data:       data,
		}
		if _, err := core.Secrets(spec.namespace).Create(ctx, secret, opts); err != nil {
			return nil, fmt.Errorf("run task on %s: creating its secret: %w", target.what, err)
		}
	}
	if _, err := core.Pods(spec.namespace).Create(ctx, p.taskPod(spec, task), opts); err != nil {
		return nil, fmt.Errorf("run task on %s: creating its pod: %w", target.what, err)
	}

	res, err := p.attachTask(ctx, target, spec, task)
	if err != nil {
		target.log.WarnContext(ctx, "task failed", "pod", spec.name, "namespace", spec.namespace,
			"command", task.Command[0], "duration", time.Since(start), "error", err)
		return res, err
	}
	target.log.InfoContext(ctx, "task finished", "pod", spec.name, "namespace", spec.namespace,
		"command", task.Command[0], "duration", time.Since(start))
	return res, nil
}

// attachTask waits for the task's container to start, attaches to it,
// lets it run, and turns its exit into the result.
func (p *Provisioner) attachTask(ctx context.Context, target taskTarget, spec taskSpec, task provisioner.Task) (*provisioner.TaskResult, error) {
	fail := func(err error) (*provisioner.TaskResult, error) {
		return nil, fmt.Errorf("run task on %s: %w", target.what, err)
	}
	early, err := p.waitTaskStart(ctx, spec)
	if err != nil {
		return fail(err)
	}
	stdout := &outbuf.Head{Max: taskMaxOutput + 1}
	stderr := &outbuf.Tail{Max: taskMaxStderr}
	if early != nil {
		// The command never ran: the shim itself failed. Its words are in
		// the logs.
		logs, _ := p.typed.CoreV1().Pods(spec.namespace).GetLogs(spec.name, &corev1.PodLogOptions{Container: taskContainer}).DoRaw(ctx)
		_, _ = stderr.Write(logs)
		return p.taskResult(ctx, spec, early, stdout, stderr)
	}

	var out io.Writer = stdout
	var streamed *recordingWriter
	if task.Stdout != nil {
		// Streamed: the result's Stdout stays empty. A failing writer ends
		// the attachment, and the pod is deleted, which kills the task.
		streamed = &recordingWriter{w: task.Stdout}
		out = streamed
	}
	attachErr := p.attacher()(ctx, spec.namespace, spec.name, taskContainer, remotecommand.StreamOptions{
		Stdin: strings.NewReader("\n"), Stdout: out, Stderr: stderr,
	})
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if streamed != nil && streamed.Err() != nil {
		return fail(fmt.Errorf("writing its output: %w", streamed.Err()))
	}
	term, err := p.waitTaskEnd(ctx, spec, attachErr)
	if err != nil {
		return fail(err)
	}
	return p.taskResult(ctx, spec, term, stdout, stderr)
}

// taskResult turns the container's exit into RunTask's result.
func (p *Provisioner) taskResult(ctx context.Context, spec taskSpec, term *corev1.ContainerStateTerminated, stdout *outbuf.Head, stderr *outbuf.Tail) (*provisioner.TaskResult, error) {
	what := spec.namespace + "/" + spec.name
	out := stdout.Bytes()
	truncated := len(out) > taskMaxOutput
	if truncated {
		out = out[:taskMaxOutput]
	}
	res := &provisioner.TaskResult{Stdout: string(out), Stderr: stderr.String()}
	if term.ExitCode != 0 {
		ee := &provisioner.ExecError{ExitCode: int(term.ExitCode), Stdout: res.Stdout, Stderr: res.Stderr}
		if term.Reason == "OOMKilled" {
			// Kubernetes says so, which restic, killed, can't.
			ee.Stderr += fmt.Sprintf("the task was killed for exceeding its memory limit of %d MiB (OOMKilled)\n", spec.memoryMB)
		}
		return nil, fmt.Errorf("run task in pod %s: %w", what, ee)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if truncated {
		return res, fmt.Errorf("run task in pod %s: %w to the first %d bytes", what, ErrOutputTruncated, taskMaxOutput)
	}
	return res, nil
}

// waitTaskStart waits, at most the driver's wait timeout, until the task's
// container runs. It returns the container's end if it ended already,
// which only the shim can make it do before the driver attaches.
func (p *Provisioner) waitTaskStart(ctx context.Context, spec taskSpec) (*corev1.ContainerStateTerminated, error) {
	wctx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	var (
		ended *corev1.ContainerStateTerminated
		why   string
	)
	err := wait.PollUntilContextCancel(wctx, p.taskPoll(), true, func(ctx context.Context) (bool, error) {
		pod, err := p.typed.CoreV1().Pods(spec.namespace).Get(ctx, spec.name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if err := podFailure(pod, spec.noun()); err != nil {
			return false, err
		}
		if cs := taskContainerStatus(pod); cs != nil {
			switch w := cs.State.Waiting; {
			case cs.State.Running != nil:
				return true, nil
			case cs.State.Terminated != nil:
				ended = cs.State.Terminated
				return true, nil
			case w != nil && stuckReasons[w.Reason]:
				return false, fmt.Errorf("the %s's container can't start: %s: %s", spec.noun(), w.Reason, w.Message)
			case w != nil:
				why = w.Reason
			}
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
				why = "can't be scheduled: " + c.Message
			}
		}
		return false, nil
	})
	switch {
	case err == nil:
		return ended, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case wctx.Err() != nil && why != "":
		return nil, fmt.Errorf("the %s's pod didn't start within %s (%s)", spec.noun(), p.waitTimeout, why)
	case wctx.Err() != nil:
		return nil, fmt.Errorf("the %s's pod didn't start within %s", spec.noun(), p.waitTimeout)
	}
	return nil, err
}

// waitTaskEnd waits for the pod's status to show how the container ended,
// once its streams have ended: shortly if they failed (attachErr), since
// the container may run on without the driver then.
func (p *Provisioner) waitTaskEnd(ctx context.Context, spec taskSpec, attachErr error) (*corev1.ContainerStateTerminated, error) {
	limit := taskEndTimeout
	if attachErr != nil {
		limit = taskLostStreamTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var ended *corev1.ContainerStateTerminated
	err := wait.PollUntilContextCancel(wctx, p.taskPoll(), true, func(ctx context.Context) (bool, error) {
		pod, err := p.typed.CoreV1().Pods(spec.namespace).Get(ctx, spec.name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if err := podFailure(pod, spec.noun()); err != nil {
			return false, err
		}
		if cs := taskContainerStatus(pod); cs != nil && cs.State.Terminated != nil {
			ended = cs.State.Terminated
			return true, nil
		}
		return false, nil
	})
	switch {
	case err == nil:
		return ended, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case attachErr != nil && wctx.Err() != nil:
		return nil, fmt.Errorf("lost the %s's output: %w", spec.noun(), attachErr)
	case wctx.Err() != nil:
		return nil, fmt.Errorf("the %s's output ended, but its pod doesn't show that it exited within %s", spec.noun(), limit)
	}
	return nil, err
}

// podFailure returns why the pod failed as a whole, or nil. A pod past its
// activeDeadlineSeconds (Task.Timeout) is context.DeadlineExceeded, as a
// timeout is on Compose.
func podFailure(pod *corev1.Pod, noun string) error {
	if pod.Status.Phase != corev1.PodFailed {
		return nil
	}
	if cs := taskContainerStatus(pod); cs != nil && cs.State.Terminated != nil && pod.Status.Reason != "DeadlineExceeded" {
		return nil // The container's own exit says more.
	}
	switch pod.Status.Reason {
	case "DeadlineExceeded":
		return fmt.Errorf("the %s's pod ran past its timeout: %w", noun, context.DeadlineExceeded)
	case "":
		return fmt.Errorf("the %s's pod failed", noun)
	}
	return fmt.Errorf("the %s's pod failed: %s: %s", noun, pod.Status.Reason, pod.Status.Message)
}

func taskContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == taskContainer {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// taskPoll is how often the driver looks at a task's pod: tasks are short,
// so more often than at sites, unless the client polls faster still
// (tests).
func (p *Provisioner) taskPoll() time.Duration { return min(p.poll, taskPollInterval) }

// removeTask deletes the task's pod, at once, and its Secret and
// NetworkPolicy. It runs after ctx may have ended, so it has its own
// deadline; what it can't delete, Startup does.
func (p *Provisioner) removeTask(ctx context.Context, log *slog.Logger, spec taskSpec, secret bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskCleanupTimeout)
	defer cancel()
	now := int64(0)
	logErr := func(kind string, err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			log.ErrorContext(ctx, "removing a task's object failed", "kind", kind, "namespace", spec.namespace, "name", spec.name, "error", err)
		}
	}
	core := p.typed.CoreV1()
	logErr("pod", core.Pods(spec.namespace).Delete(ctx, spec.name, metav1.DeleteOptions{GracePeriodSeconds: &now}))
	if secret {
		logErr("secret", core.Secrets(spec.namespace).Delete(ctx, spec.name, metav1.DeleteOptions{}))
	}
	if spec.policy != nil {
		logErr("networkpolicy", p.typed.NetworkingV1().NetworkPolicies(spec.namespace).Delete(ctx, spec.name, metav1.DeleteOptions{}))
	}
}

// taskPod is the task's pod.
func (p *Provisioner) taskPod(spec taskSpec, task provisioner.Task) *corev1.Pod {
	var env []corev1.EnvVar
	if _, ok := task.Env["HOME"]; !ok {
		env = append(env, corev1.EnvVar{Name: "HOME", Value: taskHome})
	}
	for _, name := range slices.Sorted(maps.Keys(task.Env)) {
		env = append(env, corev1.EnvVar{Name: name, Value: task.Env[name]})
	}
	volumes := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: taskHome}}
	if spec.wpContent != provisioner.MountNone {
		ro := spec.wpContent == provisioner.MountReadOnly
		volumes = append(volumes, corev1.Volume{Name: "wp-content", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "wp-content", ReadOnly: ro},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "wp-content", MountPath: provisioner.TaskWPContentPath, ReadOnly: ro})
	}
	if spec.dataClaim != "" {
		volumes = append(volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: spec.dataClaim},
		}})
		if spec.transferDir != "" {
			mounts = append(mounts, corev1.VolumeMount{Name: "data", MountPath: provisioner.TaskTransferPath, SubPath: spec.transferDir})
		}
		if spec.repositoryDir != "" {
			mounts = append(mounts, corev1.VolumeMount{Name: "data", MountPath: provisioner.TaskRepositoryPath, SubPath: spec.repositoryDir})
		}
	}
	if len(task.SecretFiles) > 0 {
		// Owned by root without fsGroup (see RunTask), so readable by
		// every user of the pod, which runs one container as one user:
		// the task's. Not in the environment or the pod spec.
		mode := int32(0o444)
		volumes = append(volumes, corev1.Volume{Name: "secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: spec.name, DefaultMode: &mode},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "secrets", MountPath: provisioner.TaskSecretsPath, ReadOnly: true})
	}
	var deadline *int64
	if task.Timeout > 0 {
		s := int64(math.Ceil(task.Timeout.Seconds()))
		deadline = &s
	}
	var pullSecrets []corev1.LocalObjectReference
	for _, name := range p.pullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}
	// Request and limit alike: the scheduler reserves what the task may
	// use, and a container whose memory request equals its limit gets no
	// swap where nodes have it (LimitedSwap).
	memory := *resource.NewQuantity(int64(spec.memoryMB)<<20, resource.BinarySI)
	grace := int64(5)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.name, Namespace: spec.namespace, Labels: spec.labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// restic records the host name in its locks.
			Hostname:                      "pf-task-" + spec.token,
			ActiveDeadlineSeconds:         deadline,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  probePtr(false),
			EnableServiceLinks:            probePtr(false),
			ImagePullSecrets:              pullSecrets,
			Affinity:                      spec.affinity,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:       probePtr(true),
				RunAsUser:          probePtr(spec.uid),
				RunAsGroup:         probePtr(spec.uid),
				SupplementalGroups: spec.groups,
				SeccompProfile:     &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            taskContainer,
				Image:           spec.image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         slices.Concat([]string{"sh", "-c", taskShim, "pressfleet-task"}, task.Command),
				Env:             env,
				Stdin:           true,
				StdinOnce:       true,
				VolumeMounts:    mounts,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: memory},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: memory},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: probePtr(false),
					ReadOnlyRootFilesystem:   probePtr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: volumes,
		},
	}
}

// repositoryTaskPolicy is the NetworkPolicy of a repository task, without
// its name and pod selector: no traffic at all, or with backupNetwork,
// egress to DNS, the S3 endpoint, and the internet. Nothing may connect to
// the task.
func (p *Provisioner) repositoryTaskPolicy(backupNetwork bool) *networkingv1.NetworkPolicy {
	pol := &networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
	}}
	if !backupNetwork {
		return pol
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(proto *corev1.Protocol, n int) networkingv1.NetworkPolicyPort {
		v := intstr.FromInt32(int32(n)) //nolint:gosec // G115: ports are below 65536
		return networkingv1.NetworkPolicyPort{Protocol: proto, Port: &v}
	}
	egress := []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{port(&udp, 53), port(&tcp, 53)}}}
	if s3 := p.tasks.s3; s3 != nil {
		peer := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: s3.CIDR}}
		if s3.Namespace != "" {
			peer = networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: s3.Namespace},
			}}
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{peer}, Ports: []networkingv1.NetworkPolicyPort{port(&tcp, s3.Port)},
		})
	}
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: p.egress.ExceptV4}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: p.egress.ExceptV6}},
	}})
	pol.Spec.Egress = egress
	return pol
}

// Startup deletes the task pods, Secrets, and NetworkPolicies that a
// restart of the control plane left in the platform namespace and the site
// namespaces, and the data volume's clones of sites that no longer exist
// (driver.StartupHook). Call it before tasks run, from one process only:
// it can't tell a leftover from a running task's.
func (p *Provisioner) Startup(ctx context.Context) error {
	errs := []error{p.removeStaleTasks(ctx, p.platformNamespace, true)}
	list, err := p.typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + FieldManager + "," + siteIDLabel,
	})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("listing site namespaces: %w", err))...)
	}
	namespaces := map[string]bool{}
	for _, ns := range list.Items {
		if !strings.HasPrefix(ns.Name, p.namespacePrefix) {
			continue
		}
		namespaces[ns.Name] = true
		errs = append(errs, p.removeStaleTasks(ctx, ns.Name, false))
	}
	errs = append(errs, p.removeStaleDataClones(ctx, namespaces))
	return errors.Join(errs...)
}

// removeStaleTasks deletes the task objects in ns.
func (p *Provisioner) removeStaleTasks(ctx context.Context, ns string, policies bool) error {
	opts := metav1.ListOptions{LabelSelector: managedByLabel + "=" + FieldManager + "," + TaskLabel}
	now := int64(0)
	var errs []error
	del := func(kind string, names []string, fn func(string) error) {
		for _, name := range names {
			if err := fn(name); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("deleting %s %s/%s: %w", kind, ns, name, err))
				continue
			}
			p.log.InfoContext(ctx, "removed a task's leftover", "kind", kind, "namespace", ns, "name", name)
		}
	}
	core := p.typed.CoreV1()
	if pods, err := core.Pods(ns).List(ctx, opts); err != nil {
		errs = append(errs, fmt.Errorf("listing task pods in %s: %w", ns, err))
	} else {
		del("pod", objectNames(pods.Items, func(o corev1.Pod) string { return o.Name }), func(name string) error {
			return core.Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &now})
		})
	}
	if secrets, err := core.Secrets(ns).List(ctx, opts); err != nil {
		errs = append(errs, fmt.Errorf("listing task secrets in %s: %w", ns, err))
	} else {
		del("secret", objectNames(secrets.Items, func(o corev1.Secret) string { return o.Name }), func(name string) error {
			return core.Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})
		})
	}
	if policies {
		nps := p.typed.NetworkingV1().NetworkPolicies(ns)
		if list, err := nps.List(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("listing task network policies in %s: %w", ns, err))
		} else {
			del("networkpolicy", objectNames(list.Items, func(o networkingv1.NetworkPolicy) string { return o.Name }), func(name string) error {
				return nps.Delete(ctx, name, metav1.DeleteOptions{})
			})
		}
	}
	return errors.Join(errs...)
}

func objectNames[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = name(it)
	}
	return out
}

// The writer below is written by the attachment's copy goroutines, which
// may outlive a failed attachment, and read by the driver: it locks.

// recordingWriter remembers the first error of the writer it wraps.
type recordingWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.w.Write(p)
	if err != nil {
		r.err = err
	}
	return n, err
}

func (r *recordingWriter) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
