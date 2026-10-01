package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// logDocker fakes `docker compose ps` and `docker container logs` for
// testSiteID. out and errs hold what each container wrote, as docker prints
// it with --timestamps.
type logDocker struct {
	t       *testing.T
	noSite  bool
	mu      sync.Mutex
	calls   []string
	out     map[string]string // service -> stdout
	errs    map[string]string // service -> stderr
	follow  func(ctx context.Context, service string, stdout, stderr io.Writer) error
	failLog error
}

func (f *logDocker) exec(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	if args[0] == "compose" && slices.Contains(args, "ps") {
		if f.noSite {
			return nil, nil
		}
		var out []byte
		for _, svc := range []string{"php", "nginx"} {
			b, _ := json.Marshal(compose.Container{Name: "pf-" + testSiteID + "-" + svc + "-1", Service: svc, State: "running"})
			out = append(append(out, b...), '\n')
		}
		return out, nil
	}
	f.t.Fatalf("unexpected docker call: %q", args)
	return nil, nil
}

func (f *logDocker) stream(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	f.mu.Unlock()
	if args[0] != "container" || args[1] != "logs" {
		f.t.Fatalf("unexpected streamed docker call: %q", args)
	}
	if f.failLog != nil {
		return f.failLog
	}
	service := strings.TrimSuffix(strings.TrimPrefix(args[len(args)-1], "pf-"+testSiteID+"-"), "-1")
	if slices.Contains(args, "--follow") {
		return f.follow(ctx, service, stdout, stderr)
	}
	_, _ = io.WriteString(stdout, f.out[service])
	_, _ = io.WriteString(stderr, f.errs[service])
	// docker's own noise on stderr has no timestamp.
	_, _ = io.WriteString(stderr, "Error response from daemon: not a log line\n")
	return nil
}

func newLogProvisioner(t *testing.T, f *logDocker) *compose.Provisioner {
	t.Helper()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	f.t = t
	p, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB,
		Docker: f.exec, DockerStream: f.stream,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sampleLogDocker() *logDocker {
	return &logDocker{
		out: map[string]string{
			"nginx": `2026-10-01T12:00:00.000000001Z {"time":"2026-10-01T12:00:00+00:00","status":200,"uri":"/"}` + "\n" +
				`2026-10-01T12:00:02.000000000Z {"time":"2026-10-01T12:00:02+00:00","status":503,"uri":"/down"}` + "\n",
		},
		errs: map[string]string{
			"nginx": "2026-10-01T12:00:01.000000000Z 2026/10/01 12:00:01 [error] 7#7: *1 upstream timed out\n",
			"php": "2026-10-01T12:00:00.500000000Z PHP Warning:  Undefined variable $x in /a.php on line 3\n" +
				"2026-10-01T12:00:03.000000000Z [01-Oct-2026 12:00:03]  [pool www] pid 9\n" +
				"2026-10-01T12:00:03.000000001Z script_filename = /var/www/wordpress/index.php\n" +
				"2026-10-01T12:00:03.000000002Z [0x00007f] sleep() /x.php:1\n",
		},
	}
}

func readLogs(t *testing.T, p *compose.Provisioner, opts provisioner.LogOptions) []provisioner.LogEntry {
	t.Helper()
	var out []provisioner.LogEntry
	for e, err := range p.Logs(context.Background(), testSiteID, opts) {
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestLogsMergesClassifiesAndFilters(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		opts       provisioner.LogOptions
		want       []provisioner.LogSource
		wantArgs   []string // substrings of the docker calls, in order
		wantNoArgs []string
	}{
		{"all, oldest first", provisioner.LogOptions{},
			[]provisioner.LogSource{"access", "php", "error", "access", "slow", "slow", "slow"}, nil, nil},
		{"sources", provisioner.LogOptions{Sources: []provisioner.LogSource{provisioner.LogSlow}},
			[]provisioner.LogSource{"slow", "slow", "slow"}, []string{"pf-" + testSiteID + "-php-1"}, []string{"-nginx-1"}},
		{"level", provisioner.LogOptions{MinLevel: provisioner.LevelWarn},
			[]provisioner.LogSource{"php", "error", "access", "slow", "slow", "slow"}, nil, nil},
		{"contains ignores case", provisioner.LogOptions{Contains: "TIMED OUT"}, []provisioner.LogSource{"error"}, nil, nil},
		{"window", provisioner.LogOptions{Since: base.Add(time.Second), Until: base.Add(3 * time.Second)},
			[]provisioner.LogSource{"error", "access"}, []string{"--since 2026-10-01T12:00:01Z", "--until 2026-10-01T12:00:03Z"}, nil},
		{"unfiltered tail goes to docker", provisioner.LogOptions{Tail: 2},
			[]provisioner.LogSource{"slow", "slow"}, []string{"--tail 2"}, nil},
		{"filtered tail is applied here", provisioner.LogOptions{Tail: 1, Sources: []provisioner.LogSource{provisioner.LogAccess}},
			[]provisioner.LogSource{"access"}, nil, []string{"--tail"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := sampleLogDocker()
			p := newLogProvisioner(t, f)
			got := readLogs(t, p, tt.opts)
			var sources []provisioner.LogSource
			for _, e := range got {
				sources = append(sources, e.Source)
			}
			// The fake docker ignores --tail, so Tail pushdown shows in
			// the arguments and the result is cut by SelectLogs.
			if !slices.Equal(sources, tt.want) {
				t.Errorf("sources = %v, want %v", sources, tt.want)
			}
			calls := strings.Join(f.calls, "\n")
			for _, a := range tt.wantArgs {
				if !strings.Contains(calls, a) {
					t.Errorf("docker calls %q lack %q", calls, a)
				}
			}
			for _, a := range tt.wantNoArgs {
				if strings.Contains(calls, a) {
					t.Errorf("docker calls %q contain %q", calls, a)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i].Time.Before(got[i-1].Time) {
					t.Errorf("entries out of order at %d", i)
				}
			}
		})
	}
}

func TestLogsEntryFields(t *testing.T) {
	p := newLogProvisioner(t, sampleLogDocker())
	got := readLogs(t, p, provisioner.LogOptions{Sources: []provisioner.LogSource{provisioner.LogError, provisioner.LogAccess}, MinLevel: provisioner.LevelError})
	if len(got) != 2 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	e := got[0]
	if e.Component != provisioner.ComponentNginx || e.Stream != provisioner.Stderr || e.Source != provisioner.LogError ||
		e.Level != provisioner.LevelError || !e.Time.Equal(time.Date(2026, 10, 1, 12, 0, 1, 0, time.UTC)) ||
		e.Line != "2026/10/01 12:00:01 [error] 7#7: *1 upstream timed out" {
		t.Errorf("entry = %+v", e)
	}
	if got[1].Stream != provisioner.Stdout || got[1].Source != provisioner.LogAccess {
		t.Errorf("access entry = %+v", got[1])
	}
}

func TestLogsErrors(t *testing.T) {
	ctx := context.Background()
	first := func(p *compose.Provisioner, id string, opts provisioner.LogOptions) error {
		for _, err := range p.Logs(ctx, id, opts) {
			return err
		}
		return nil
	}
	f := sampleLogDocker()
	f.noSite = true
	if err := first(newLogProvisioner(t, f), testSiteID, provisioner.LogOptions{}); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("missing site err = %v", err)
	}
	p := newLogProvisioner(t, sampleLogDocker())
	if err := first(p, testSiteID, provisioner.LogOptions{Sources: []provisioner.LogSource{"nope"}}); !errors.Is(err, provisioner.ErrInvalidLogOptions) {
		t.Errorf("invalid options err = %v", err)
	}
	if err := first(p, "Bad ID", provisioner.LogOptions{}); !errors.Is(err, compose.ErrInvalidSite) {
		t.Errorf("invalid site err = %v", err)
	}
	f = sampleLogDocker()
	f.failLog = &compose.CommandError{Args: []string{"container", "logs"}, Stderr: "boom", Err: errors.New("exit status 1")}
	if err := first(newLogProvisioner(t, f), testSiteID, provisioner.LogOptions{}); !errors.Is(err, f.failLog) {
		t.Errorf("docker failure err = %v", err)
	}
}

func TestLogsFollow(t *testing.T) {
	f := sampleLogDocker()
	var attached sync.WaitGroup
	attached.Add(2)
	var sinceArgs []string
	f.follow = func(ctx context.Context, service string, _, stderr io.Writer) error {
		attached.Done()
		// A line from before the watermark, as docker repeats on reattach,
		// then new ones, split mid-line to exercise buffering.
		if service == "php" {
			_, _ = io.WriteString(stderr, "2026-10-01T11:00:00.000000000Z old line\n")
			_, _ = io.WriteString(stderr, "2036-10-01T12:00:00.000000000Z PHP Fata")
			_, _ = io.WriteString(stderr, "l error:  boom\n")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	p := newLogProvisioner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []provisioner.LogEntry
	for e, err := range p.Logs(ctx, testSiteID, provisioner.LogOptions{Follow: true, Tail: 1, MinLevel: provisioner.LevelError}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
		if e.Line == "PHP Fatal error:  boom" {
			cancel()
		}
	}
	attached.Wait()
	if len(got) != 2 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if got[0].Source != provisioner.LogAccess || got[0].Level != provisioner.LevelError {
		t.Errorf("backlog entry = %+v", got[0])
	}
	if got[1].Source != provisioner.LogPHP || got[1].Level != provisioner.LevelError || got[1].Stream != provisioner.Stderr {
		t.Errorf("followed entry = %+v", got[1])
	}
	for _, c := range f.calls {
		if strings.Contains(c, "--follow") {
			sinceArgs = append(sinceArgs, c)
		}
	}
	if len(sinceArgs) != 2 || !strings.Contains(sinceArgs[0], "--since 20") {
		t.Errorf("follow calls = %q", sinceArgs)
	}
}

func TestLogsFollowReattaches(t *testing.T) {
	f := sampleLogDocker()
	var mu sync.Mutex
	attaches := 0
	f.follow = func(ctx context.Context, service string, _, stderr io.Writer) error {
		if service != "php" {
			<-ctx.Done()
			return nil
		}
		mu.Lock()
		attaches++
		n := attaches
		mu.Unlock()
		if n == 1 {
			return nil // the container was recreated: the stream ends
		}
		_, _ = io.WriteString(stderr, "2036-10-01T12:00:00.000000000Z after the restart\n")
		<-ctx.Done()
		return nil
	}
	p := newLogProvisioner(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for e, err := range p.Logs(ctx, testSiteID, provisioner.LogOptions{Follow: true, Components: []provisioner.Component{provisioner.ComponentPHP}, Contains: "restart"}) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Line != "after the restart" {
			t.Errorf("entry = %+v", e)
		}
		return
	}
	t.Error("no entry after reattaching")
}
