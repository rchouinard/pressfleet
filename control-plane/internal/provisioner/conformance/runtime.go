package conformance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// testIsolation checks that sites can't reach each other: each site's PHP
// reaches its own web server (the control that proves the check can
// connect) and neither the other site's.
func testIsolation(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	a, b := f.create(ctx, t), f.create(ctx, t)
	if f.inst.Address == nil {
		t.Skip("Instance.Address is not set")
	}
	addr := map[string]string{a.ID: f.address(ctx, t, a.ID), b.ID: f.address(ctx, t, b.ID)}
	connect := func(from, to *provisioner.SiteConfig) string {
		t.Helper()
		return f.wp(ctx, t, from, "--skip-wordpress", "eval",
			fmt.Sprintf(`$f = @fsockopen( %q, %d, $e, $s, 5 ); echo $f ? 'connected' : 'refused';`, hostOf(addr[to.ID]), portOf(t, addr[to.ID])))
	}
	for _, p := range []struct{ from, to *provisioner.SiteConfig }{{a, a}, {b, b}} {
		if got := connect(p.from, p.to); got != "connected" {
			t.Fatalf("site %s reaching its own web server at %s: %s", p.from.ID, addr[p.to.ID], got)
		}
	}
	for _, p := range []struct{ from, to *provisioner.SiteConfig }{{a, b}, {b, a}} {
		if got := connect(p.from, p.to); got != "refused" {
			t.Errorf("site %s reaching site %s at %s: %s, want refused", p.from.ID, p.to.ID, addr[p.to.ID], got)
		}
	}
	// Tasks and sessions belong to a site's network, not to every site's:
	// a task of one site can't reach the other.
	if got := f.taskConnector(ctx, t, a.ID)(addr[b.ID]); got != "refused" {
		t.Errorf("a task of site %s reaching site %s: %s, want refused", a.ID, b.ID, got)
	}
	// Their files are their own.
	f.putFile(ctx, t, a, "/var/www/wordpress/wp-content/pf-isolation.txt", "a")
	if _, err := f.inst.Exec(ctx, b.ID, provisioner.ComponentPHP, "test", "-e", "/var/www/wordpress/wp-content/pf-isolation.txt"); err == nil {
		t.Error("site B sees site A's wp-content")
	}
}

func hostOf(address string) string {
	h, _, _ := strings.Cut(address, ":")
	return h
}

func portOf(t *testing.T, address string) int {
	t.Helper()
	_, p, ok := strings.Cut(address, ":")
	n, err := strconv.Atoi(p)
	if !ok || err != nil {
		t.Fatalf("address %q is not host:port", address)
	}
	return n
}

// testSessions checks developer sessions: the command's streams are
// connected to the caller's, it runs as the site with the site's wp-content
// and little else (no network, a read-only root filesystem, no
// capabilities), and it is gone when the session ends, however it ends.
func testSessions(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	other := f.create(ctx, t)
	session := func(ctx context.Context, siteID, script, stdin string) (stdout, stderr string, err error) {
		t.Helper()
		var out, errOut strings.Builder
		err = f.d.RunSession(ctx, siteID, provisioner.Session{
			Image: f.phpImage(), Command: []string{"sh", "-c", script},
			Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		})
		return out.String(), errOut.String(), err
	}

	// The streams: standard input reaches the command, which ends when it
	// ends, and each output arrives on its own stream.
	stdout, stderr, err := session(ctx, cfg.ID, `cat; echo finished >&2`, "hello\nworld\n")
	if err != nil || stdout != "hello\nworld\n" || stderr != "finished\n" {
		t.Errorf("echo session = %q, %q, %v", stdout, stderr, err)
	}
	// Without a Stderr, standard error is dropped.
	var out strings.Builder
	if err := f.d.RunSession(ctx, cfg.ID, provisioner.Session{
		Image: f.phpImage(), Command: []string{"sh", "-c", "echo dropped >&2; echo kept"}, Stdin: strings.NewReader(""), Stdout: &out,
	}); err != nil || out.String() != "kept\n" {
		t.Errorf("session without Stderr = %q, %v", out.String(), err)
	}
	// The exit code.
	_, _, err = session(ctx, cfg.ID, `echo oops >&2; exit 7`, "")
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 7 {
		t.Errorf("failing session: %v, want an *ExecError with exit code 7", err)
	}

	// The site's user, and its wp-content, read-write.
	uid := strconv.Itoa(cfg.UID)
	stdout, stderr, err = session(ctx, cfg.ID, `
		echo "$(id -u):$(id -g)"
		echo from-session > `+provisioner.TaskWPContentPath+`/pf-session.txt
		stat -c %u `+provisioner.TaskWPContentPath+`/pf-session.txt`, "")
	if err != nil || stdout != uid+":"+uid+"\n"+uid+"\n" {
		t.Errorf("identity session = %q, %q, %v; want the site UID %s", stdout, stderr, err, uid)
	}
	if got := f.exec(ctx, t, cfg, provisioner.ComponentPHP, "cat", "/var/www/wordpress/wp-content/pf-session.txt"); got != "from-session" {
		t.Errorf("the site sees %q of what the session wrote", got)
	}

	// As little as possible: only the site's volume, a read-only root
	// filesystem with a scratch /tmp, no capabilities.
	stdout, stderr, err = session(ctx, cfg.ID, `
		ls /site
		if touch /pf-root 2>/dev/null; then echo root-writable; fi
		touch /tmp/pf-scratch && echo tmp-writable
		grep CapEff /proc/self/status
		grep -c . /proc/mounts > /dev/null`, "")
	if err != nil || stdout != "wp-content\ntmp-writable\nCapEff:\t0000000000000000\n" {
		t.Errorf("sandbox session = %q, %q, %v", stdout, stderr, err)
	}

	// No network: not the site's own web server, not another site's, not
	// the internet.
	targets := []string{"1.1.1.1:443"}
	if f.inst.Address != nil {
		targets = append(targets, f.address(ctx, t, cfg.ID), f.address(ctx, t, other.ID))
	}
	for _, target := range targets {
		stdout, _, err = session(ctx, cfg.ID, strings.ReplaceAll(connectScript, `"$1"`, target), "")
		if err != nil || stdout != "refused" {
			t.Errorf("a session reaching %s: %q, %v; want refused", target, stdout, err)
		}
	}

	// Canceling the context ends the session and kills the command.
	w := &signalWriter{first: make(chan struct{})}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- f.d.RunSession(runCtx, cfg.ID, provisioner.Session{
			Image: f.phpImage(), Command: []string{"sh", "-c", "echo started; sleep 300"}, Stdin: strings.NewReader(""), Stdout: w,
		})
	}()
	select {
	case <-w.first:
	case err := <-done:
		t.Fatalf("session ended before it started: %v", err)
	case <-time.After(2 * time.Minute):
		t.Fatal("session never started")
	}
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a canceled session succeeded")
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("canceled session didn't return")
	}
	f.wantNoLeftovers(ctx, t, cfg.ID, false)

	// A stopped site's files are reachable too.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = session(ctx, cfg.ID, `cat `+provisioner.TaskWPContentPath+`/pf-session.txt`, "")
	if err != nil || stdout != "from-session\n" {
		t.Errorf("session on a stopped site = %q, %v", stdout, err)
	}
	f.wantNoLeftovers(ctx, t, cfg.ID, false)
}

// testLogs reads a site's logs: lines the components write to their standard
// streams come back classified into sources, and the options select by
// component, time, tail, and following.
func testLogs(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	write := func(c provisioner.Component, script string) {
		t.Helper()
		f.exec(ctx, t, cfg, c, "sh", "-c", script)
	}
	logs := func(opts provisioner.LogOptions) []provisioner.LogEntry {
		t.Helper()
		var entries []provisioner.LogEntry
		for e, err := range f.d.Logs(ctx, cfg.ID, opts) {
			if err != nil {
				t.Fatalf("Logs(%+v): %v", opts, err)
			}
			entries = append(entries, e)
		}
		return entries
	}

	write(provisioner.ComponentNginx,
		`echo '{"time":"2026-10-01T12:00:00+00:00","status":502,"uri":"/logs-test"}' >/proc/1/fd/1; `+
			`echo '2026/10/01 12:00:00 [error] 7#7: logs-test upstream failed' >/proc/1/fd/2`)
	write(provisioner.ComponentPHP,
		`echo 'PHP Warning:  logs-test warning' >/proc/1/fd/2; `+
			`printf '[01-Oct-2026 12:00:00]  [pool www] pid 9\nscript_filename = /var/www/wordpress/slow.php\n[0x00007f] sleep() /x.php:1\n' >/proc/1/fd/2`)

	// Sources, and the fields every entry has.
	sources := map[provisioner.LogSource]int{}
	for _, e := range logs(provisioner.LogOptions{Contains: "logs-test"}) {
		sources[e.Source]++
		if e.Time.IsZero() || e.Line == "" || e.Level == "" || e.Source == "" {
			t.Errorf("incomplete entry %+v", e)
		}
		if e.Stream != "" && e.Stream != provisioner.Stdout && e.Stream != provisioner.Stderr {
			t.Errorf("entry with stream %q", e.Stream)
		}
	}
	if sources[provisioner.LogAccess] != 1 || sources[provisioner.LogError] != 1 || sources[provisioner.LogPHP] != 1 {
		t.Errorf("sources = %v", sources)
	}
	slow := logs(provisioner.LogOptions{Sources: []provisioner.LogSource{provisioner.LogSlow}})
	if len(slow) != 3 {
		t.Errorf("slow entries = %d, want 3", len(slow))
	}
	for _, e := range slow {
		if e.Level != provisioner.LevelWarn || e.Component != provisioner.ComponentPHP {
			t.Errorf("slow entry %+v", e)
		}
	}

	// Components.
	for _, c := range []provisioner.Component{provisioner.ComponentNginx, provisioner.ComponentPHP} {
		entries := logs(provisioner.LogOptions{Components: []provisioner.Component{c}})
		if len(entries) == 0 {
			t.Errorf("no entries of %s", c)
		}
		for _, e := range entries {
			if e.Component != c {
				t.Errorf("entry of %s among those of %s", e.Component, c)
			}
		}
	}

	// Since and Until split what was written at two times; the times are
	// the driver's own, so clocks don't matter.
	write(provisioner.ComponentPHP, `echo 'PHP Notice:  logs-since first' >/proc/1/fd/2`)
	time.Sleep(2 * time.Second)
	write(provisioner.ComponentPHP, `echo 'PHP Notice:  logs-since second' >/proc/1/fd/2`)
	both := logs(provisioner.LogOptions{Contains: "logs-since"})
	if len(both) != 2 || !both[0].Time.Before(both[1].Time) || !strings.Contains(both[0].Line, "first") {
		t.Fatalf("entries written two seconds apart: %+v", both)
	}
	if got := logs(provisioner.LogOptions{Contains: "logs-since", Since: both[1].Time}); len(got) != 1 || !strings.Contains(got[0].Line, "second") {
		t.Errorf("Since the second entry's time: %+v", got)
	}
	if got := logs(provisioner.LogOptions{Contains: "logs-since", Until: both[1].Time}); len(got) != 1 || !strings.Contains(got[0].Line, "first") {
		t.Errorf("Until the second entry's time: %+v", got)
	}
	if got := logs(provisioner.LogOptions{Contains: "LOGS-SINCE", MinLevel: provisioner.LevelError}); len(got) != 0 {
		t.Errorf("MinLevel above the entries' level kept %+v", got)
	}

	// Tail keeps the newest, oldest first.
	for i := 1; i <= 5; i++ {
		write(provisioner.ComponentPHP, fmt.Sprintf(`echo 'PHP Notice:  logs-tail %d' >/proc/1/fd/2`, i))
	}
	tail := logs(provisioner.LogOptions{Contains: "logs-tail", Tail: 2})
	if len(tail) != 2 || !strings.HasSuffix(tail[0].Line, "logs-tail 4") || !strings.HasSuffix(tail[1].Line, "logs-tail 5") {
		t.Errorf("Tail 2 = %+v", tail)
	}

	// Following sees a line written after it started, and ends with ctx.
	fctx, fcancel := context.WithTimeout(ctx, time.Minute)
	defer fcancel()
	seen := make(chan bool, 1)
	go func() {
		ok := false
		for e, err := range f.d.Logs(fctx, cfg.ID, provisioner.LogOptions{Follow: true, Contains: "logs-follow"}) {
			if err == nil && e.Source == provisioner.LogPHP {
				ok = true
				fcancel()
			}
		}
		seen <- ok
	}()
	time.Sleep(2 * time.Second)
	write(provisioner.ComponentPHP, `echo 'PHP Notice:  logs-follow' >/proc/1/fd/2`)
	if !<-seen {
		t.Error("follow did not see the new line")
	}

	// Options that can't be combined.
	for _, err := range f.d.Logs(ctx, cfg.ID, provisioner.LogOptions{Follow: true, Until: time.Now()}) {
		wantErr(t, "Follow with Until", err, provisioner.ErrInvalidLogOptions)
		break
	}
	for _, err := range f.d.Logs(ctx, id.New(), provisioner.LogOptions{}) {
		wantErr(t, "Logs of a missing site", err, provisioner.ErrSiteNotFound)
		break
	}
}

// testStats samples a site's resource use: one sample for each running
// component, memory with its limit, CPU time that is cumulative, and
// restarts that count.
func testStats(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	sample := func() map[provisioner.Component]provisioner.ComponentStats {
		t.Helper()
		stats, err := f.d.Stats(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		m := map[provisioner.Component]provisioner.ComponentStats{}
		for _, s := range stats {
			if _, dup := m[s.Component]; dup {
				t.Errorf("two samples of %s", s.Component)
			}
			m[s.Component] = s
		}
		return m
	}

	var first map[provisioner.Component]provisioner.ComponentStats
	eventually(t, statsWait, "the first Stats of both components", func() string {
		first = sample()
		if len(first) < 2 {
			return fmt.Sprintf("samples: %+v", first)
		}
		return ""
	})
	for _, c := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
		s, ok := first[c]
		if !ok {
			t.Fatalf("no sample of %s: %+v", c, first)
		}
		if s.MemoryBytes == 0 || s.MemoryLimitBytes == 0 || s.MemoryBytes > s.MemoryLimitBytes || s.Restarts != 0 || time.Since(s.SampledAt) > time.Minute {
			t.Errorf("sample of %s = %+v", c, s)
		}
		if s.CPUUsage <= 0 {
			t.Errorf("%s has used no CPU time since it started: %+v", c, s)
		}
	}
	if len(first) != 2 {
		t.Errorf("samples = %+v, want php and nginx", first)
	}

	// CPU time is cumulative: it grows with work and never goes back.
	f.wp(ctx, t, cfg, "--skip-wordpress", "eval", "$e = microtime( true ) + 3; while ( microtime( true ) < $e ) {}")
	// A driver that reads usage from a metrics pipeline sees the work
	// when it next scrapes.
	var second map[provisioner.Component]provisioner.ComponentStats
	eventually(t, statsWait, "php CPU time growing over 3 seconds of work", func() string {
		second = sample()
		if grew := second[provisioner.ComponentPHP].CPUUsage - first[provisioner.ComponentPHP].CPUUsage; grew < time.Second {
			return fmt.Sprintf("grew by %s", grew)
		}
		return ""
	})
	for c, s := range second {
		if s.CPUUsage < first[c].CPUUsage {
			t.Errorf("%s CPU time went back from %s to %s", c, first[c].CPUUsage, s.CPUUsage)
		}
	}

	// A component that dies is restarted by the orchestrator, and counted.
	if _, err := f.inst.Exec(ctx, cfg.ID, provisioner.ComponentPHP, "sh", "-c", "kill -TERM 1"); err != nil {
		t.Fatalf("stopping the main process of php: %v", err)
	}
	eventually(t, 3*time.Minute, "php restarting", func() string {
		s, ok := sample()[provisioner.ComponentPHP]
		switch {
		case !ok:
			return "no php sample yet"
		case s.Restarts < 1:
			return fmt.Sprintf("php restarts = %d", s.Restarts)
		}
		return ""
	})

	// A stopped site has no running components.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if got := sample(); len(got) != 0 {
		t.Errorf("samples of a stopped site: %+v", got)
	}
}

// testRenderSnapshots checks the optional snapshots of a site's rendered
// definition: restoring puts the site back to how it was when the snapshot
// was taken, whatever happened since, and keeps the snapshot. Drivers that
// don't implement provisioner.RenderSnapshotter skip it.
func testRenderSnapshots(t *testing.T, env Env) {
	f := newFixture(t, env)
	rs, ok := f.d.(provisioner.RenderSnapshotter)
	if !ok {
		t.Skip("the driver doesn't implement provisioner.RenderSnapshotter")
	}
	ctx := context10m(t)
	cfg := f.create(ctx, t)

	if fp, ok := f.d.(provisioner.TemplateFingerprinter); ok && fp.TemplateFingerprint() == "" {
		t.Error("TemplateFingerprint is empty")
	}
	_, err := rs.RestoreRender(ctx, cfg.ID)
	wantErr(t, "RestoreRender without a snapshot", err, provisioner.ErrNoRenderSnapshot)
	wantErr(t, "SnapshotRender of a missing site", rs.SnapshotRender(ctx, id.New()), provisioner.ErrSiteNotFound)

	before := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP)
	if err := rs.SnapshotRender(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.SnapshotRender(ctx, cfg.ID); err != nil {
		t.Errorf("replacing the snapshot: %v", err)
	}
	cfg.Resources.MemoryLimitMB = 700
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP); got <= before {
		t.Fatalf("php memory limit = %d after the update, was %d", got, before)
	}
	for range 2 { // the snapshot is kept
		st, err := rs.RestoreRender(ctx, cfg.ID)
		if err != nil || st.Phase != provisioner.SiteRunning {
			t.Fatalf("RestoreRender = %+v, %v", st, err)
		}
		if got := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP); got != before {
			t.Errorf("php memory limit = %d after the restore, want %d", got, before)
		}
	}

	// A stopped site stays stopped.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if st, err := rs.RestoreRender(ctx, cfg.ID); err != nil || st.Phase != provisioner.SiteStopped {
		t.Errorf("RestoreRender of a stopped site = %+v, %v", st, err)
	}
}
