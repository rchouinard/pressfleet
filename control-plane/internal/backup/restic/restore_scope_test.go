package restic_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/restic"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestRestoreScopes(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		groupID := f.group(t, "Acme")
		site := f.site(t, groupID, "acme")
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual", BackupID: id.New()})
		if err != nil {
			t.Fatal(err)
		}
		snapshotted := f.wpContent(t, site.ID)
		dumpBefore, _ := f.dbs.Data(site.ID)
		wpc := filepath.Join(f.siteRoots[site.ID], "wp-content")

		// change makes every part of the site differ from the snapshot:
		// code that changed, was added (also in directories the snapshot
		// lacks), and was removed; uploads; and the database.
		change := func() {
			writeFile(t, filepath.Join(wpc, "plugins/hello/hello.php"), "<?php // changed")
			writeFile(t, filepath.Join(wpc, "plugins/new/new.php"), "<?php // new")
			writeFile(t, filepath.Join(wpc, "mu-plugins/loader.php"), "<?php // mu")
			writeFile(t, filepath.Join(wpc, "languages/de_DE.mo"), "mo")
			writeFile(t, filepath.Join(wpc, "uploads/2026/10/new.jpg"), "new")
			writeFile(t, filepath.Join(wpc, "uploads/2026/09/photo.jpg"), "edited")
			if err := os.Remove(filepath.Join(wpc, "themes/twentytwenty/style.css")); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			f.dbs.SetData(site.ID, []byte("-- changed"))
		}
		// wantCodeRestored is the snapshot's code and the current uploads.
		wantCodeRestored := map[string]string{
			"plugins/hello/hello.php":       snapshotted["plugins/hello/hello.php"],
			"themes/twentytwenty/style.css": snapshotted["themes/twentytwenty/style.css"],
			"uploads/2026/10/new.jpg":       "new",
			"uploads/2026/09/photo.jpg":     "edited",
		}

		t.Run("code and database", func(t *testing.T) {
			change()
			tasksBefore := len(f.tasks())
			if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesCode, Database: true}); err != nil {
				t.Fatal(err)
			}
			if got := f.wpContent(t, site.ID); !maps.Equal(got, wantCodeRestored) {
				t.Errorf("wp-content = %v, want %v", got, wantCodeRestored)
			}
			if got, _ := f.dbs.Data(site.ID); string(got) != string(dumpBefore) {
				t.Errorf("database = %q, want the snapshot's", got)
			}
			// The dump, then each code directory: restored where the
			// snapshot has it, removed where it doesn't.
			tasks := f.tasks()[tasksBefore:]
			if len(tasks) != 5 {
				t.Fatalf("restore tasks = %+v", tasks)
			}
			lock := []string{"restic", "restore", "--json", "--quiet", "--retry-lock", "30m0s"}
			for i, want := range [][]string{
				append(slices.Clone(lock), string(snap.ID)+":/transfer", "--target", "/transfer", "--include", "/database.sql"),
				append(slices.Clone(lock), string(snap.ID)+":/site/wp-content/plugins", "--target", "/site/wp-content/plugins", "--delete"),
				append(slices.Clone(lock), string(snap.ID)+":/site/wp-content/themes", "--target", "/site/wp-content/themes", "--delete"),
				{"rm", "-rf", "--", "/site/wp-content/mu-plugins"},
				{"rm", "-rf", "--", "/site/wp-content/languages"},
			} {
				if !slices.Equal(tasks[i].Command, want) {
					t.Errorf("task %d = %q, want %q", i, tasks[i].Command, want)
				}
				if i > 0 && tasks[i].WPContent != provisioner.MountReadWrite {
					t.Errorf("task %d mounts wp-content %v, want read-write", i, tasks[i].WPContent)
				}
			}
		})

		t.Run("code only", func(t *testing.T) {
			change()
			tasksBefore := len(f.tasks())
			if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesCode}); err != nil {
				t.Fatal(err)
			}
			if got := f.wpContent(t, site.ID); !maps.Equal(got, wantCodeRestored) {
				t.Errorf("wp-content = %v, want %v", got, wantCodeRestored)
			}
			if got, _ := f.dbs.Data(site.ID); string(got) != "-- changed" {
				t.Errorf("database = %q, want it unchanged", got)
			}
			// No dump task.
			tasks := f.tasks()[tasksBefore:]
			if len(tasks) != 4 {
				t.Errorf("restore ran %d tasks, want 4", len(tasks))
			}
			for _, task := range tasks {
				if task.TransferDir != "" {
					t.Errorf("task %q has a transfer directory", task.Command)
				}
			}
			if left := f.tmpEntries(t); len(left) != 0 {
				t.Errorf("left in tmp/: %v", left)
			}
		})

		t.Run("database only", func(t *testing.T) {
			change()
			before := f.wpContent(t, site.ID)
			tasksBefore := len(f.tasks())
			if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesNone, Database: true}); err != nil {
				t.Fatal(err)
			}
			if got := f.wpContent(t, site.ID); !maps.Equal(got, before) {
				t.Errorf("wp-content = %v, want it unchanged", got)
			}
			if got, _ := f.dbs.Data(site.ID); string(got) != string(dumpBefore) {
				t.Errorf("database = %q, want the snapshot's", got)
			}
			tasks := f.tasks()[tasksBefore:]
			if len(tasks) != 1 || tasks[0].WPContent != provisioner.MountNone {
				t.Errorf("restore tasks = %+v, want only the dump", tasks)
			}
		})

		t.Run("all files only", func(t *testing.T) {
			change()
			if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesAll}); err != nil {
				t.Fatal(err)
			}
			if got := f.wpContent(t, site.ID); !maps.Equal(got, snapshotted) {
				t.Errorf("wp-content = %v, want %v", got, snapshotted)
			}
			if got, _ := f.dbs.Data(site.ID); string(got) != "-- changed" {
				t.Errorf("database = %q, want it unchanged", got)
			}
		})

		t.Run("a failing code task", func(t *testing.T) {
			change()
			f.output = func(task provisioner.Task) (*provisioner.TaskResult, error) {
				if slices.Contains(task.Command, "--delete") && strings.HasSuffix(task.Command[len(task.Command)-2], "/themes") {
					return nil, &provisioner.ExecError{ExitCode: 1, Stderr: `{"message_type":"exit_error","code":1,"message":"Fatal: disk full"}` + "\n"}
				}
				return f.runTaskDirect(t, site.ID, task)
			}
			defer func() { f.output = nil }()
			err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, backup.RestoreOptions{Files: backup.RestoreFilesCode, Database: true})
			if !errors.Is(err, restic.ErrRestoreFailed) || !strings.Contains(err.Error(), "wp-content/themes") || !strings.Contains(err.Error(), "Fatal: disk full") {
				t.Errorf("Restore = %v, want an error naming themes and restic's reason", err)
			}
			if got, _ := f.dbs.Data(site.ID); string(got) != "-- changed" {
				t.Errorf("a failed files restore imported the database: %q", got)
			}
		})
	})
}

func TestRestoreInvalidScope(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		site := f.site(t, f.group(t, "Acme"), "acme")
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		tasksBefore := len(f.tasks())
		for _, opts := range []backup.RestoreOptions{
			{},
			{Files: backup.RestoreFilesNone},
			{Files: "most", Database: true},
		} {
			if err := f.engine.Restore(ctx, site.ID, snap.ID, site.ID, opts); !errors.Is(err, backup.ErrInvalidRestoreOptions) {
				t.Errorf("Restore(%+v) = %v, want ErrInvalidRestoreOptions", opts, err)
			}
		}
		if n := len(f.tasks()) - tasksBefore; n != 0 {
			t.Errorf("rejected restores ran %d tasks", n)
		}
	})
}
