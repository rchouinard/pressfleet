package provisioner

import (
	"context"
	"io"
	"iter"
	"path"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Driver is everything an orchestrator driver implements. Consumers should
// depend on the narrowest interface they need (Provisioner, LogReader,
// StatsReader, TaskRunner, or SessionRunner) rather than on Driver.
type Driver interface {
	Provisioner
	LogReader
	StatsReader
	TaskRunner
	SessionRunner
	Prober
	ImagePinner
}

// ImagePinner pins site images to exact builds (§5.5). Site image tags
// move: the platform rebuilds them for PHP patch releases, security fixes,
// and base OS updates without changing the tag. A site's containers are
// therefore rendered with a pinned reference of the build (SiteConfig
// PinnedImages), so a site only moves to a new build when the image rollout
// moves it, with a health check, and a failed update can move it back.
type ImagePinner interface {
	// PinImage returns a reference to the build that the image reference
	// image (a tag) names now, making the image available first if it
	// isn't: in Compose, repository@sha256:<digest>, or the local image
	// ID for an image built locally that no registry knows. It doesn't
	// look for a newer build (PullImage does).
	PinImage(ctx context.Context, image string) (string, error)
	// PullImage fetches the newest build of the tag from its registry, so
	// that PinImage returns it.
	PullImage(ctx context.Context, image string) error
}

// ProbeRequest is an HTTP GET a driver sends to a site's web server.
type ProbeRequest struct {
	// Host is the Host header: one of the site's domains.
	Host string
	// Path is the request target, starting with "/"; it may have a query.
	Path string
}

// ProbeResponse is what the site's web server answered.
type ProbeResponse struct {
	StatusCode int
}

// Prober checks that a site serves HTTP, for health checks after its
// containers were recreated (§5.5). The request goes to the site's web
// server (the nginx component) from inside the site's runtime, as the edge
// router's requests do, so it reaches WordPress whether or not the site
// is in maintenance mode or its preview domain is password protected, and
// needs no DNS or certificate. It says the request came over HTTPS, as the
// edge router does, and follows no redirects.
type Prober interface {
	// ProbeHTTP sends the request and returns the response status. A site
	// whose web server can't be reached is an error, not a status. Returns
	// ErrSiteNotFound if the site does not exist and ErrSiteNotRunning if
	// it isn't running.
	ProbeHTTP(ctx context.Context, siteID string, req ProbeRequest) (*ProbeResponse, error)
}

// Component identifies one of a site's runtime components. Names describe
// roles rather than orchestrator objects: in Compose each is a container; in
// Kubernetes each maps to a container in the site's Pod.
type Component string

// Site components (§4.1 Site Stack Topology).
const (
	ComponentNginx Component = "nginx"
	ComponentPHP   Component = "php"
)

// LogStream identifies the output stream a log line was written to.
type LogStream string

// Log streams.
const (
	Stdout LogStream = "stdout"
	Stderr LogStream = "stderr"
)

// LogOptions selects which log entries to read. Filters combine: an entry
// has to pass all of them.
type LogOptions struct {
	// Components to read; empty means all of the site's components.
	Components []Component
	// Sources to read; empty means all of them.
	Sources []LogSource
	// Since, if non-zero, skips entries written before it. Until, if
	// non-zero, skips entries written at or after it, so a page of older
	// entries ends where the previous one began.
	Since, Until time.Time
	// Contains, if set, keeps entries whose line contains it, ignoring case.
	Contains string
	// MinLevel, if set, keeps entries of at least this level; entries
	// without a known level count as info.
	MinLevel LogLevel
	// Tail, if positive, keeps the newest Tail entries of what the other
	// options select, across components. It is applied after the filters, so
	// the result holds Tail matching entries whenever there are that many.
	Tail int
	// Follow keeps the stream open for new entries until ctx is canceled.
	// The entries Tail selects come first. Follow can't be combined with
	// Until.
	Follow bool
}

// LogEntry is one line of a site component's output.
type LogEntry struct {
	// Time is when the orchestrator received the line.
	Time      time.Time
	Component Component
	// Stream is empty if the driver can't tell stdout from stderr.
	Stream LogStream
	// Source and Level are derived from the line by [LogClassifier]; a
	// driver sets them on every entry it yields.
	Source LogSource
	Level  LogLevel
	// Line is the line without its line terminator. Access log lines are
	// the JSON objects nginx writes.
	Line string
}

// LogReader reads site container logs (§7.4).
type LogReader interface {
	// Logs yields log entries for the site, oldest first, with Source and
	// Level set. Iteration ends when the logs are exhausted (or, with Follow,
	// when ctx is canceled); a non-nil error is yielded once and ends
	// iteration. Invalid options yield ErrInvalidLogOptions, and a site that
	// does not exist ErrSiteNotFound, via the error value.
	Logs(ctx context.Context, siteID string, opts LogOptions) iter.Seq2[LogEntry, error]
}

// ComponentStats is a resource usage sample for one site component.
type ComponentStats struct {
	Component Component
	// CPUUsage is cumulative CPU time consumed since the component started;
	// callers derive rates from successive samples.
	CPUUsage time.Duration
	// MemoryBytes is current memory usage (working set).
	MemoryBytes uint64
	// MemoryLimitBytes is the hard memory limit, or 0 if unlimited.
	MemoryLimitBytes uint64
	// Restarts counts restarts since the component was created.
	Restarts int
	// SampledAt is when the sample was taken.
	SampledAt time.Time
}

// StatsReader samples site resource usage (§7.1 Resources).
type StatsReader interface {
	// Stats returns one sample per running component of the site. Returns
	// ErrSiteNotFound if the site does not exist.
	Stats(ctx context.Context, siteID string) ([]ComponentStats, error)
}

// MountMode controls how a task sees a site volume.
type MountMode int

// Mount modes.
const (
	MountNone MountMode = iota
	MountReadOnly
	MountReadWrite
)

// Task is a short-lived command run with access to a site's data, e.g.
// backing up or restoring wp-content (§5.3, §5.4).
//
// Inside the task:
//   - wp-content is at TaskWPContentPath (if WPContent is not MountNone)
//   - TransferDir is at TaskTransferPath (if set)
//   - RepositoryDir is at TaskRepositoryPath (if set)
//   - SecretFiles are in TaskSecretsPath (if any)
//   - the platform services the site can reach (MariaDB, Redis) are reachable
//     the same way the site reaches them
type Task struct {
	// Image is the container image to run; typically the control plane's own
	// image, which ships restic and the MariaDB client.
	Image string
	// Command is the argv to run; it is not passed through a shell.
	Command []string
	// Env holds non-secret environment variables.
	Env map[string]string
	// SecretFiles holds files with secret contents (repository passwords,
	// storage credentials), by file name. Each is readable by the task's user
	// alone at TaskSecretPath(name), and gone when the task ends. Env points
	// the program at them (RESTIC_PASSWORD_FILE and the like). Secrets are
	// files rather than environment variables because a driver's container
	// configuration, which anything allowed to inspect the container can
	// read, holds the environment (§6); a mounted secret (a Kubernetes
	// Secret volume, say) stays out of it. Names are plain file names:
	// letters, digits, dots, hyphens, and underscores. Drivers must never log
	// the contents.
	SecretFiles map[string]redact.Secret
	// WPContent controls access to the site's wp-content volume.
	WPContent MountMode
	// TransferDir, if set, is a directory under the control plane's data
	// directory (config.Layout.TmpDir) shared read-write with the task. It is
	// how files such as database dumps move between the control plane and the
	// task. The Compose driver relies on the data directory being mounted at
	// the same path on the host and in the control plane container.
	TransferDir string
	// RepositoryDir, if set, is a directory under the control plane's data
	// directory holding a local Restic repository, bind-mounted read-write at
	// TaskRepositoryPath. Like TransferDir, it relies on the data directory
	// being at the same path on the host and in the control plane container.
	// The task keeps the site's UID and gets RepositoryGID as a supplementary
	// group, so the repository must be group-writable by RepositoryGID (see
	// there), not owned by the site's UID: other sites share it.
	RepositoryDir string
	// BackupNetwork asks for the platform's backup network too, where a
	// self-hosted S3 service holding the repository may live. Drivers
	// without one ignore it.
	BackupNetwork bool
	// Timeout bounds the task's run time; zero means no limit beyond ctx.
	Timeout time.Duration
	// Stdout, if set, receives the task's standard output as it is
	// written, without a size limit, instead of TaskResult.Stdout: data
	// such as an archive of wp-content, streamed to the control plane
	// without a copy in the transfer directory. A write error stops the
	// task.
	Stdout io.Writer
}

// Paths inside a task container.
const (
	TaskWPContentPath  = "/site/wp-content"
	TaskTransferPath   = "/transfer"
	TaskRepositoryPath = "/repository"
	TaskSecretsPath    = "/run/pressfleet/secrets" //nolint:gosec // G101: a directory, not a credential
)

// TaskSecretPath returns where a task sees the secret file name
// (Task.SecretFiles).
func TaskSecretPath(name string) string { return path.Join(TaskSecretsPath, name) }

// TaskResult is the outcome of a task that ran to completion.
type TaskResult struct {
	Stdout string
	Stderr string
}

// TaskRunner runs tasks against site data.
type TaskRunner interface {
	// RunTask runs task with access to the site's data and waits for it to
	// finish. A non-zero exit yields an *ExecError. Returns ErrSiteNotFound if
	// the site does not exist. The task's container is always removed.
	RunTask(ctx context.Context, siteID string, task Task) (*TaskResult, error)
}

// Session is one developer session on a site's files (§5.8): a command,
// normally the control plane's SFTP server, that speaks a protocol over
// its standard input and output, which the caller connects to a client.
type Session struct {
	// Image is the container image to run; empty means the driver's task
	// image, the control plane's own, which ships the SFTP server.
	Image string
	// Command is the argv to run; it is not passed through a shell.
	Command []string
	// Stdin is the command's standard input. When it ends, the command's
	// standard input is closed, which is how the command learns that the
	// client has gone.
	Stdin io.Reader
	// Stdout receives the command's standard output.
	Stdout io.Writer
	// Stderr, if set, receives its standard error; otherwise it is
	// discarded.
	Stderr io.Writer
}

// SessionRunner runs developer sessions on a site's files: the SFTP
// gateway's per-session workers (§5.8).
//
// A worker sees as little as possible: it runs as the site's UID and GID,
// has the site's wp-content volume mounted read-write at TaskWPContentPath
// and no other volume, no network (the gateway relays everything over the
// session's streams), a read-only root filesystem apart from a small
// tmpfs at /tmp, no capabilities, and small memory and process limits.
// Files it writes are therefore owned by the site, and it can't reach
// another site's files, the platform services, or the host.
//
// In Compose a worker is a one-off container attached to the caller's
// streams; on Kubernetes it maps to a short-lived Pod (stdin: true,
// stdinOnce: true, runAsUser/runAsGroup the site's UID, the site's PVC,
// a deny-all NetworkPolicy) that the driver attaches to through the API.
type SessionRunner interface {
	// RunSession starts a worker for the site that runs s.Command with its
	// standard streams connected to s's, and waits until it exits: when the
	// client ends the session (s.Stdin reaches its end), or when ctx ends,
	// which kills it. The worker is removed whatever happens. A non-zero
	// exit yields an *ExecError. Returns ErrSiteNotFound if the site does
	// not exist. The site may be running or stopped.
	RunSession(ctx context.Context, siteID string, s Session) error
}
