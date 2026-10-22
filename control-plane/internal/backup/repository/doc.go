// Package repository manages the Restic repository of each client group
// (design doc §5.3 "Repository Layout", §6).
//
// # One repository and key per client group
//
// Every client group gets its own repository, encrypted with its own random
// password. Sites within a group deduplicate against each other, and
// offboarding a client means handing over or deleting one repository and
// its password. The password is a secret (secrets.KindResticRepoKey),
// encrypted with the platform master key, referenced from the group's
// backup_repositories row, which also records the backend and location.
// client_groups.backup_repository mirrors the location for display.
//
// # Backends
//
// The backend is chosen at install time (config.BackupConfig):
//
//   - local: <data dir>/backups/repositories/<client group ID>
//     (config.Layout.BackupRepositoryDir)
//   - s3: s3:<endpoint>/<bucket>/<prefix>/<client group ID> on an
//     S3-compatible service such as Amazon S3 or MinIO
//
// Each repository keeps the location it was created with, so changing the
// backend later only affects new repositories.
//
// # Initialization is single-flight
//
// Each restic init writes its own master key, and a repository with several
// is unreadable for good ("ciphertext verification failed"), so Ensure
// initializes under a per-location lock (initlock.go); the only concurrent
// callers are this process's jobs. The init itself runs in a task as the
// repository group (provisioner.RepositoryGID), so its files are owned by
// that user with mode 0600: Ensure shares them with the group afterwards
// (shareLocal), which also makes restic give later files group-writable
// modes. An init cut short by a restart leaves a
// key without a config, which restic refuses to init over, so Ensure removes
// such keys first when the repository has no snapshots or index. A
// repository already broken is reported with ErrRepositoryDamaged, naming
// its key files and the repair, and never changed (damage.go).
//
// # Running restic
//
// The control plane process never runs restic (#178, #191): its memory
// grows with the repository and every command pays for deriving the key, so
// every command runs in a repository task, a short-lived container with its
// own memory limit (provisioner.RepositoryTaskRunner). Manager.Restic,
// ResticTo, and StateRestic, and so Ensure, Export, and the callers in
// backup/restic, backup/repomaint, backup/statebackup, and metrics, build
// the task: the repository and its password file (and, on S3, the
// credentials file) as TaskAccess hands them to site tasks, so secrets reach
// the container as files and never in its environment or arguments. The
// local repository directory is bind-mounted at /repository; an S3
// repository gets the task network that reaches it.
//
// restic's exit codes keep the meaning they had when it ran in the process:
// 10 is errRepositoryMissing (and, for the state repository, ErrNoRepository;
// a local repository whose directory does not exist is the same without
// running a task), 11 is ErrLocked, 12 is ErrWrongPassword, and anything else
// is an error with restic's words, to which a 137 adds the hint that the
// task probably exceeded its memory limit. A repository broken by racing
// inits is ErrRepositoryDamaged, as before.
//
// A task has no standard input: Manager.StateRestic stages its stdin as a
// file in a transfer directory and redirects it in a shell wrapper. Output
// streams to the caller's writer without the task result's size limit.
// Export runs `restic check` of the copy in a task that mounts the export
// directory, which therefore has to be below Layout.TmpDir.
//
// At most Options.MaxTasks of these tasks run at once (jobs.concurrency).
// Jobs already run one task at a time each and are bounded by the job
// runner, so the semaphore matters for the callers outside jobs: the storage
// metrics collector and reads in API requests. The package-level
// documentation of the design (§5.3) says why site tasks are not counted.
//
// Snapshots of sites are taken by the backup engine in a site task
// container instead (§4.4); TaskAccess returns what such a task needs.
//
// # The state repository
//
// Beside the client group repositories, the state repository holds the
// state database's disaster recovery backups (state.go, package
// backup/statebackup). Its password is derived from the master key rather
// than stored, and its commands run in repository tasks like the
// group-level ones; its local directory belongs to the repository group too.
//
// # Lifecycle
//
// Repositories are created lazily: Ensure (and TaskAccess, which calls it
// when the repository is not there) creates a group's repository the first
// time the group needs one, i.e. on its first backup. TaskAccess checks that
// the repository exists without running restic (the config file, or the
// config object on S3), so a backup of a group that has one costs no restic
// task before the backup's own; Ensure also checks that it opens. Client group creation and deletion are synchronous API
// calls rather than jobs, and running restic (possibly against a remote
// bucket) doesn't belong there. Ensure is idempotent: it stores the
// password with the record in one transaction before running restic init,
// so a retry initializes the repository with the same password.
//
// A client group can't be deleted while its repository exists (a foreign
// key backs this up), so backups are never lost by accident. Offboarding
// runs Export, hands over the result, and then Delete, which removes the
// repository's data and then its record, its password, and the group's
// backup records (store.BackupRecords).
package repository
