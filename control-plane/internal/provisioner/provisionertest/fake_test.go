package provisionertest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
)

func config(id string) *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID:               id,
		ClientGroupID:    "group1",
		PreviewDomain:    id + ".preview.example.com",
		PHPVersion:       "8.4",
		WPCoreVersion:    "6.9.1",
		DBSecretRef:      "secret/db/" + id,
		Resources:        provisioner.DefaultResourceLimits(),
		UID:              provisioner.MinSiteUID,
		WPSaltsSecretRef: "secret/salts/" + id,
	}
}

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()

	st, err := f.CreateSite(ctx, config("a"))
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if st.Phase != provisioner.SiteRunning || st.URL != "https://a.preview.example.com" {
		t.Errorf("CreateSite status = %+v", st)
	}
	// Create is idempotent.
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatalf("second CreateSite: %v", err)
	}

	if err := f.StopSite(ctx, "a"); err != nil {
		t.Fatalf("StopSite: %v", err)
	}
	if st, _ := f.GetSite(ctx, "a"); st.Phase != provisioner.SiteStopped {
		t.Errorf("phase after stop = %q", st.Phase)
	}

	cfg := config("a")
	cfg.PrimaryDomain = "a.example.com"
	if st, err := f.UpdateSite(ctx, cfg); err != nil || st.URL != "https://a.example.com" {
		t.Errorf("UpdateSite = %+v, %v", st, err)
	}

	if err := f.DeleteSite(ctx, "a"); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	// Delete is idempotent.
	if err := f.DeleteSite(ctx, "a"); err != nil {
		t.Fatalf("second DeleteSite: %v", err)
	}
	if _, err := f.GetSite(ctx, "a"); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("GetSite after delete err = %v, want ErrSiteNotFound", err)
	}
}

func TestFakeNotFound(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	checks := map[string]error{
		"UpdateSite":     func() error { _, err := f.UpdateSite(ctx, config("x")); return err }(),
		"StartSite":      f.StartSite(ctx, "x"),
		"StopSite":       f.StopSite(ctx, "x"),
		"SetMaintenance": f.SetMaintenance(ctx, "x", true),
		"PurgeCache":     f.PurgeCache(ctx, "x"),
		"ExecWPCLI":      func() error { _, err := f.ExecWPCLI(ctx, "x", nil); return err }(),
	}
	for op, err := range checks {
		if !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("%s err = %v, want ErrSiteNotFound", op, err)
		}
	}
}

func TestFakeInvalidConfig(t *testing.T) {
	f := provisionertest.New()
	cfg := config("a")
	cfg.PHPVersion = ""
	if _, err := f.CreateSite(context.Background(), cfg); err == nil {
		t.Fatal("CreateSite with invalid config succeeded")
	}
	if _, ok := f.Site("a"); ok {
		t.Error("invalid config created a site")
	}
}

func TestFakeWPCLIAndFaults(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatal(err)
	}

	f.WPCLI = func(_ string, args []string) (string, error) {
		if args[0] == "fail" {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "boom"}
		}
		return "ok", nil
	}
	if out, err := f.ExecWPCLI(ctx, "a", []string{"core", "version"}); err != nil || out != "ok" {
		t.Errorf("ExecWPCLI = %q, %v", out, err)
	}
	var execErr *provisioner.ExecError
	if _, err := f.ExecWPCLI(ctx, "a", []string{"fail"}); !errors.As(err, &execErr) || execErr.ExitCode != 1 {
		t.Errorf("ExecWPCLI(fail) err = %v, want *ExecError", err)
	}
	if err := f.StopSite(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ExecWPCLI(ctx, "a", []string{"core", "version"}); !errors.Is(err, provisioner.ErrSiteNotRunning) {
		t.Errorf("ExecWPCLI on a stopped site err = %v, want ErrSiteNotRunning", err)
	}
	if err := f.StartSite(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected")
	f.Err = func(op, _ string) error {
		if op == "PurgeCache" {
			return injected
		}
		return nil
	}
	if err := f.PurgeCache(ctx, "a"); !errors.Is(err, injected) {
		t.Errorf("PurgeCache err = %v, want injected", err)
	}
	if s, _ := f.Site("a"); s.CachePurges != 0 {
		t.Errorf("failed purge changed state: %d purges", s.CachePurges)
	}

	calls := f.Calls()
	if last := calls[len(calls)-1]; last.Op != "PurgeCache" || last.SiteID != "a" {
		t.Errorf("last call = %+v", last)
	}
}

func TestFakeLogs(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		f.AddLogs("a",
			provisioner.LogEntry{Time: base.Add(time.Duration(i) * time.Minute), Component: provisioner.ComponentNginx, Line: fmt.Sprintf("nginx %d", i)},
			provisioner.LogEntry{Time: base.Add(time.Duration(i) * time.Minute), Component: provisioner.ComponentPHP, Line: fmt.Sprintf("php %d", i)},
		)
	}
	collect := func(opts provisioner.LogOptions) []string {
		t.Helper()
		var lines []string
		for e, err := range f.Logs(ctx, "a", opts) {
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, e.Line)
		}
		return lines
	}

	if got := collect(provisioner.LogOptions{Components: []provisioner.Component{provisioner.ComponentPHP}, Tail: 2}); !slices.Equal(got, []string{"php 2", "php 3"}) {
		t.Errorf("php tail 2 = %v", got)
	}
	if got := collect(provisioner.LogOptions{Tail: 3}); !slices.Equal(got, []string{"php 2", "nginx 3", "php 3"}) {
		t.Errorf("tail 3 = %v", got)
	}
	if got := collect(provisioner.LogOptions{Until: base.Add(time.Minute), Contains: "PHP"}); !slices.Equal(got, []string{"php 0"}) {
		t.Errorf("until + contains = %v", got)
	}
	if got := collect(provisioner.LogOptions{Since: base.Add(3 * time.Minute)}); !slices.Equal(got, []string{"nginx 3", "php 3"}) {
		t.Errorf("since = %v", got)
	}
	for _, err := range f.Logs(ctx, "missing", provisioner.LogOptions{}) {
		if !errors.Is(err, provisioner.ErrSiteNotFound) {
			t.Errorf("Logs(missing) err = %v, want ErrSiteNotFound", err)
		}
	}
}

func TestFakeLogsFollowAndClassify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := provisionertest.New()
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.AddLogs("a", provisioner.LogEntry{Time: base, Component: provisioner.ComponentNginx, Line: `{"time":"x","status":502}`})
	got := make(chan provisioner.LogEntry)
	go func() {
		defer close(got)
		for e, err := range f.Logs(ctx, "a", provisioner.LogOptions{Follow: true, MinLevel: provisioner.LevelWarn}) {
			if err != nil {
				return
			}
			got <- e
		}
	}()
	if e := <-got; e.Source != provisioner.LogAccess || e.Level != provisioner.LevelError {
		t.Errorf("backlog entry = %+v", e)
	}
	f.AddLogs("a",
		provisioner.LogEntry{Time: base.Add(time.Second), Component: provisioner.ComponentPHP, Line: "PHP Notice:  ignored"},
		provisioner.LogEntry{Time: base.Add(2 * time.Second), Component: provisioner.ComponentPHP, Line: "PHP Warning:  followed"},
	)
	if e := <-got; e.Line != "PHP Warning:  followed" || e.Source != provisioner.LogPHP {
		t.Errorf("followed entry = %+v", e)
	}
	cancel()
	for range got {
	}

	for _, err := range f.Logs(context.Background(), "a", provisioner.LogOptions{Sources: []provisioner.LogSource{"bogus"}}) {
		if !errors.Is(err, provisioner.ErrInvalidLogOptions) {
			t.Errorf("invalid options err = %v", err)
		}
	}
}

func TestFakeStatsAndTasks(t *testing.T) {
	ctx := context.Background()
	f := provisionertest.New()
	if _, err := f.CreateSite(ctx, config("a")); err != nil {
		t.Fatal(err)
	}
	f.SetStats("a", provisioner.ComponentStats{Component: provisioner.ComponentPHP, MemoryBytes: 64 << 20})
	if st, err := f.Stats(ctx, "a"); err != nil || len(st) != 1 || st[0].MemoryBytes != 64<<20 {
		t.Errorf("Stats = %+v, %v", st, err)
	}

	f.RunTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
		return &provisioner.TaskResult{Stdout: task.Command[0]}, nil
	}
	task := provisioner.Task{
		Image: "tools", Command: []string{"restic"}, WPContent: provisioner.MountReadOnly,
		Env:           map[string]string{"RESTIC_REPOSITORY": provisioner.TaskRepositoryPath},
		RepositoryDir: "/data/backups/repositories/g1",
	}
	if res, err := f.RunTask(ctx, "a", task); err != nil || res.Stdout != "restic" {
		t.Errorf("RunTask = %+v, %v", res, err)
	}
	task.Env["RESTIC_REPOSITORY"] = "changed"
	calls := f.Calls()
	if last := calls[len(calls)-1]; last.Task == nil || last.Task.WPContent != provisioner.MountReadOnly ||
		last.Task.RepositoryDir != "/data/backups/repositories/g1" ||
		last.Task.Env["RESTIC_REPOSITORY"] != provisioner.TaskRepositoryPath {
		t.Errorf("RunTask call not recorded with task: %+v", last)
	}
	if _, err := f.RunTask(ctx, "missing", task); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("RunTask(missing) err = %v", err)
	}
}
