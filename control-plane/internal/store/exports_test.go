package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestExports(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		job := store.Job{Type: "site.export", TargetType: "site", TargetID: id.New()}
		if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
			t.Fatal(err)
		}
		exports := store.NewExports(db)
		siteID, groupID := job.TargetID, id.New()
		taken := time.Now().Add(-time.Hour)

		// Create fills in the running state, keeping a given ID and what
		// the export is of.
		before := time.Now().Add(-time.Second)
		e := &store.Export{
			ID: id.New(), Kind: store.ExportSite, SiteID: siteID, SiteSlug: "acme", ClientGroupID: groupID, JobID: job.ID,
			Source: store.ExportSource{SiteURL: "https://acme.example", TablePrefix: "wp_", WPCoreVersion: "6.9.1", PHPVersion: "8.4", TakenAt: taken},
			// Ignored: a new export is running and has no files.
			Status: store.ExportReady, SizeBytes: 9, Files: []store.ExportFile{{Name: "x", SizeBytes: 9}},
		}
		wantID := e.ID
		if err := exports.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
		if e.ID != wantID || e.Status != store.ExportRunning || e.SizeBytes != 0 || e.Files != nil || e.CreatedAt.Before(before) ||
			!e.ReadyAt.IsZero() || !e.ExpiresAt.IsZero() || !e.ExpiredAt.IsZero() {
			t.Errorf("created = %+v", e)
		}
		got, err := exports.Get(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := *e
		want.Files = []store.ExportFile{}
		want.Source.TakenAt = taken.UTC().Truncate(time.Microsecond)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Get = %+v, want %+v", got, want)
		}
		if err := exports.Create(ctx, &store.Export{ID: e.ID, Kind: store.ExportSite, SiteID: siteID}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create with a taken ID: %v, want ErrConflict", err)
		}
		if err := exports.Create(ctx, &store.Export{Kind: store.ExportSite}); err == nil {
			t.Error("Create accepted an export without a site")
		}
		if err := exports.Create(ctx, &store.Export{Kind: "everything", SiteID: siteID}); err == nil {
			t.Error("Create accepted an unknown kind")
		}
		if _, err := exports.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get of a missing export: %v, want ErrNotFound", err)
		}

		// Nothing to expire yet, and a running export can't expire.
		now := time.Now()
		if list, err := exports.ListExpired(ctx, now.Add(time.Hour), 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired with nothing ready = %+v, %v", list, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || changed {
			t.Errorf("Expire of a running export = %v, %v", changed, err)
		}

		// Ready once, with its files and what they hold; a second call
		// changes nothing.
		expires := now.Add(time.Hour)
		files := []store.ExportFile{{Name: "wp-content.tar.gz", SizeBytes: 1000}, {Name: "database.sql.gz", SizeBytes: 234}}
		src := store.ExportSource{SiteURL: "https://www.acme.example", TablePrefix: "acme_", WPCoreVersion: "6.9.2", TakenAt: taken}
		if _, err := exports.Ready(ctx, e.ID, nil, src, expires); err == nil {
			t.Error("Ready accepted no files")
		}
		ready := func(files []store.ExportFile) (changed bool, err error) {
			err = db.InTx(ctx, func(q store.Querier) error {
				changed, err = store.NewExports(q).Ready(ctx, e.ID, files, src, expires)
				return err
			})
			return changed, err
		}
		if changed, err := ready(files); err != nil || !changed {
			t.Fatalf("Ready = %v, %v", changed, err)
		}
		if changed, err := ready(files[:1]); err != nil || changed {
			t.Errorf("second Ready = %v, %v", changed, err)
		}
		if changed, err := exports.Fail(ctx, e.ID, "late"); err != nil || changed {
			t.Errorf("Fail of a ready export = %v, %v", changed, err)
		}
		got, err = exports.Get(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantFiles := []store.ExportFile{files[1], files[0]} // by name
		if got.Status != store.ExportReady || got.SizeBytes != 1234 || got.ReadyAt.IsZero() ||
			!got.ExpiresAt.Equal(expires.UTC().Truncate(time.Microsecond)) || got.Message != "" ||
			!reflect.DeepEqual(got.Files, wantFiles) || got.Source.TablePrefix != "acme_" || got.Source.PHPVersion != "" {
			t.Errorf("ready = %+v", got)
		}
		if f, ok := got.File("database.sql.gz"); !ok || f.SizeBytes != 234 {
			t.Errorf("File(database.sql.gz) = %+v, %v", f, ok)
		}
		if _, ok := got.File("wp-config.php"); ok {
			t.Error("File found a file the export doesn't have")
		}

		// Another export, of the database, that fails. IDs are ULIDs,
		// ordered by the millisecond.
		time.Sleep(2 * time.Millisecond)
		failed := &store.Export{Kind: store.ExportDatabase, SiteID: siteID, ClientGroupID: groupID}
		if err := exports.Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(failed.ID) || failed.ID == e.ID {
			t.Errorf("generated ID %q", failed.ID)
		}
		if changed, err := exports.Fail(ctx, failed.ID, "dump failed"); err != nil || !changed {
			t.Fatalf("Fail = %v, %v", changed, err)
		}
		if got, err := exports.Get(ctx, failed.ID); err != nil || got.Status != store.ExportFailed || got.Message != "dump failed" {
			t.Errorf("failed = %+v, %v", got, err)
		}

		// List: newest first, by site or group, kind, and status.
		time.Sleep(2 * time.Millisecond)
		snap := &store.Export{Kind: store.ExportSnapshot, SiteID: id.New(), ClientGroupID: groupID, BackupID: id.New(), SnapshotID: "f3b098f1"}
		if err := exports.Create(ctx, snap); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			f    store.ExportFilter
			want []string
		}{
			{"site", store.ExportFilter{SiteID: siteID}, []string{failed.ID, e.ID}},
			{"group", store.ExportFilter{ClientGroupID: groupID}, []string{snap.ID, failed.ID, e.ID}},
			{"kinds", store.ExportFilter{ClientGroupID: groupID, Kinds: []store.ExportKind{store.ExportSite, store.ExportSnapshot}}, []string{snap.ID, e.ID}},
			{"statuses", store.ExportFilter{SiteID: siteID, Statuses: []store.ExportStatus{store.ExportReady, store.ExportRunning}}, []string{e.ID}},
			{"page", store.ExportFilter{ClientGroupID: groupID, Before: snap.ID, Limit: 1}, []string{failed.ID}},
			{"other group", store.ExportFilter{ClientGroupID: id.New()}, []string{}},
		} {
			page, err := exports.List(ctx, tc.f)
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
				continue
			}
			ids := []string{}
			for _, x := range page.Exports {
				ids = append(ids, x.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Errorf("%s: List = %v, want %v", tc.name, ids, tc.want)
			}
		}
		page, err := exports.List(ctx, store.ExportFilter{SiteID: siteID, Limit: 1})
		if err != nil || len(page.Exports) != 1 || page.NextCursor != failed.ID {
			t.Errorf("first page = %+v, %v", page, err)
		}
		page, err = exports.List(ctx, store.ExportFilter{SiteID: siteID, Before: page.NextCursor})
		if err != nil || len(page.Exports) != 1 || !reflect.DeepEqual(page.Exports[0].Files, wantFiles) || page.NextCursor != "" {
			t.Errorf("second page = %+v, %v", page, err)
		}
		if got, err := exports.Get(ctx, snap.ID); err != nil || got.BackupID != snap.BackupID || got.SnapshotID != "f3b098f1" {
			t.Errorf("snapshot export = %+v, %v", got, err)
		}
		if _, err := exports.List(ctx, store.ExportFilter{}); err == nil {
			t.Error("List without a site or group succeeded")
		}

		// Only ready exports past their expiry are listed.
		if list, err := exports.ListExpired(ctx, now, 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired before the expiry = %+v, %v", list, err)
		}
		list, err := exports.ListExpired(ctx, expires, 10)
		if err != nil || len(list) != 1 || list[0].ID != e.ID {
			t.Fatalf("ListExpired at the expiry = %+v, %v", list, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || !changed {
			t.Fatalf("Expire = %v, %v", changed, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || changed {
			t.Errorf("second Expire = %v, %v", changed, err)
		}
		if got, err := exports.Get(ctx, e.ID); err != nil || got.Status != store.ExportExpired || got.ExpiredAt.IsZero() || len(got.Files) != 2 {
			t.Errorf("expired = %+v, %v", got, err)
		}
		if list, err := exports.ListExpired(ctx, expires.Add(time.Hour), 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired after expiring = %+v, %v", list, err)
		}

		// Deleting the job keeps the record.
		if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := exports.Get(ctx, e.ID); err != nil || got.JobID != "" {
			t.Errorf("after deleting the job = %+v, %v", got, err)
		}
	})
}

func TestExportKindValid(t *testing.T) {
	for _, k := range []store.ExportKind{store.ExportDatabase, store.ExportSite, store.ExportSnapshot} {
		if !k.Valid() {
			t.Errorf("%q is not valid", k)
		}
	}
	for _, k := range []store.ExportKind{"", "Site", "files"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
}

func TestMigrateDatabaseExports(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			if _, err := db.MigrateTo(ctx, t.TempDir(), 26); err != nil {
				t.Fatal(err)
			}
			group := createGroup(ctx, t, db)
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			live, gone := id.New(), id.New()
			if _, err := db.ExecContext(ctx, "INSERT INTO sites (id, client_group_id, name, slug, status, preview_domain, "+
				"php_version, wp_core_version, uid, created_at, updated_at) VALUES (?, ?, 'Live Site', 'live', 'running', 'live.example.test', '8.4', '7.1.2', 20000, ?, ?)",
				live, group, at, at); err != nil {
				t.Fatal(err)
			}
			ready, failed := id.New(), id.New()
			for _, e := range []struct {
				id, site, status string
				size             any
			}{{ready, live, "ready", 1234}, {failed, gone, "failed", nil}} {
				if _, err := db.ExecContext(ctx, "INSERT INTO database_exports (id, site_id, status, size_bytes, message, created_at, ready_at, expires_at) "+
					"VALUES (?, ?, ?, ?, 'm', ?, ?, ?)", e.id, e.site, e.status, e.size, at, at, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			liveBackup, goneBackup := id.New(), id.New()
			for _, b := range []struct{ id, site string }{{liveBackup, live}, {goneBackup, gone}} {
				if _, err := db.ExecContext(ctx, "INSERT INTO backup_records (id, site_id, client_group_id, kind, status, wp_core_version, created_at, started_at) "+
					"VALUES (?, ?, ?, 'manual', 'succeeded', '7.1.2', ?, ?)", b.id, b.site, group, at, at); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Migrate(ctx, t.TempDir()); err != nil {
				t.Fatal(err)
			}

			exports := store.NewExports(db)
			got, err := exports.Get(ctx, ready)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != store.ExportDatabase || got.SiteID != live || got.SiteSlug != "live" || got.ClientGroupID != group ||
				got.Status != store.ExportReady || got.SizeBytes != 1234 || got.Message != "m" || !got.ExpiresAt.Equal(at.Add(time.Hour)) ||
				len(got.Files) != 0 {
				t.Errorf("carried-over ready export = %+v", got)
			}
			got, err = exports.Get(ctx, failed)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != store.ExportDatabase || got.SiteID != gone || got.SiteSlug != "" || got.ClientGroupID != "" || got.Status != store.ExportFailed {
				t.Errorf("carried-over failed export = %+v", got)
			}
			if _, err := db.ExecContext(ctx, "SELECT 1 FROM database_exports"); err == nil {
				t.Error("database_exports still exists")
			}

			records := store.NewBackupRecords(db)
			if b, err := records.Get(ctx, liveBackup); err != nil || b.SiteName != "Live Site" || b.SiteSlug != "live" {
				t.Errorf("live site's backup = %+v, %v", b, err)
			}
			if b, err := records.Get(ctx, goneBackup); err != nil || b.SiteName != "" || b.SiteSlug != "" {
				t.Errorf("deleted site's backup = %+v, %v", b, err)
			}
		})
	}
}
