package kubernetes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// denyAll is the site namespace's default-deny NetworkPolicy, as the
// driver renders it.
func denyAll() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: denyAllPolicy, Namespace: "pf-" + testSiteID},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// newSessionCluster is a task cluster whose site namespace has its
// default-deny policy.
func newSessionCluster(t *testing.T, objs ...runtime.Object) *taskCluster {
	t.Helper()
	return newTaskCluster(t, append(objs, denyAll())...)
}

// runSession runs a session of command on the test site with stdin as its
// input, and returns what it wrote.
func runSession(ctx context.Context, tc *taskCluster, stdin string, command ...string) (stdout, stderr string, err error) {
	var out, errOut strings.Builder
	err = tc.RunSession(ctx, testSiteID, provisioner.Session{
		Command: command, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
	})
	return out.String(), errOut.String(), err
}

func TestRunSession(t *testing.T) {
	ns := "pf-" + testSiteID
	tc := newSessionCluster(t)
	tc.setTask(fakeTask{stdout: "packets", stderr: "notice\n"})
	stdout, stderr, err := runSession(t.Context(), tc, "client bytes", "/usr/local/bin/pressfleet", "sftp-server")
	if err != nil || stdout != "packets" || stderr != "notice\n" {
		t.Fatalf("RunSession = %q, %q, %v", stdout, stderr, err)
	}
	// The shim's line, then the client's bytes, on one stream.
	if tc.stdin != "\nclient bytes" {
		t.Errorf("standard input = %q", tc.stdin)
	}
	tc.wantNoTaskObjects(ns)

	pod := tc.lastPod()
	if pod.Namespace != ns || !strings.HasPrefix(pod.Name, "session-") {
		t.Errorf("pod %s/%s", pod.Namespace, pod.Name)
	}
	if l := pod.Labels; l[ComponentLabel] != SessionComponent || l[TaskLabel] == "" ||
		l[siteIDLabel] != testSiteID || l[managedByLabel] != FieldManager {
		t.Errorf("labels = %v", l)
	}
	spec := pod.Spec
	uid := int64(provisioner.MinSiteUID)
	if sc := spec.SecurityContext; *sc.RunAsUser != uid || *sc.RunAsGroup != uid || !*sc.RunAsNonRoot ||
		len(sc.SupplementalGroups) != 0 || sc.FSGroup != nil ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", sc)
	}
	if *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks || spec.RestartPolicy != corev1.RestartPolicyNever ||
		spec.ActiveDeadlineSeconds != nil || !strings.HasPrefix(spec.Hostname, "pf-session-") {
		t.Errorf("pod spec = %+v", spec)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("%d containers", len(spec.Containers))
	}
	c := spec.Containers[0]
	wantCmd := []string{"sh", "-c", sessionShim, "pressfleet-session", "/usr/local/bin/pressfleet", "sftp-server"}
	if !slices.Equal(c.Command, wantCmd) || len(c.Args) != 0 || c.Image != testTaskImage {
		t.Errorf("container runs %q %q from %s", c.Command, c.Args, c.Image)
	}
	if !c.Stdin || !c.StdinOnce || c.TTY {
		t.Errorf("stdin %v, stdinOnce %v, tty %v; want stdin closed with the session's", c.Stdin, c.StdinOnce, c.TTY)
	}
	if sc := c.SecurityContext; *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 {
		t.Errorf("container security context = %+v", sc)
	}
	memory := resource.MustParse("128Mi")
	if r := c.Resources; !r.Limits.Memory().Equal(memory) || !r.Requests.Memory().Equal(memory) {
		t.Errorf("resources = %+v, want 128Mi", r)
	}
	if env := envOf(pod); len(env) != 1 || env["HOME"] != taskHome {
		t.Errorf("env = %v, want HOME only", env)
	}
	// The site's wp-content read-write, a small /tmp in memory, and nothing
	// else.
	if names := slices.Sorted(func(yield func(string) bool) {
		for _, v := range spec.Volumes {
			if !yield(v.Name) {
				return
			}
		}
	}); !slices.Equal(names, []string{"tmp", "wp-content"}) {
		t.Errorf("volumes = %v", names)
	}
	wp := volume(pod, "wp-content")
	if wp.PersistentVolumeClaim == nil || wp.PersistentVolumeClaim.ClaimName != "wp-content" || wp.PersistentVolumeClaim.ReadOnly {
		t.Errorf("wp-content volume = %+v", wp)
	}
	if m := mountAt(pod, provisioner.TaskWPContentPath); m == nil || m.Name != "wp-content" || m.ReadOnly {
		t.Errorf("wp-content mount = %+v", m)
	}
	tmp := volume(pod, "tmp").EmptyDir
	if tmp == nil || tmp.Medium != corev1.StorageMediumMemory || tmp.SizeLimit == nil || tmp.SizeLimit.Value() != sessionTmpSize {
		t.Errorf("tmp volume = %+v", tmp)
	}
	if m := mountAt(pod, taskHome); m == nil || m.Name != "tmp" {
		t.Errorf("tmp mount = %+v", m)
	}
}

func TestRunSessionScheduling(t *testing.T) {
	site := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "site-1", Namespace: "pf-" + testSiteID, Labels: map[string]string{ComponentLabel: SiteComponent},
	}}
	t.Run("running site", func(t *testing.T) {
		tc := newSessionCluster(t, site)
		if _, _, err := runSession(t.Context(), tc, "", "true"); err != nil {
			t.Fatal(err)
		}
		aff := tc.lastPod().Spec.Affinity
		if aff == nil || aff.PodAffinity == nil {
			t.Fatalf("affinity = %+v, want the site's pod's node", aff)
		}
		terms := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if len(terms) != 1 || terms[0].TopologyKey != corev1.LabelHostname ||
			terms[0].LabelSelector.MatchLabels[ComponentLabel] != SiteComponent {
			t.Errorf("affinity = %+v", aff)
		}
	})
	t.Run("stopped site", func(t *testing.T) {
		tc := newSessionCluster(t)
		if _, _, err := runSession(t.Context(), tc, "", "true"); err != nil {
			t.Fatal(err)
		}
		if aff := tc.lastPod().Spec.Affinity; aff != nil {
			t.Errorf("affinity = %+v, want none: the volume's topology places it", aff)
		}
	})
}

func TestRunSessionEnd(t *testing.T) {
	ns := "pf-" + testSiteID
	t.Run("non-zero exit", func(t *testing.T) {
		tc := newSessionCluster(t)
		tc.setTask(fakeTask{stderr: "oops\n", exitCode: 7})
		var out strings.Builder
		err := tc.RunSession(t.Context(), testSiteID, provisioner.Session{
			Command: []string{"false"}, Stdin: strings.NewReader(""), Stdout: &out,
		})
		var ee *provisioner.ExecError
		if !errors.As(err, &ee) || ee.ExitCode != 7 || ee.Stderr != "oops\n" {
			t.Errorf("err = %#v, want exit 7 with the standard error kept", err)
		}
		tc.wantNoTaskObjects(ns)
	})
	t.Run("out of memory", func(t *testing.T) {
		tc := newSessionCluster(t)
		tc.setTask(fakeTask{exitCode: 137, reason: "OOMKilled"})
		_, _, err := runSession(t.Context(), tc, "", "true")
		var ee *provisioner.ExecError
		if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "memory limit of 128 MiB (OOMKilled)") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		tc := newSessionCluster(t)
		tc.setTask(fakeTask{block: true})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _, err := runSession(ctx, tc, "", "sleep", "300")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		tc.wantNoTaskObjects(ns)
	})
	t.Run("client gone", func(t *testing.T) {
		tc := newSessionCluster(t)
		tc.setTask(fakeTask{stdout: "packets", block: true})
		err := tc.RunSession(t.Context(), testSiteID, provisioner.Session{
			Command: []string{"cat"}, Stdin: strings.NewReader(""), Stdout: failingWriter{},
		})
		if !errors.Is(err, errWrite) {
			t.Errorf("err = %v, want the writer's", err)
		}
		tc.wantNoTaskObjects(ns)
	})
	t.Run("pod can't start", func(t *testing.T) {
		tc := newSessionCluster(t)
		tc.start = &corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: taskContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "not found"}},
		}}}
		_, _, err := runSession(t.Context(), tc, "", "true")
		if err == nil || !strings.Contains(err.Error(), "the session's container can't start: ErrImagePull") {
			t.Errorf("err = %v", err)
		}
		tc.wantNoTaskObjects(ns)
	})
}

func TestRunSessionRefuses(t *testing.T) {
	tc := newSessionCluster(t)
	in, out := strings.NewReader(""), &strings.Builder{}
	for name, s := range map[string]provisioner.Session{
		"no command":   {Stdin: in, Stdout: out},
		"empty argv0":  {Command: []string{""}, Stdin: in, Stdout: out},
		"NUL argument": {Command: []string{"sh", "a\x00b"}, Stdin: in, Stdout: out},
		"no stdin":     {Command: []string{"true"}, Stdout: out},
		"no stdout":    {Command: []string{"true"}, Stdin: in},
		"bad image":    {Image: "bad image", Command: []string{"true"}, Stdin: in, Stdout: out},
	} {
		if err := tc.RunSession(t.Context(), testSiteID, s); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("%s: err = %v, want ErrInvalidSession", name, err)
		}
	}
	valid := provisioner.Session{Command: []string{"true"}, Stdin: in, Stdout: out}
	if err := tc.RunSession(t.Context(), "01hzy3k5q7m9c2v4b6n8r0t1zz", valid); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site: err = %v, want ErrSiteNotFound", err)
	}
	if err := tc.RunSession(t.Context(), "Not-A-Site", valid); !errors.Is(err, ErrInvalidSite) {
		t.Errorf("invalid site ID: err = %v, want ErrInvalidSite", err)
	}
	if len(tc.pods) != 0 {
		t.Errorf("%d pods ran", len(tc.pods))
	}
}

// A session relies on the namespace's default-deny policy for having no
// network, so it doesn't start without it.
func TestRunSessionNeedsDenyAll(t *testing.T) {
	widened := denyAll()
	widened.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	selective := denyAll()
	selective.Spec.PodSelector.MatchLabels = map[string]string{ComponentLabel: SiteComponent}
	ingressOnly := denyAll()
	ingressOnly.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	for name, objs := range map[string][]runtime.Object{
		"missing":      nil,
		"egress":       {widened},
		"selective":    {selective},
		"ingress only": {ingressOnly},
	} {
		t.Run(name, func(t *testing.T) {
			tc := newTaskCluster(t, objs...)
			_, _, err := runSession(t.Context(), tc, "", "true")
			if err == nil || !strings.Contains(err.Error(), denyAllPolicy) {
				t.Errorf("err = %v, want a refusal naming %s", err, denyAllPolicy)
			}
			pods, _ := tc.typed.CoreV1().Pods("pf-"+testSiteID).List(t.Context(), metav1.ListOptions{})
			if len(pods.Items) != 0 || len(tc.pods) != 0 {
				t.Errorf("a session pod was created without isolation")
			}
		})
	}
}

// Of the policies a site's namespace gets, only default-deny selects a
// session pod: none allows it any traffic, as the site-egress policy
// allows tasks.
func TestSessionPodIsIsolated(t *testing.T) {
	p := cluster(t, nil)
	ctx := t.Context()
	if _, err := p.CreateSite(ctx, testSite()); err != nil {
		t.Fatal(err)
	}
	ns := p.SiteNamespace(testSiteID)
	pod := p.sessionPod(taskSpec{
		namespace: ns, name: "session-abc", token: "abc", uid: provisioner.MinSiteUID,
		image: testTaskImage, memoryMB: sessionMemoryMB, wpContent: provisioner.MountReadWrite, session: true,
		labels: map[string]string{managedByLabel: FieldManager, siteIDLabel: testSiteID, TaskLabel: "abc", ComponentLabel: SessionComponent},
	}, []string{"true"})
	policies, err := p.typed.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(policies.Items, func(pol networkingv1.NetworkPolicy) bool { return pol.Name == "site-egress" }) {
		t.Fatalf("the site has no site-egress policy to compare with")
	}
	var selecting []string
	for _, pol := range policies.Items {
		sel, err := metav1.LabelSelectorAsSelector(&pol.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			selecting = append(selecting, pol.Name)
		}
	}
	if !slices.Equal(selecting, []string{denyAllPolicy}) {
		t.Errorf("policies selecting a session pod = %v, want %s alone", selecting, denyAllPolicy)
	}
	// The rendered default-deny passes RunSession's check.
	if err := p.checkDenyAll(ctx, ns); err != nil {
		t.Error(err)
	}
}

func TestStartupRemovesSessions(t *testing.T) {
	site := "pf-" + testSiteID
	tc := newSessionCluster(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: site, Labels: map[string]string{managedByLabel: FieldManager, siteIDLabel: testSiteID}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: site, Name: "session-abc", Labels: map[string]string{
			managedByLabel: FieldManager, TaskLabel: "abc", ComponentLabel: SessionComponent,
		}}},
	)
	if err := tc.Startup(t.Context()); err != nil {
		t.Fatal(err)
	}
	tc.wantNoTaskObjects(site)
}
