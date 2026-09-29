package provisioner

import (
	"context"
	"io"
	"iter"
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

// LogOptions selects which log lines to read.
type LogOptions struct {
	// Components to read; empty means all of the site's components.
	Components []Component
	// Since, if non-zero, skips lines written before it.
	Since time.Time
	// Tail, if positive, starts at the last Tail lines per component.
	Tail int
	// Follow keeps the stream open for new lines until ctx is canceled.
	Follow bool
}

// LogEntry is one line of a site component's output.
type LogEntry struct {
	Time      time.Time
	Component Component
	Stream    LogStream
	Line      string
}

// LogReader reads site container logs (§7.4).
type LogReader interface {
	// Logs yields log entries for the site. Iteration ends when the logs are
	// exhausted (or, with Follow, when ctx is canceled); a non-nil error is
	// yielded once and ends iteration. Returns ErrSiteNotFound via the error
	// value if the site does not exist.
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
	// SecretEnv holds environment variables with secret values (repository
	// passwords, storage credentials). Drivers must never log them.
	SecretEnv map[string]redact.Secret
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
}

// Paths inside a task container.
const (
	TaskWPContentPath  = "/site/wp-content"
	TaskTransferPath   = "/transfer"
	TaskRepositoryPath = "/repository"
)

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
