package kubernetes

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// PullSecretLabel marks the copies of the install's image pull Secrets
// (kubernetes.image_pull_secrets) in a site's namespace, with the value
// "true".
const PullSecretLabel = "pressfleet.io/image-pull-secret"

// managedByLabel is on everything the driver creates, and on nothing it
// didn't: DeleteSite only deletes namespaces that carry it.
const managedByLabel = "app.kubernetes.io/managed-by"

// PlatformNamespaceLabel names, on each site namespace and doctor probe
// namespace, the platform namespace of the install that owns it. Several
// installs may share a cluster and a site namespace prefix; the chart's
// admission policy and platform NetworkPolicies tell their namespaces apart
// by this label (security review K6).
const PlatformNamespaceLabel = "pressfleet.io/platform-namespace"

// ownsNamespace reports whether labels mark a site namespace of this
// install. Namespaces created before PlatformNamespaceLabel existed lack it
// until their next apply adds it, so a missing label counts as this
// install's, as it does in the chart's admission policy; another install's
// label never does.
func (p *Provisioner) ownsNamespace(labels map[string]string) bool {
	if labels[managedByLabel] != FieldManager {
		return false
	}
	owner, ok := labels[PlatformNamespaceLabel]
	return !ok || owner == p.platformNamespace
}

// labelNamespaces sets PlatformNamespaceLabel on the namespaces among objs.
// The templates render it too, but manifests rendered before it existed (a
// render snapshot that RestoreRender puts back) don't, and applying them
// would take the label off again, which the chart's admission policy
// refuses.
func (p *Provisioner) labelNamespaces(objs []*unstructured.Unstructured) {
	for _, obj := range objs {
		if obj.GetKind() != "Namespace" || obj.GetAPIVersion() != "v1" {
			continue
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[PlatformNamespaceLabel] = p.platformNamespace
		obj.SetLabels(labels)
	}
}

// sitePodSelector selects the site's pod, and no task pods.
var sitePodSelector = ComponentLabel + "=" + SiteComponent

// CreateSite renders the site's manifests, applies them, starts the site
// (scales it to one pod), and waits for its pod to be ready. For an
// existing site it converges it to cfg and starts it.
func (p *Provisioner) CreateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	dir, err := p.RenderSite(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create site: %w", err)
	}
	if err := p.maintenanceBeforeApply(cfg); err != nil {
		return nil, fmt.Errorf("create site %s: %w", cfg.ID, err)
	}
	if err := p.applySite(ctx, cfg.ID, dir); err != nil {
		return nil, fmt.Errorf("create site %s: %w", cfg.ID, err)
	}
	if err := p.scale(ctx, cfg.ID, 1); err != nil {
		return nil, fmt.Errorf("create site %s: %w", cfg.ID, err)
	}
	if err := p.waitForSite(ctx, cfg.ID); err != nil {
		return nil, fmt.Errorf("create site %s: %w", cfg.ID, err)
	}
	if err := p.maintenanceAfterApply(cfg); err != nil {
		return nil, fmt.Errorf("create site %s: %w", cfg.ID, err)
	}
	p.log.InfoContext(ctx, "site created", "site_id", cfg.ID)
	return p.GetSite(ctx, cfg.ID)
}

// UpdateSite re-renders an existing site and applies the result. Server-side
// apply changes only what changed, and the pod is recreated only if its
// template did (images, environment, resources, or wp-config.php, whose
// checksum the template carries). A stopped site stays stopped and picks up
// the change when it starts; a running one is waited for.
func (p *Provisioner) UpdateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	if cfg == nil {
		return nil, fmt.Errorf("update site: %w: site config is required", ErrInvalidSite)
	}
	d, err := p.deployment(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("update site: %w", err)
	}
	dir, err := p.RenderSite(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("update site: %w", err)
	}
	if err := p.maintenanceBeforeApply(cfg); err != nil {
		return nil, fmt.Errorf("update site %s: %w", cfg.ID, err)
	}
	if err := p.applySite(ctx, cfg.ID, dir); err != nil {
		return nil, fmt.Errorf("update site %s: %w", cfg.ID, err)
	}
	if replicas(d) > 0 {
		if err := p.waitForSite(ctx, cfg.ID); err != nil {
			return nil, fmt.Errorf("update site %s: %w", cfg.ID, err)
		}
	}
	if err := p.maintenanceAfterApply(cfg); err != nil {
		return nil, fmt.Errorf("update site %s: %w", cfg.ID, err)
	}
	p.log.InfoContext(ctx, "site updated", "site_id", cfg.ID)
	return p.GetSite(ctx, cfg.ID)
}

// applySite applies the site's rendered manifests in dir: the namespace and
// the RoleBinding that gives the control plane its rights in it first, then
// the copies of the image pull Secrets the pod needs, then everything else.
// Objects that the manifests no longer have are deleted last.
func (p *Provisioner) applySite(ctx context.Context, siteID, dir string) error {
	objs, err := LoadDir(dir)
	if err != nil {
		return err
	}
	p.labelNamespaces(objs)
	ns := p.SiteNamespace(siteID)
	var first, rest []*unstructured.Unstructured
	for _, obj := range objs {
		if applyRank(kinds[obj.GroupVersionKind()], obj.GroupVersionKind()) < 2 {
			first = append(first, obj)
		} else {
			rest = append(rest, obj)
		}
	}
	if err := p.Apply(ctx, first); err != nil {
		return err
	}
	// These are the first calls in a namespace whose RoleBinding may be
	// seconds old.
	if err := p.whileForbidden(ctx, func() error { return p.syncPullSecrets(ctx, ns) }); err != nil {
		return err
	}
	if err := p.Apply(ctx, rest); err != nil {
		return err
	}
	return p.Prune(ctx, ns, objs)
}

// syncPullSecrets copies the install's image pull Secrets from the
// platform namespace into ns, because a pod can only use Secrets of its own
// namespace, and deletes copies of Secrets the install no longer names.
func (p *Provisioner) syncPullSecrets(ctx context.Context, ns string) error {
	secrets := p.typed.CoreV1().Secrets(ns)
	for _, name := range p.pullSecrets {
		src, err := p.typed.CoreV1().Secrets(p.platformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading image pull secret %s/%s: %w", p.platformNamespace, name, err)
		}
		cp := corev1ac.Secret(name, ns).
			WithLabels(map[string]string{managedByLabel: FieldManager, PullSecretLabel: "true"}).
			WithType(src.Type).
			WithData(src.Data)
		if _, err := secrets.Apply(ctx, cp, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
			return fmt.Errorf("copying image pull secret %s into %s: %w", name, ns, err)
		}
	}
	copies, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: PullSecretLabel + "=true"})
	if err != nil {
		return fmt.Errorf("listing image pull secrets in %s: %w", ns, err)
	}
	for _, s := range copies.Items {
		if slices.Contains(p.pullSecrets, s.Name) {
			continue
		}
		if err := secrets.Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting image pull secret %s/%s: %w", ns, s.Name, err)
		}
	}
	return nil
}

// GetSite reports the site's phase from its Deployment and pod.
func (p *Provisioner) GetSite(ctx context.Context, siteID string) (*provisioner.SiteStatus, error) {
	d, err := p.deployment(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("get site: %w", err)
	}
	pods, err := p.typed.CoreV1().Pods(p.SiteNamespace(siteID)).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
	if err != nil {
		return nil, fmt.Errorf("get site %s: %w", siteID, err)
	}
	phase, msg := sitePhase(d, pods.Items)
	// URL stays empty, as on Compose: the site record has the domains.
	return &provisioner.SiteStatus{ID: siteID, Phase: phase, Message: msg}, nil
}

// StartSite scales a stopped site to one pod and waits for it to be ready.
func (p *Provisioner) StartSite(ctx context.Context, siteID string) error {
	if _, err := p.deployment(ctx, siteID); err != nil {
		return fmt.Errorf("start site: %w", err)
	}
	if err := p.scale(ctx, siteID, 1); err != nil {
		return fmt.Errorf("start site %s: %w", siteID, err)
	}
	if err := p.waitForSite(ctx, siteID); err != nil {
		return fmt.Errorf("start site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site started", "site_id", siteID)
	return nil
}

// StopSite scales the site to zero pods, keeping everything else, and
// waits until its pod is gone.
func (p *Provisioner) StopSite(ctx context.Context, siteID string) error {
	if _, err := p.deployment(ctx, siteID); err != nil {
		return fmt.Errorf("stop site: %w", err)
	}
	if err := p.scale(ctx, siteID, 0); err != nil {
		return fmt.Errorf("stop site %s: %w", siteID, err)
	}
	if err := p.waitForSite(ctx, siteID); err != nil {
		return fmt.Errorf("stop site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site stopped", "site_id", siteID)
	return nil
}

// DeleteSite stops serving the site's routes, deletes the site's namespace,
// which deletes everything in it, the wp-content volume included, waits
// until it is gone, deletes the
// site's clone of the data volume (never the data volume's storage), and
// removes the site's rendered directory. Deleting a site that doesn't exist succeeds. A
// namespace of the site's name that PressFleet didn't create is refused.
func (p *Provisioner) DeleteSite(ctx context.Context, siteID string) error {
	if err := p.validSiteNamespace(siteID); err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return fmt.Errorf("delete site: %w: %w", ErrInvalidSite, err)
	}
	name := p.SiteNamespace(siteID)
	namespaces := p.typed.CoreV1().Namespaces()
	ns, err := namespaces.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("delete site %s: %w", siteID, err)
	case !p.ownsNamespace(ns.Labels):
		return fmt.Errorf("delete site %s: %w: namespace %s isn't this install's", siteID, ErrInvalidSite, name)
	case ns.DeletionTimestamp == nil:
		policy := metav1.DeletePropagationBackground
		err := namespaces.Delete(ctx, name, metav1.DeleteOptions{
			PropagationPolicy: &policy,
			Preconditions:     &metav1.Preconditions{UID: &ns.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete site %s: %w", siteID, err)
		}
	}
	// Its routes go first, rather than at the end with the site's
	// directory, so that Traefik stops sending visitors to a site that is
	// going away.
	if err := p.unroute(siteID); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	wctx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	if err := p.WaitForNamespaceDeleted(wctx, name); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	// Cluster-wide, so not deleted with the namespace; the storage stays.
	if err := p.deleteSiteData(ctx, siteID); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site deleted", "site_id", siteID)
	return nil
}

// deployment returns the site's Deployment, or provisioner.ErrSiteNotFound:
// a site exists once its Deployment does.
func (p *Provisioner) deployment(ctx context.Context, siteID string) (*appsv1.Deployment, error) {
	if err := p.validSiteNamespace(siteID); err != nil {
		return nil, err
	}
	ns := p.SiteNamespace(siteID)
	d, err := p.typed.AppsV1().Deployments(ns).Get(ctx, SiteDeployment, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		// The control plane's rights in a site's namespace come from a
		// RoleBinding in it (charts/pressfleet), so without the namespace
		// the API server refuses rather than finds nothing (#228).
		if _, nerr := p.typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(nerr) {
			err = nerr
		}
	}
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("site %s: %w", siteID, provisioner.ErrSiteNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("site %s: %w", siteID, err)
	}
	return d, nil
}

// scale sets the number of the site's pods, which the manifests leave to
// it, unless the Deployment has it already.
func (p *Provisioner) scale(ctx context.Context, siteID string, n int32) error {
	deployments := p.typed.AppsV1().Deployments(p.SiteNamespace(siteID))
	d, err := deployments.Get(ctx, SiteDeployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if d.Spec.Replicas != nil && *d.Spec.Replicas == n {
		return nil
	}
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, n)
	if _, err := deployments.Patch(ctx, SiteDeployment, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		return fmt.Errorf("scaling to %d: %w", n, err)
	}
	return nil
}

// waitForSite waits, at most the driver's wait timeout, until the site's
// Deployment has rolled out and no pod other than the one it wants is
// left: none at all for a stopped site.
func (p *Provisioner) waitForSite(ctx context.Context, siteID string) error {
	ctx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	ns := p.SiteNamespace(siteID)
	if err := p.WaitForRollout(ctx, ns, SiteDeployment); err != nil {
		if st, serr := p.GetSite(context.WithoutCancel(ctx), siteID); serr == nil && st.Message != "" {
			return fmt.Errorf("%w (%s)", err, st.Message)
		}
		return err
	}
	d, err := p.typed.AppsV1().Deployments(ns).Get(ctx, SiteDeployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if replicas(d) > 0 {
		return nil
	}
	// A Deployment counts a terminating pod as gone already, but its
	// containers may still be running: stopped means they aren't.
	var left int
	err = wait.PollUntilContextCancel(ctx, p.poll, true, func(ctx context.Context) (bool, error) {
		pods, err := p.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sitePodSelector})
		if err != nil {
			return false, err
		}
		left = len(pods.Items)
		return left == 0, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the site's pods to stop (%d left): %w", left, err)
	}
	return nil
}

// replicas is the number of pods the Deployment wants; the API server
// defaults a missing number to 1.
func replicas(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// failedReasons are container states a pod doesn't recover from by
// waiting: the site is failed until something changes.
var failedReasons = []string{
	"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
	"CreateContainerConfigError", "CreateContainerError", "RunContainerError",
}

// sitePhase derives a site's phase from its Deployment and pods.
func sitePhase(d *appsv1.Deployment, pods []corev1.Pod) (provisioner.SitePhase, string) {
	if replicas(d) == 0 {
		if len(pods) > 0 {
			return provisioner.SitePending, "site is stopping"
		}
		return provisioner.SiteStopped, "site is stopped"
	}
	var failed, waiting []string
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodFailed {
			failed = append(failed, fmt.Sprintf("pod %s failed: %s", pod.Name, firstNonEmpty(pod.Status.Message, pod.Status.Reason)))
			continue
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
				waiting = append(waiting, "pod can't be scheduled: "+c.Message)
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			w := cs.State.Waiting
			switch {
			case w != nil && slices.Contains(failedReasons, w.Reason):
				failed = append(failed, strings.TrimSuffix(fmt.Sprintf("%s: %s: %s", cs.Name, w.Reason, w.Message), ": "))
			case w != nil:
				waiting = append(waiting, fmt.Sprintf("%s is %s", cs.Name, firstNonEmpty(w.Reason, "waiting")))
			case cs.State.Running != nil && !cs.Ready:
				waiting = append(waiting, cs.Name+" is starting")
			}
		}
	}
	if len(failed) > 0 {
		return provisioner.SiteFailed, strings.Join(failed, "; ")
	}
	done, msg, err := rolloutStatus(d)
	switch {
	case err != nil:
		return provisioner.SiteFailed, err.Error()
	case done:
		return provisioner.SiteRunning, ""
	case len(waiting) > 0:
		return provisioner.SitePending, strings.Join(waiting, "; ")
	}
	return provisioner.SitePending, msg
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
