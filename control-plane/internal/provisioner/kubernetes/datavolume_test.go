package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestEnsureDataClone(t *testing.T) {
	ns := "pf-" + testSiteID
	t.Run("replaces a released clone", func(t *testing.T) {
		stale := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: DataCloneName(ns), Labels: map[string]string{DataCloneLabel: "pressfleet"}},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				PersistentVolumeSource:        corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "old", Path: "/old"}},
				ClaimRef:                      &corev1.ObjectReference{Namespace: ns, Name: SiteDataClaim},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
		}
		tc := newTaskCluster(t, stale)
		claim, err := tc.ensureDataClone(t.Context(), testSiteID)
		if err != nil || claim != SiteDataClaim {
			t.Fatalf("ensureDataClone = %q, %v", claim, err)
		}
		pv, err := tc.typed.CoreV1().PersistentVolumes().Get(t.Context(), DataCloneName(ns), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pv.Spec.NFS != nil || pv.Spec.CSI == nil || pv.Status.Phase == corev1.VolumeReleased {
			t.Errorf("clone = %+v, want a new one of the data volume", pv.Spec)
		}
		pvc, err := tc.typed.CoreV1().PersistentVolumeClaims(ns).Get(t.Context(), SiteDataClaim, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pvc.Spec.VolumeName != pv.Name || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" ||
			pvc.Labels[RenderedLabel] != "" {
			t.Errorf("claim = %+v", pvc)
		}
		// Again: nothing to do.
		if _, err := tc.ensureDataClone(t.Context(), testSiteID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unsupported volume", func(t *testing.T) {
		objs := dataVolumeObjects()
		objs[1].(*corev1.PersistentVolume).Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/srv"},
		}
		c, _ := fakeClient(t, objs...)
		p := newWithClient(testConfig(t), c)
		if _, err := p.ensureDataClone(t.Context(), testSiteID); !errors.Is(err, ErrDataVolume) {
			t.Errorf("err = %v, want ErrDataVolume", err)
		}
	})
	t.Run("unbound claim", func(t *testing.T) {
		c, _ := fakeClient(t, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: config.DefaultDataClaim, Namespace: "pressfleet"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		})
		p := newWithClient(testConfig(t), c)
		if _, err := p.ensureDataClone(t.Context(), testSiteID); !errors.Is(err, ErrDataVolume) {
			t.Errorf("err = %v, want ErrDataVolume", err)
		}
	})
}

func TestDeleteSiteDeletesDataClone(t *testing.T) {
	tc := newTaskCluster(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "pf-" + testSiteID, Labels: map[string]string{managedByLabel: FieldManager},
	}})
	ctx := t.Context()
	if _, err := tc.ensureDataClone(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if err := tc.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	pvs := tc.typed.CoreV1().PersistentVolumes()
	if _, err := pvs.Get(ctx, DataCloneName("pf-"+testSiteID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("clone after DeleteSite: %v", err)
	}
	if _, err := pvs.Get(ctx, "pvc-data", metav1.GetOptions{}); err != nil {
		t.Errorf("the data volume after DeleteSite: %v", err)
	}
}

func TestDeleteClonePVRefusesOthers(t *testing.T) {
	c, _ := fakeClient(t, []runtime.Object{&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pf-x-data"}}}...)
	p := newWithClient(testConfig(t), c)
	if err := p.deleteClonePV(t.Context(), "pf-x-data"); err == nil {
		t.Error("deleted a volume that isn't a clone")
	}
}

func TestDataSubPath(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{filepath.Join(tmp, "job"), filepath.Join(root, "backups", "g1")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "backups"), filepath.Join(tmp, "escape")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		base, dir, want string
	}{
		{tmp, filepath.Join(tmp, "job"), "tmp/job"},
		{root, filepath.Join(root, "backups", "g1"), "backups/g1"},
		{tmp, filepath.Join(tmp, "escape"), ""},
		{tmp, tmp, ""},
		{tmp, filepath.Join(tmp, "file"), ""},
		{tmp, filepath.Join(tmp, "missing"), ""},
		{tmp, "tmp/job", ""},
		{tmp, filepath.Join(root, "backups"), ""},
	}
	for _, tt := range tests {
		got, err := dataSubPath(root, tt.base, tt.dir)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("dataSubPath(%s) = %q, %v; want %q", tt.dir, got, err, tt.want)
		}
	}
}

func TestShareTransferDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "dump.sql")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := shareTransferDir(dir, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir:     0o770 | os.ModeSetgid | os.ModeDir,
		sub:     0o770 | os.ModeSetgid | os.ModeDir,
		file:    0o660,
		outside: 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != want {
			t.Errorf("%s mode = %s, want %s", path, fi.Mode(), want)
		}
	}
}
