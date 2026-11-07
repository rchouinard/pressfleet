package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// The control plane's data directory is a ReadWriteMany volume, claimed in
// the platform namespace (kubernetes.data_claim). A claim belongs to one
// namespace, so site tasks, which run in their site's namespace, reach the
// same storage through a PersistentVolume of their own: a clone of the data
// volume's PersistentVolume, with the same volume source, bound to a claim
// in the site's namespace (decision of 2026-11-06 in #219). The clone is
// Retain, so deleting it, or its claim, never deletes the storage. The Helm
// chart's admission policy admits no other PersistentVolume from the
// control plane.

// ErrDataVolume is returned when the data volume can't be shared with a
// site's tasks: its claim isn't bound, or its storage isn't of a kind a
// clone can point at.
var ErrDataVolume = errors.New("kubernetes: data volume")

const (
	// DataVolumeLabel marks the data volume's own PersistentVolume, with
	// the platform namespace as the value. The chart's admission policy
	// compares every clone with the PersistentVolume that has it, and lets
	// the control plane add it to that one alone.
	DataVolumeLabel = "pressfleet.io/data-volume"
	// DataCloneLabel marks the clones, with the platform namespace as the
	// value.
	DataCloneLabel = "pressfleet.io/data-clone"
	// SiteDataClaim is the clone's claim in a site's namespace.
	SiteDataClaim = "pressfleet-data"
	// siteIDLabel names the site an object belongs to.
	siteIDLabel = "pressfleet.io/site-id"
)

// DataCloneName is the name of a site namespace's clone of the data
// volume: PersistentVolumes are cluster-wide, and the namespace is unique.
func DataCloneName(siteNamespace string) string { return siteNamespace + "-data" }

// dataVolume returns the data volume's PersistentVolume, labeled as such.
func (p *Provisioner) dataVolume(ctx context.Context) (*corev1.PersistentVolume, error) {
	claim, err := p.typed.CoreV1().PersistentVolumeClaims(p.platformNamespace).Get(ctx, p.tasks.dataClaim, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: reading claim %s/%s: %w", ErrDataVolume, p.platformNamespace, p.tasks.dataClaim, err)
	}
	if claim.Spec.VolumeName == "" || claim.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("%w: claim %s/%s isn't bound", ErrDataVolume, p.platformNamespace, p.tasks.dataClaim)
	}
	pv, err := p.typed.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: reading persistent volume %s: %w", ErrDataVolume, claim.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil && pv.Spec.NFS == nil {
		return nil, fmt.Errorf("%w: persistent volume %s is neither CSI nor NFS, so site tasks can't mount it", ErrDataVolume, pv.Name)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		p.tasks.warnReclaim.Do(func() {
			p.log.WarnContext(ctx, "the data volume's reclaim policy isn't Retain: deleting its claim would delete the storage that site tasks mount too",
				"persistent_volume", pv.Name, "reclaim_policy", string(pv.Spec.PersistentVolumeReclaimPolicy))
		})
	}
	if pv.Labels[DataVolumeLabel] == p.platformNamespace {
		return pv, nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%q}}}`, DataVolumeLabel, p.platformNamespace)
	pv, err = p.typed.CoreV1().PersistentVolumes().Patch(ctx, pv.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager})
	if err != nil {
		return nil, fmt.Errorf("%w: labeling persistent volume %s: %w", ErrDataVolume, claim.Spec.VolumeName, err)
	}
	return pv, nil
}

// dataClone is what the clone of src for the site namespace ns is.
func (p *Provisioner) dataClone(src *corev1.PersistentVolume, siteID, ns string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: DataCloneName(ns),
			Labels: map[string]string{
				managedByLabel: FieldManager,
				DataCloneLabel: p.platformNamespace,
				siteIDLabel:    siteID,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    src.Spec.Capacity.DeepCopy(),
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: src.Spec.CSI.DeepCopy(),
				NFS: src.Spec.NFS.DeepCopy(),
			},
			// Never Delete: the storage is the data volume's.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			// No class, so no provisioner takes an interest in it.
			StorageClassName: "",
			MountOptions:     slices.Clone(src.Spec.MountOptions),
			VolumeMode:       src.Spec.VolumeMode,
			NodeAffinity:     src.Spec.NodeAffinity.DeepCopy(),
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "PersistentVolumeClaim",
				Namespace: ns, Name: SiteDataClaim,
			},
		},
	}
}

// cloneCurrent reports whether an existing clone still points at the data
// volume's storage, for the site's claim, and can still bind: a clone
// whose claim was deleted (Released) can't bind a new one.
func cloneCurrent(have, want *corev1.PersistentVolume) bool {
	return have.DeletionTimestamp == nil &&
		have.Status.Phase != corev1.VolumeReleased && have.Status.Phase != corev1.VolumeFailed &&
		have.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain &&
		equality.Semantic.DeepEqual(have.Spec.CSI, want.Spec.CSI) &&
		equality.Semantic.DeepEqual(have.Spec.NFS, want.Spec.NFS) &&
		slices.Equal(have.Spec.MountOptions, want.Spec.MountOptions) &&
		have.Spec.ClaimRef != nil &&
		have.Spec.ClaimRef.Namespace == want.Spec.ClaimRef.Namespace &&
		have.Spec.ClaimRef.Name == want.Spec.ClaimRef.Name
}

// ensureDataClone makes sure that the site's namespace has a claim on the
// data volume, bound, and returns its name. It replaces a clone that no
// longer matches the data volume (the volume was replaced) or can no
// longer bind, which never touches the storage.
func (p *Provisioner) ensureDataClone(ctx context.Context, siteID string) (string, error) {
	src, err := p.dataVolume(ctx)
	if err != nil {
		return "", err
	}
	ns := p.SiteNamespace(siteID)
	want := p.dataClone(src, siteID, ns)
	pvs := p.typed.CoreV1().PersistentVolumes()
	claims := p.typed.CoreV1().PersistentVolumeClaims(ns)

	have, err := pvs.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		have = nil
	case err != nil:
		return "", fmt.Errorf("reading the data volume's clone %s: %w", want.Name, err)
	case !cloneCurrent(have, want):
		p.log.InfoContext(ctx, "replacing the data volume's clone", "site_id", siteID, "persistent_volume", want.Name)
		if err := p.deleteDataClone(ctx, ns); err != nil {
			return "", err
		}
		have = nil
	}
	if have == nil {
		if _, err := pvs.Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the data volume's clone %s: %w", want.Name, err)
		}
	}

	claim, err := claims.Get(ctx, SiteDataClaim, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		claim = nil
	case err != nil:
		return "", fmt.Errorf("reading claim %s/%s: %w", ns, SiteDataClaim, err)
	case claim.Spec.VolumeName != want.Name || claim.DeletionTimestamp != nil:
		if err := p.deleteDataClone(ctx, ns); err != nil {
			return "", err
		}
		if _, err := pvs.Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the data volume's clone %s: %w", want.Name, err)
		}
		claim = nil
	}
	if claim == nil {
		storageClass := ""
		claim = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: SiteDataClaim, Namespace: ns,
				// Not RenderedLabel: pruning a site's rendered objects
				// leaves it alone.
				Labels: map[string]string{managedByLabel: FieldManager, siteIDLabel: siteID},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: &storageClass,
				VolumeName:       want.Name,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: want.Spec.Capacity[corev1.ResourceStorage]},
				},
			},
		}
		if _, err := claims.Create(ctx, claim, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating claim %s/%s: %w", ns, SiteDataClaim, err)
		}
	}
	if err := p.waitClaimBound(ctx, ns, SiteDataClaim); err != nil {
		return "", err
	}
	return SiteDataClaim, nil
}

// waitClaimBound waits, at most the driver's wait timeout, until the claim
// is bound: binding a claim to the volume it names takes the controller a
// moment.
func (p *Provisioner) waitClaimBound(ctx context.Context, ns, name string) error {
	ctx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	phase := corev1.PersistentVolumeClaimPhase("unknown")
	err := wait.PollUntilContextCancel(ctx, p.taskPoll(), true, func(ctx context.Context) (bool, error) {
		claim, err := p.typed.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		phase = claim.Status.Phase
		return phase == corev1.ClaimBound, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for claim %s/%s to bind (%s): %w", ns, name, phase, err)
	}
	return nil
}

// deleteDataClone deletes the site namespace's claim on the data volume
// and the clone itself, and waits until the clone is gone, so that one of
// the same name can be created. The storage stays: the clone is Retain.
func (p *Provisioner) deleteDataClone(ctx context.Context, ns string) error {
	name := DataCloneName(ns)
	err := p.typed.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, SiteDataClaim, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting claim %s/%s: %w", ns, SiteDataClaim, err)
	}
	if err := p.deleteClonePV(ctx, name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, p.taskPoll(), true, func(ctx context.Context) (bool, error) {
		_, err := p.typed.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		return fmt.Errorf("waiting for the data volume's clone %s to go: %w", name, err)
	}
	return nil
}

// deleteClonePV deletes a clone of the data volume, if it is one.
func (p *Provisioner) deleteClonePV(ctx context.Context, name string) error {
	pvs := p.typed.CoreV1().PersistentVolumes()
	pv, err := pvs.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading persistent volume %s: %w", name, err)
	case pv.Labels[DataCloneLabel] != p.platformNamespace:
		return fmt.Errorf("persistent volume %s isn't a clone of this install's data volume", name)
	case pv.DeletionTimestamp != nil:
		return nil
	}
	err = pvs.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pv.UID}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting persistent volume %s: %w", name, err)
	}
	return nil
}

// deleteSiteData deletes the site's clone of the data volume, once its
// namespace (and with it the clone's claim) is gone. The storage stays.
func (p *Provisioner) deleteSiteData(ctx context.Context, siteID string) error {
	return p.deleteClonePV(ctx, DataCloneName(p.SiteNamespace(siteID)))
}

// removeStaleDataClones deletes the clones whose site namespaces no longer
// exist: a site deleted while the control plane couldn't finish.
func (p *Provisioner) removeStaleDataClones(ctx context.Context, namespaces map[string]bool) error {
	list, err := p.typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{LabelSelector: DataCloneLabel + "=" + p.platformNamespace})
	if err != nil {
		return fmt.Errorf("listing the data volume's clones: %w", err)
	}
	var errs []error
	for _, pv := range list.Items {
		if pv.Spec.ClaimRef == nil || namespaces[pv.Spec.ClaimRef.Namespace] {
			continue
		}
		errs = append(errs, p.deleteClonePV(ctx, pv.Name))
	}
	return errors.Join(errs...)
}

// dataSubPath returns where dir is on the data volume, which is mounted at
// the data directory (layout.Root) in the control plane: its path below
// root, which must be an existing directory strictly below base (itself
// below root), also after resolving symbolic links. Kubernetes mounts it
// as a subPath, and refuses one that leaves the volume.
func dataSubPath(root, base, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q must be an absolute path", dir)
	}
	if rel, err := filepath.Rel(base, dir); err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q must be below %s", dir, base)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(realBase, realDir); err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q must be below %s, also after resolving symbolic links", dir, base)
	}
	fi, err := os.Stat(realDir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory", dir)
	}
	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q must be on the data volume (%s)", dir, root)
	}
	return filepath.ToSlash(rel), nil
}

// shareTransferDir opens dir and what the control plane put in it to the
// control plane's group, which the task gets as a supplementary group.
// The control plane doesn't run as root on Kubernetes, so it can't hand
// the directory to the task's user as the Compose driver does; and the
// task pod can't use fsGroup for it, because fsGroup changes the group of
// the whole volume, which is the data directory of every site.
// Directories get the setgid bit, so files the task creates there belong
// to the group too, and the control plane can read and remove them
// afterwards. What belongs to someone else (a task of an earlier attempt)
// is left as it is.
func shareTransferDir(dir string, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("sharing the transfer directory with the task: %w", err)
	}
	defer func() { _ = root.Close() }()
	uid := os.Geteuid()
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fileOwner(fi) != uid {
			return nil
		}
		if err := root.Lchown(path, -1, gid); err != nil {
			return err
		}
		mode := fi.Mode().Perm() | 0o060
		if d.IsDir() {
			mode |= 0o010 | fs.ModeSetgid
		}
		return root.Chmod(path, mode)
	})
	if err != nil {
		return fmt.Errorf("sharing the transfer directory with the task: %w", err)
	}
	return nil
}
