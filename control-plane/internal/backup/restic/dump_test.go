package restic_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// untar reads a tar archive's regular files.
func untar(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(b)
	}
}

func TestDumpSnapshot(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		groupID := f.group(t, "Acme")
		site := f.site(t, groupID, "acme")
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual", BackupID: id.New()})
		if err != nil {
			t.Fatal(err)
		}
		want := f.wpContent(t, site.ID)
		wantDump, _ := f.dbs.Data(site.ID)
		tasksBefore := len(f.tasks())

		// The site changes after the snapshot; the dump is of the snapshot.
		f.dbs.SetData(site.ID, []byte("-- changed"))

		check := func(siteID string, snapID backup.SnapshotID) {
			t.Helper()
			var files, dump bytes.Buffer
			got, err := f.engine.DumpFiles(ctx, siteID, snapID, &files)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != snap.ID || got.WPCoreVersion != "6.9.1" || !got.Time.Equal(snap.Time) {
				t.Errorf("DumpFiles snapshot = %+v, want %s", got, snap.ID)
			}
			// Paths are relative to wp-content; core and wp-config.php
			// were never backed up.
			if archive := untar(t, &files); !maps.Equal(archive, want) {
				t.Errorf("dumped wp-content = %v, want %v", archive, want)
			}
			if _, err := f.engine.DumpDatabase(ctx, siteID, snapID, &dump); err != nil {
				t.Fatal(err)
			}
			if dump.String() != string(wantDump) {
				t.Errorf("dumped database = %q, want %q", dump.String(), wantDump)
			}
		}
		check(site.ID, snap.ID)
		check(site.ID, snap.ID[:8])
		// Dumping runs no task: it touches nothing of the site.
		if n := len(f.tasks()) - tasksBefore; n != 0 {
			t.Errorf("dumping ran %d tasks", n)
		}

		// A deleted site's snapshot is found through its backup records.
		rec := store.BackupRecord{SiteID: site.ID, ClientGroupID: groupID, Kind: store.BackupManual}
		if err := store.NewBackupRecords(db).Create(ctx, &rec); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewSites(db).Delete(ctx, site.ID); err != nil {
			t.Fatal(err)
		}
		check(site.ID, snap.ID)

		// Errors.
		other := f.site(t, groupID, "other")
		for name, tc := range map[string]struct {
			site string
			snap backup.SnapshotID
			want error
		}{
			"another site's snapshot": {other.ID, snap.ID, backup.ErrSnapshotNotFound},
			"unknown snapshot":        {site.ID, "0123456789abcdef", backup.ErrSnapshotNotFound},
			"not a snapshot ID":       {site.ID, "latest", backup.ErrSnapshotNotFound},
			"unknown site":            {id.New(), snap.ID, backup.ErrSiteNotFound},
		} {
			if _, err := f.engine.DumpFiles(ctx, tc.site, tc.snap, io.Discard); !errors.Is(err, tc.want) {
				t.Errorf("%s: DumpFiles err = %v, want %v", name, err, tc.want)
			}
			if _, err := f.engine.DumpDatabase(ctx, tc.site, tc.snap, io.Discard); !errors.Is(err, tc.want) {
				t.Errorf("%s: DumpDatabase err = %v, want %v", name, err, tc.want)
			}
		}
		// A group without a repository has no snapshots.
		empty := f.site(t, f.group(t, "Empty"), "empty")
		if _, err := f.engine.DumpFiles(ctx, empty.ID, snap.ID, io.Discard); !errors.Is(err, backup.ErrSnapshotNotFound) {
			t.Errorf("group without a repository: err = %v", err)
		}
		if _, err := f.engine.DumpFiles(ctx, "bad id", snap.ID, io.Discard); err == nil || strings.Contains(err.Error(), "restic dump") {
			t.Errorf("invalid site ID: err = %v", err)
		}
	})
}
