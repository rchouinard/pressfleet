package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var _ provisioner.RepositoryTaskRunner = (*Provisioner)(nil)

// repositoryNetwork is the network of the repository tasks that need a route
// to the internet (S3). The underscores keep it clear of site networks
// (pf-<site ID>, and site IDs have no underscores) and the platform's.
const repositoryNetwork = "pf_repository_egress"

// repositoryNamePattern names a client group's repository task containers;
// the state repository's are statePattern.
const (
	repositoryNamePattern = "pf-group-%s-task-%s"
	stateNamePattern      = "pf-state-task-%s"
)

// RunRepositoryTask runs task in a one-off container tied to a backup
// repository instead of a site (provisioner.RepositoryTaskRunner).
//
// The container has the hardening of a site task (read-only root filesystem
// with a tmpfs at /tmp, which is HOME; all capabilities dropped;
// no-new-privileges; an init process; no log) and the memory limit, but
// none of a site: it runs as provisioner.RepositoryGID, which owns local
// repositories, joins no site or platform network, and has no wp-content.
//
// With RepositoryDir, a local repository is bind-mounted at
// provisioner.TaskRepositoryPath. Without Task.BackupNetwork the container
// has no network at all (--network none). With it, the repository is on S3:
// the container joins repositoryNetwork, a plain bridge network created on
// first use that only repository tasks join, so it has a route out (NAT
// through the host) and nothing else to reach on it; it can't reach other
// repository tasks, because the network doesn't let its containers talk to
// each other; and the platform's backup network is connected beside it, if
// the platform has one, for a self-hosted S3 service. The platform's own
// networks and the sites' stay out of it, so a repository task, which holds
// the repository's password and S3 credentials, can't reach MariaDB, Redis,
// the control plane's API, or a site.
//
// Every task has a host name of its own, which restic records in its locks.
// restic treats a lock taken on its own host as stale when no process with
// the lock's PID exists, so a shared host name would let one container
// remove another's live lock; with unique ones only the age rule (30
// minutes without a refresh) applies across containers.
//
// Secrets, the transfer directory, output, Timeout, and the removal of the
// container and the secrets, also when ctx ends, are as in RunTask. The
// transfer directory goes to provisioner.RepositoryGID when the control
// plane runs as root.
func (p *Provisioner) RunRepositoryTask(ctx context.Context, repository string, task provisioner.Task) (*provisioner.TaskResult, error) {
	if repository != provisioner.StateRepository {
		if err := validRepositoryID(repository); err != nil {
			return nil, fmt.Errorf("run repository task: %w", err)
		}
	}
	what := "repository " + repository
	spec, err := p.taskSpec(task)
	if err == nil {
		err = checkRepositoryTask(task)
	}
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", what, err)
	}
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}

	const uid = provisioner.RepositoryGID
	spec.repository = repository
	spec.user = strconv.Itoa(uid) + ":" + strconv.Itoa(uid)
	spec.network = "none"
	if task.BackupNetwork {
		if err := p.ensureRepositoryNetwork(ctx); err != nil {
			return nil, fmt.Errorf("run task on %s: %w", what, err)
		}
		spec.network = repositoryNetwork
	}
	if spec.transferDir != "" && os.Geteuid() == 0 {
		if err := chownTree(spec.transferDir, uid); err != nil {
			return nil, fmt.Errorf("run task on %s: %w", what, err)
		}
	}
	token, err := taskToken()
	if err != nil {
		return nil, fmt.Errorf("run task on %s: %w", what, err)
	}
	spec.hostname = "pf-task-" + token
	if repository == provisioner.StateRepository {
		spec.name = fmt.Sprintf(stateNamePattern, token)
	} else {
		spec.name = fmt.Sprintf(repositoryNamePattern, repository, token)
	}
	return p.runTask(ctx, taskTarget{what: what, log: p.log.With("repository", repository)}, spec, task, uid)
}

// validRepositoryID checks a client group ID, which becomes part of a
// container name and a label.
func validRepositoryID(id string) error {
	if !siteID.MatchString(id) {
		return fmt.Errorf("%w: repository %q must be a client group ID (up to 60 lowercase letters, digits, and hyphens) or %q",
			ErrInvalidTask, id, provisioner.StateRepository)
	}
	return nil
}

// checkRepositoryTask checks what makes a task a repository task and refuses
// what belongs to sites.
func checkRepositoryTask(task provisioner.Task) error {
	var errs []error
	if task.WPContent != provisioner.MountNone {
		errs = append(errs, fmt.Errorf("a repository task has no wp-content (mount mode %d)", task.WPContent))
	}
	if task.RepositoryDir == "" && !task.BackupNetwork {
		errs = append(errs, errors.New("a repository task needs RepositoryDir or BackupNetwork to say where the repository is"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTask, err)
	}
	return nil
}

// ensureRepositoryNetwork creates repositoryNetwork unless it exists. It
// tries to create it every time instead of looking for it first: that is one
// call either way, concurrent tasks can't both create it, and a `docker
// network prune` that removed it while no task ran is no problem.
func (p *Provisioner) ensureRepositoryNetwork(ctx context.Context) error {
	_, err := p.docker(ctx, nil, "network", "create",
		"--driver", "bridge",
		"--label", managedLabel+"=true",
		"--label", repositoryLabel+"=network",
		// Repository tasks of different client groups share the network;
		// they have no business with each other.
		"--opt", "com.docker.network.bridge.enable_icc=false",
		repositoryNetwork)
	if err != nil && !stderrContains(err, "already exists") {
		return fmt.Errorf("creating the repository task network %s: %w", repositoryNetwork, err)
	}
	return nil
}
