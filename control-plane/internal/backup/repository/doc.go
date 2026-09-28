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
// # Running restic
//
// Group-level commands (init, and later forget, prune, and check) run in the
// control plane process by executing the restic binary. The repository
// password and S3 credentials reach restic only through its environment,
// which holds nothing else from the control plane's (in particular not the
// master key). They never appear in arguments or logs.
//
// Snapshots of sites are taken by the backup engine in a site task
// container instead (§4.4); TaskAccess returns what such a task needs.
//
// # Lifecycle
//
// Repositories are created lazily: Ensure (and TaskAccess, which calls it)
// creates a group's repository the first time the group needs one, i.e. on
// its first backup. Client group creation and deletion are synchronous API
// calls rather than jobs, and running restic (possibly against a remote
// bucket) doesn't belong there. Ensure is idempotent: it stores the
// password with the record in one transaction before running restic init,
// so a retry initializes the repository with the same password.
//
// A client group can't be deleted while its repository exists (a foreign
// key backs this up), so backups are never lost by accident. Offboarding
// runs Export, hands over the result, and then Delete, which removes the
// repository's data and then its record and password.
package repository
