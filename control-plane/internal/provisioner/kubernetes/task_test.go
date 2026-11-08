package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

const testTaskImage = "ghcr.io/rchouinard/pressfleet/control-plane:1.2.0"

// siteDeployment is a site's Deployment as the driver renders it, as far
// as tasks care: its pod template's user.
func siteDeployment(ns string) *appsv1.Deployment {
	uid := int64(provisioner.MinSiteUID)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: SiteDeployment, Namespace: ns},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid},
		}}},
	}
}

// dataVolumeObjects are the chart's data claim and the volume an NFS
// provisioner bound it to.
func dataVolumeObjects() []runtime.Object {
	return []runtime.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: config.DefaultDataClaim, Namespace: "pressfleet"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-data"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-data"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				StorageClassName:              "nfs",
				MountOptions:                  []string{"nfsvers=4.1"},
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           "nfs.csi.k8s.io",
					VolumeHandle:     "nfs.example#exports#pvc-data##",
					VolumeAttributes: map[string]string{"server": "nfs.example", "share": "/exports"},
				}},
				ClaimRef: &corev1.ObjectReference{Namespace: "pressfleet", Name: config.DefaultDataClaim},
			},
		},
	}
}

// fakeTask is what the fake cluster does with a task's pod once the
// driver attaches: what the command writes, and how it ends.
type fakeTask struct {
	stdout, stderr string
	exitCode       int32
	reason         string // the container's, e.g. OOMKilled
	podReason      string // the pod's, e.g. DeadlineExceeded
	// block makes the attachment wait for ctx, as a task that runs on.
	block bool
}

// taskCluster is the driver on a fake cluster with a site, the data volume,
// and pods that start at once and run as fake says.
type taskCluster struct {
	*Provisioner
	t    *testing.T
	fake *kubefake.Clientset

	mu sync.Mutex
	// task is what the next attachment does.
	task fakeTask
	// start, if set, is the status a new task pod gets, in place of a
	// running container.
	start *corev1.PodStatus
	// pods and secrets are the task pods and Secrets as they were when the
	// driver attached.
	pods    []*corev1.Pod
	secrets []*corev1.Secret
	stdin   string
}

func newTaskCluster(t *testing.T, objs ...runtime.Object) *taskCluster {
	t.Helper()
	objs = append(objs, siteDeployment("pf-"+testSiteID))
	objs = append(objs, dataVolumeObjects()...)
	p := cluster(t, func(c *Config) {
		c.Tasks = TaskConfig{Image: testTaskImage, MemoryLimitMB: 512, S3Endpoint: "http://minio.storage.svc:9000"}
	}, objs...)
	tc := &taskCluster{Provisioner: p, t: t, fake: p.typed.(*kubefake.Clientset)}
	// Containers start as soon as their pods exist, and the controller
	// binds a claim to the volume it names.
	tc.fake.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if tc.start != nil {
			pod.Status = *tc.start.DeepCopy()
			return false, nil, nil
		}
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: taskContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}}
		return false, nil, nil
	})
	tc.fake.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim).Status.Phase = corev1.ClaimBound
		return false, nil, nil
	})
	p.attach = tc.attach
	return tc
}

func (tc *taskCluster) setTask(f fakeTask) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.task = f
}

// attach plays the task: it records the pod and its Secret, takes the
// line the shim waits for, writes the output, and ends the container.
func (tc *taskCluster) attach(ctx context.Context, ns, name, container string, s remotecommand.StreamOptions) error {
	tc.mu.Lock()
	f := tc.task
	tc.mu.Unlock()
	core := tc.typed.CoreV1()
	pod, err := core.Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if container != taskContainer {
		tc.t.Errorf("attached to container %q", container)
	}
	secret, serr := core.Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if serr != nil && !apierrors.IsNotFound(serr) {
		return serr
	}
	in, err := io.ReadAll(s.Stdin)
	if err != nil {
		return err
	}
	tc.mu.Lock()
	tc.pods = append(tc.pods, pod)
	if serr == nil {
		tc.secrets = append(tc.secrets, secret)
	}
	tc.stdin = string(in)
	tc.mu.Unlock()

	// Like client-go's streams on a cluster, a failing writer doesn't end
	// the attachment: the task runs on until ctx ends (#228).
	_, _ = io.WriteString(s.Stdout, f.stdout)
	if _, err := io.WriteString(s.Stderr, f.stderr); err != nil {
		return err
	}
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: f.exitCode, Reason: f.reason,
	}}
	if f.podReason != "" {
		pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, f.podReason
	}
	_, err = core.Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	return err
}

// lastPod is the pod of the task that ran last.
func (tc *taskCluster) lastPod() *corev1.Pod {
	tc.t.Helper()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if len(tc.pods) == 0 {
		tc.t.Fatal("no task ran")
	}
	return tc.pods[len(tc.pods)-1]
}

// wantNoTaskObjects checks that no task's pod, Secret, or NetworkPolicy is
// left in ns.
func (tc *taskCluster) wantNoTaskObjects(ns string) {
	tc.t.Helper()
	ctx := context.Background()
	opts := metav1.ListOptions{LabelSelector: TaskLabel}
	pods, _ := tc.typed.CoreV1().Pods(ns).List(ctx, opts)
	secrets, _ := tc.typed.CoreV1().Secrets(ns).List(ctx, opts)
	policies, _ := tc.typed.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if n := len(pods.Items) + len(secrets.Items) + len(policies.Items); n > 0 {
		tc.t.Errorf("%d task objects left in %s", n, ns)
	}
}

// transferDir makes a transfer directory below the data directory's tmp/.
func (tc *taskCluster) transferDir(name string) string {
	tc.t.Helper()
	dir := filepath.Join(tc.layout.TmpDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tc.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dump.sql"), []byte("-- dump"), 0o600); err != nil {
		tc.t.Fatal(err)
	}
	return dir
}

func (tc *taskCluster) repositoryDir() string {
	tc.t.Helper()
	dir := filepath.Join(tc.layout.Root, "backups", "repositories", "01hzy3k0000000000000000000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tc.t.Fatal(err)
	}
	return dir
}

func volume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func mountAt(pod *corev1.Pod, path string) *corev1.VolumeMount {
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if m := &pod.Spec.Containers[0].VolumeMounts[i]; m.MountPath == path {
			return m
		}
	}
	return nil
}

func envOf(pod *corev1.Pod) map[string]string {
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestRunTask(t *testing.T) {
	tc := newTaskCluster(t)
	ctx := t.Context()
	ns := "pf-" + testSiteID
	const secret = "pf-task-secret-Value-9"
	transfer := tc.transferDir("backup-1")
	repo := tc.repositoryDir()
	tc.setTask(fakeTask{stdout: `{"message_type":"summary"}` + "\n", stderr: "to-stderr\n"})

	res, err := tc.RunTask(ctx, testSiteID, provisioner.Task{
		Command:       []string{"restic", "backup", "--json", provisioner.TaskWPContentPath},
		Env:           map[string]string{"RESTIC_PASSWORD_FILE": provisioner.TaskSecretPath("password"), "GOMEMLIMIT": "460MiB"},
		SecretFiles:   map[string]redact.Secret{"password": secret},
		WPContent:     provisioner.MountReadOnly,
		TransferDir:   transfer,
		RepositoryDir: repo,
		Timeout:       90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != `{"message_type":"summary"}`+"\n" || res.Stderr != "to-stderr\n" {
		t.Errorf("result = %+v", res)
	}
	if tc.stdin != "\n" {
		t.Errorf("the driver sent %q to release the shim, want a line", tc.stdin)
	}

	pod := tc.lastPod()
	spec := pod.Spec
	c := spec.Containers[0]
	uid := int64(provisioner.MinSiteUID)
	if pod.Namespace != ns || pod.Labels[TaskLabel] == "" || pod.Labels[ComponentLabel] != TaskComponent ||
		pod.Labels[siteIDLabel] != testSiteID || pod.Labels[managedByLabel] != FieldManager {
		t.Errorf("pod %s/%s labels = %v", pod.Namespace, pod.Name, pod.Labels)
	}
	if _, ok := pod.Labels["app.kubernetes.io/component"]; ok {
		t.Error("a task pod carries the chart's component label")
	}
	sc := spec.SecurityContext
	if *sc.RunAsUser != uid || *sc.RunAsGroup != uid || !*sc.RunAsNonRoot || sc.FSGroup != nil ||
		!slices.Contains(sc.SupplementalGroups, provisioner.RepositoryGID) {
		t.Errorf("pod security context = %+v", sc)
	}
	if gid := int64(os.Getegid()); gid != 0 && gid != uid && !slices.Contains(sc.SupplementalGroups, gid) {
		t.Errorf("supplemental groups %v lack the control plane's group %d, which shares the transfer directory", sc.SupplementalGroups, gid)
	}
	if *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks || spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod spec = %+v", spec)
	}
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 90 {
		t.Errorf("activeDeadlineSeconds = %v, want 90", spec.ActiveDeadlineSeconds)
	}
	if !strings.HasPrefix(spec.Hostname, "pf-task-") || len(spec.ImagePullSecrets) != 1 {
		t.Errorf("hostname %q, pull secrets %v", spec.Hostname, spec.ImagePullSecrets)
	}
	if c.Image != testTaskImage || !c.Stdin || !c.StdinOnce || c.TTY ||
		!slices.Equal(c.Command, []string{"sh", "-c", taskShim, "pressfleet-task", "restic", "backup", "--json", provisioner.TaskWPContentPath}) {
		t.Errorf("container = %+v", c)
	}
	csc := c.SecurityContext
	if *csc.AllowPrivilegeEscalation || !*csc.ReadOnlyRootFilesystem || !slices.Equal(csc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container security context = %+v", csc)
	}
	limit, request := c.Resources.Limits[corev1.ResourceMemory], c.Resources.Requests[corev1.ResourceMemory]
	if limit.Value() != 512<<20 || request.Cmp(limit) != 0 {
		t.Errorf("memory request %s, limit %s; want 512Mi both", request.String(), limit.String())
	}
	// The environment reaches the container unchanged, GOMEMLIMIT (#281)
	// included, with HOME on the emptyDir.
	if env := envOf(pod); env["GOMEMLIMIT"] != "460MiB" || env["HOME"] != taskHome ||
		env["RESTIC_PASSWORD_FILE"] != provisioner.TaskSecretPath("password") || len(env) != 3 {
		t.Errorf("env = %v", env)
	}
	if m := mountAt(pod, provisioner.TaskWPContentPath); m == nil || !m.ReadOnly || !volume(pod, m.Name).PersistentVolumeClaim.ReadOnly ||
		volume(pod, m.Name).PersistentVolumeClaim.ClaimName != "wp-content" {
		t.Errorf("wp-content mount = %+v", m)
	}
	if m := mountAt(pod, provisioner.TaskTransferPath); m == nil || m.SubPath != "tmp/backup-1" ||
		volume(pod, m.Name).PersistentVolumeClaim.ClaimName != SiteDataClaim {
		t.Errorf("transfer mount = %+v", m)
	}
	if m := mountAt(pod, provisioner.TaskRepositoryPath); m == nil || m.SubPath != "backups/repositories/01hzy3k0000000000000000000" {
		t.Errorf("repository mount = %+v", m)
	}
	m := mountAt(pod, provisioner.TaskSecretsPath)
	if m == nil || !m.ReadOnly {
		t.Fatalf("secrets mount = %+v", m)
	}
	sv := volume(pod, m.Name).Secret
	if sv.SecretName != pod.Name || *sv.DefaultMode != 0o444 {
		t.Errorf("secret volume = %+v", sv)
	}
	// The secret is in the Secret, and nowhere in the pod.
	if b, _ := json.Marshal(pod); strings.Contains(string(b), secret) {
		t.Error("the pod spec holds the secret")
	}
	if len(tc.secrets) != 1 || string(tc.secrets[0].Data["password"]) != secret || tc.secrets[0].Labels[TaskLabel] != pod.Labels[TaskLabel] {
		t.Errorf("task secrets = %v", tc.secrets)
	}
	// Nothing is left but the clone of the data volume and its claim.
	tc.wantNoTaskObjects(ns)
	clone, err := tc.typed.CoreV1().PersistentVolumes().Get(ctx, DataCloneName(ns), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if clone.Spec.CSI == nil || clone.Spec.CSI.VolumeHandle != "nfs.example#exports#pvc-data##" ||
		clone.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || clone.Spec.StorageClassName != "" ||
		clone.Spec.ClaimRef.Namespace != ns || clone.Spec.ClaimRef.Name != SiteDataClaim ||
		!slices.Equal(clone.Spec.MountOptions, []string{"nfsvers=4.1"}) || clone.Labels[DataCloneLabel] != "pressfleet" {
		t.Errorf("clone = %+v", clone)
	}
	data, _ := tc.typed.CoreV1().PersistentVolumes().Get(ctx, "pvc-data", metav1.GetOptions{})
	if data.Labels[DataVolumeLabel] != "pressfleet" {
		t.Errorf("data volume labels = %v", data.Labels)
	}
	// The transfer directory is the control plane group's.
	for _, p := range []string{transfer, filepath.Join(transfer, "dump.sql")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o060 != 0o060 {
			t.Errorf("%s mode %s, want group read and write", p, fi.Mode())
		}
		if fi.IsDir() && fi.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s mode %s, want setgid", p, fi.Mode())
		}
	}
}

func TestRunTaskScheduling(t *testing.T) {
	site := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "site-1", Namespace: "pf-" + testSiteID, Labels: map[string]string{ComponentLabel: SiteComponent},
	}}
	tests := []struct {
		name         string
		objs         []runtime.Object
		mode         provisioner.MountMode
		wantAffinity bool
	}{
		{"running site", []runtime.Object{site}, provisioner.MountReadWrite, true},
		{"stopped site", nil, provisioner.MountReadWrite, false},
		{"no wp-content", []runtime.Object{site}, provisioner.MountNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTaskCluster(t, tt.objs...)
			if _, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"true"}, WPContent: tt.mode}); err != nil {
				t.Fatal(err)
			}
			pod := tc.lastPod()
			aff := pod.Spec.Affinity
			if !tt.wantAffinity {
				if aff != nil {
					t.Errorf("affinity = %+v, want none", aff)
				}
				return
			}
			terms := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if len(terms) != 1 || terms[0].TopologyKey != corev1.LabelHostname ||
				terms[0].LabelSelector.MatchLabels[ComponentLabel] != SiteComponent {
				t.Errorf("affinity = %+v", aff)
			}
			// No data volume without a transfer or repository directory.
			if volume(pod, "data") != nil {
				t.Error("a task without directories mounts the data volume")
			}
		})
	}
}

func TestRunTaskExit(t *testing.T) {
	tests := []struct {
		name     string
		task     fakeTask
		wantCode int
		wantErr  string
		wantIs   error
	}{
		{name: "non-zero", task: fakeTask{stdout: "partial\n", stderr: "oops\n", exitCode: 4}, wantCode: 4, wantErr: "oops"},
		{name: "out of memory", task: fakeTask{exitCode: 137, reason: "OOMKilled"}, wantCode: 137, wantErr: "memory limit of 512 MiB (OOMKilled)"},
		{name: "deadline", task: fakeTask{exitCode: 137, podReason: "DeadlineExceeded"}, wantIs: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTaskCluster(t)
			tc.setTask(tt.task)
			_, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"sh", "-c", "exit 4"}})
			if tt.wantIs != nil {
				if !errors.Is(err, tt.wantIs) {
					t.Errorf("err = %v, want %v", err, tt.wantIs)
				}
			} else {
				var ee *provisioner.ExecError
				if !errors.As(err, &ee) || ee.ExitCode != tt.wantCode || !strings.Contains(ee.Stderr, tt.wantErr) || ee.Stdout != tt.task.stdout {
					t.Errorf("err = %#v, want exit %d with %q", err, tt.wantCode, tt.wantErr)
				}
			}
			tc.wantNoTaskObjects("pf-" + testSiteID)
		})
	}
}

type failingWriter struct{}

var errWrite = errors.New("the client went away")

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestRunTaskOutput(t *testing.T) {
	ns := "pf-" + testSiteID
	t.Run("streamed", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.setTask(fakeTask{stdout: "archive bytes"})
		var out strings.Builder
		res, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"tar", "c", "."}, Stdout: &out})
		if err != nil || res.Stdout != "" || out.String() != "archive bytes" {
			t.Errorf("RunTask = %+v, %v; streamed %q", res, err, out.String())
		}
	})
	t.Run("write error", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.setTask(fakeTask{stdout: "archive bytes", block: true})
		_, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"tar", "c", "."}, Stdout: failingWriter{}})
		if !errors.Is(err, errWrite) {
			t.Errorf("err = %v, want the writer's", err)
		}
		tc.wantNoTaskObjects(ns)
	})
	t.Run("truncated", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.setTask(fakeTask{stdout: strings.Repeat("x", taskMaxOutput+10)})
		res, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"yes"}})
		if !errors.Is(err, ErrOutputTruncated) || res == nil || len(res.Stdout) != taskMaxOutput {
			t.Errorf("RunTask = %v; want ErrOutputTruncated with the first %d bytes", err, taskMaxOutput)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.setTask(fakeTask{block: true})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := tc.RunTask(ctx, testSiteID, provisioner.Task{Command: []string{"sleep", "300"}})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		tc.wantNoTaskObjects(ns)
	})
	t.Run("timeout", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.setTask(fakeTask{block: true})
		_, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"sleep", "300"}, Timeout: 50 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		tc.wantNoTaskObjects(ns)
	})
}

func TestRunTaskStart(t *testing.T) {
	t.Run("image pull failure", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.start = &corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: taskContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "not found"}},
		}}}
		_, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"true"}})
		if err == nil || !strings.Contains(err.Error(), "ErrImagePull") {
			t.Errorf("err = %v", err)
		}
		tc.wantNoTaskObjects("pf-" + testSiteID)
	})
	t.Run("shim failed", func(t *testing.T) {
		tc := newTaskCluster(t)
		tc.start = &corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: taskContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 127}},
		}}}
		_, err := tc.RunTask(t.Context(), testSiteID, provisioner.Task{Command: []string{"true"}})
		var ee *provisioner.ExecError
		if !errors.As(err, &ee) || ee.ExitCode != 127 {
			t.Errorf("err = %v, want exit 127", err)
		}
	})
}

func TestRunTaskInvalid(t *testing.T) {
	tc := newTaskCluster(t)
	outside := t.TempDir()
	for name, task := range map[string]provisioner.Task{
		"no command":         {},
		"transfer elsewhere": {Command: []string{"true"}, TransferDir: outside},
		"repository at root": {Command: []string{"true"}, RepositoryDir: tc.layout.Root},
		"secret name":        {Command: []string{"true"}, SecretFiles: map[string]redact.Secret{"../x": "s"}},
		"memory":             {Command: []string{"true"}, MemoryLimitMB: 1},
		"mount mode":         {Command: []string{"true"}, WPContent: 7},
	} {
		if _, err := tc.RunTask(t.Context(), testSiteID, task); !errors.Is(err, ErrInvalidTask) {
			t.Errorf("%s: err = %v, want ErrInvalidTask", name, err)
		}
	}
	if _, err := tc.RunTask(t.Context(), "01hzy3k5q7m9c2v4b6n8r0t1zz", provisioner.Task{Command: []string{"true"}}); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: err = %v, want ErrSiteNotFound", err)
	}
	if len(tc.pods) != 0 {
		t.Errorf("%d pods ran", len(tc.pods))
	}
}

func TestRunRepositoryTask(t *testing.T) {
	tc := newTaskCluster(t)
	ctx := t.Context()
	repo := tc.repositoryDir()
	// The policy must exist before the pod does.
	var order []string
	tc.fake.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, a.GetResource().Resource)
		return false, nil, nil
	})
	tc.setTask(fakeTask{stdout: "snapshots\n"})
	res, err := tc.RunRepositoryTask(ctx, "01hzy3k0000000000000000000", provisioner.Task{
		Command:       []string{"restic", "snapshots"},
		RepositoryDir: repo,
		SecretFiles:   map[string]redact.Secret{"password": "pw"},
	})
	if err != nil || res.Stdout != "snapshots\n" {
		t.Fatalf("RunRepositoryTask = %+v, %v", res, err)
	}
	if !slices.Equal(order, []string{"networkpolicies", "secrets", "pods"}) {
		t.Errorf("created %v, want the policy first", order)
	}
	pod := tc.lastPod()
	sc := pod.Spec.SecurityContext
	if pod.Namespace != "pressfleet" || *sc.RunAsUser != provisioner.RepositoryGID || *sc.RunAsGroup != provisioner.RepositoryGID ||
		pod.Labels[repositoryLabel] != "01hzy3k0000000000000000000" || pod.Labels[siteIDLabel] != "" {
		t.Errorf("pod %s/%s, labels %v, security %+v", pod.Namespace, pod.Name, pod.Labels, sc)
	}
	for k := range pod.Labels {
		if strings.HasPrefix(k, "app.kubernetes.io/") && k != managedByLabel {
			t.Errorf("a repository task pod carries the chart's label %s", k)
		}
	}
	if m := mountAt(pod, provisioner.TaskRepositoryPath); m == nil || m.SubPath != "backups/repositories/01hzy3k0000000000000000000" ||
		volume(pod, m.Name).PersistentVolumeClaim.ClaimName != config.DefaultDataClaim {
		t.Errorf("repository mount = %+v", m)
	}
	if volume(pod, "wp-content") != nil || pod.Spec.Affinity != nil {
		t.Error("a repository task mounts wp-content or follows a site")
	}
	// No clone in the platform namespace: the claim is there already.
	if _, err := tc.typed.CoreV1().PersistentVolumes().Get(ctx, DataCloneName("pressfleet"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("clone for the platform namespace: %v", err)
	}
	tc.wantNoTaskObjects("pressfleet")

	// Two runs have distinct host names (restic's locks).
	if _, err := tc.RunRepositoryTask(ctx, provisioner.StateRepository, provisioner.Task{Command: []string{"hostname"}, RepositoryDir: repo}); err != nil {
		t.Fatal(err)
	}
	if a, b := tc.pods[0].Spec.Hostname, tc.pods[1].Spec.Hostname; a == b {
		t.Errorf("host names %q and %q", a, b)
	}

	for name, task := range map[string]provisioner.Task{
		"wp-content":    {Command: []string{"true"}, RepositoryDir: repo, WPContent: provisioner.MountReadOnly},
		"no repository": {Command: []string{"true"}},
	} {
		if _, err := tc.RunRepositoryTask(ctx, "01hzy3k0000000000000000000", task); !errors.Is(err, ErrInvalidTask) {
			t.Errorf("%s: err = %v, want ErrInvalidTask", name, err)
		}
	}
	if _, err := tc.RunRepositoryTask(ctx, "../etc", provisioner.Task{Command: []string{"true"}, RepositoryDir: repo}); !errors.Is(err, ErrInvalidTask) {
		t.Errorf("bad repository name: err = %v", err)
	}
}

func TestRepositoryTaskPolicy(t *testing.T) {
	tc := newTaskCluster(t)
	var policies []*networkingv1.NetworkPolicy
	tc.fake.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		policies = append(policies, a.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).DeepCopy())
		return false, nil, nil
	})
	repo := tc.repositoryDir()
	for _, s3 := range []bool{false, true} {
		task := provisioner.Task{Command: []string{"true"}, BackupNetwork: s3}
		if !s3 {
			task.RepositoryDir = repo
		}
		if _, err := tc.RunRepositoryTask(t.Context(), "01hzy3k0000000000000000000", task); err != nil {
			t.Fatal(err)
		}
	}
	if len(policies) != 2 {
		t.Fatalf("%d policies", len(policies))
	}
	for i, pol := range policies {
		pod := tc.pods[i]
		if pol.Spec.PodSelector.MatchLabels[TaskLabel] != pod.Labels[TaskLabel] || len(pol.Spec.PodSelector.MatchLabels) != 1 ||
			!slices.Equal(pol.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) ||
			len(pol.Spec.Ingress) != 0 {
			t.Errorf("policy %d = %+v", i, pol.Spec)
		}
	}
	if len(policies[0].Spec.Egress) != 0 {
		t.Errorf("local repository task egress = %+v, want none", policies[0].Spec.Egress)
	}
	egress := policies[1].Spec.Egress
	if len(egress) != 3 {
		t.Fatalf("S3 repository task egress = %+v", egress)
	}
	if egress[0].Ports[0].Port.IntValue() != 53 {
		t.Errorf("first rule = %+v, want DNS", egress[0])
	}
	if ns := egress[1].To[0].NamespaceSelector; ns == nil || ns.MatchLabels[corev1.LabelMetadataName] != "storage" || egress[1].Ports[0].Port.IntValue() != 9000 {
		t.Errorf("S3 rule = %+v, want namespace storage, port 9000", egress[1])
	}
	if internet := egress[2].To[0].IPBlock; internet == nil || internet.CIDR != "0.0.0.0/0" || !slices.Contains(internet.Except, "10.0.0.0/8") {
		t.Errorf("internet rule = %+v", egress[2])
	}
}

func TestResolveS3Peer(t *testing.T) {
	tests := []struct {
		endpoint string
		want     *s3Peer
	}{
		{"https://s3.eu-west-1.amazonaws.com", nil},
		{"https://203.0.113.10", nil},
		{"http://minio:9000", &s3Peer{Namespace: "pressfleet", Port: 9000}},
		{"http://minio.storage.svc.cluster.local", &s3Peer{Namespace: "storage", Port: 80}},
		{"https://10.0.5.4:9443", &s3Peer{CIDR: "10.0.5.4/32", Port: 9443}},
		{"https://[fd00::4]", &s3Peer{CIDR: "fd00::4/128", Port: 443}},
		{"", nil},
	}
	for _, tt := range tests {
		got := resolveS3Peer(tt.endpoint, "pressfleet")
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("resolveS3Peer(%q) = %+v, want %+v", tt.endpoint, got, tt.want)
		}
	}
}

func TestStartup(t *testing.T) {
	site := "pf-" + testSiteID
	taskLabels := map[string]string{managedByLabel: FieldManager, TaskLabel: "abc"}
	meta := func(ns, name string, labels map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}
	}
	clone := func(ns string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: DataCloneName(ns), Labels: map[string]string{DataCloneLabel: "pressfleet"}},
			Spec:       corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: ns, Name: SiteDataClaim}},
		}
	}
	tc := newTaskCluster(t,
		&corev1.Namespace{ObjectMeta: meta("", site, map[string]string{managedByLabel: FieldManager, siteIDLabel: testSiteID})},
		&corev1.Pod{ObjectMeta: meta(site, "task-abc", taskLabels)},
		&corev1.Secret{ObjectMeta: meta(site, "task-abc", taskLabels)},
		&corev1.Pod{ObjectMeta: meta(site, "site-1", map[string]string{ComponentLabel: SiteComponent})},
		&corev1.Pod{ObjectMeta: meta("pressfleet", "task-def", taskLabels)},
		&corev1.Secret{ObjectMeta: meta("pressfleet", "task-def", taskLabels)},
		&networkingv1.NetworkPolicy{ObjectMeta: meta("pressfleet", "task-def", taskLabels)},
		&corev1.Pod{ObjectMeta: meta("pressfleet", "control-plane-1", map[string]string{"app.kubernetes.io/component": "control-plane"})},
		clone(site),
		clone("pf-gone"),
	)
	if err := tc.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	tc.wantNoTaskObjects(site)
	tc.wantNoTaskObjects("pressfleet")
	core := tc.typed.CoreV1()
	for _, k := range [][2]string{{site, "site-1"}, {"pressfleet", "control-plane-1"}} {
		if _, err := core.Pods(k[0]).Get(t.Context(), k[1], metav1.GetOptions{}); err != nil {
			t.Errorf("pod %s/%s: %v", k[0], k[1], err)
		}
	}
	if _, err := core.PersistentVolumes().Get(t.Context(), DataCloneName(site), metav1.GetOptions{}); err != nil {
		t.Errorf("the live site's clone: %v", err)
	}
	if _, err := core.PersistentVolumes().Get(t.Context(), DataCloneName("pf-gone"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the deleted site's clone: %v, want it gone", err)
	}
}
