package cli

import "text/template"

// recoveryData fills the recovery steps of a failed upgrade.
type recoveryData struct {
	// From and To are the versions upgraded from and to.
	From, To string
	// Project is the platform's Compose project name.
	Project string
	// PlatformDir is the platform project, and PreviousDir the copy of it
	// from before the upgrade.
	PlatformDir, PreviousDir string
	// PreviousImage is the control plane image from before the upgrade,
	// by digest when it was pulled from a registry.
	PreviousImage string
	// Backup is the state database backup the control plane took before
	// migrating; empty if no migration ran (or BackupUnchecked).
	Backup string
	// BackupUnchecked says the host couldn't tell whether the control plane
	// took a backup (Backup is empty then, but a migration may have run);
	// the steps leave the check to the operator. BackupsDir is where the
	// backups are, and Since the time the upgrade started, in the
	// timestamp format of the backups' names.
	BackupUnchecked bool
	BackupsDir      string
	Since           string
	// Postgres says the state database is PostgreSQL, which the operator
	// restores from their own backup.
	Postgres bool
	// Forced lists the services to recreate on the previous project's
	// files, space-separated; empty if none.
	Forced string
	// ConfigFlag is the --config flag the CLI was run with, with a
	// leading space, for the step that runs the CLI again; empty if none.
	ConfigFlag string
}

// recoveryText is printed by a failed `pressfleet upgrade`, and
// docs/upgrading.md shows it rendered with placeholders (a test keeps them
// in sync). Every command must work when pasted as is: the end-to-end tests
// of the recovery (#142, #251) run them verbatim. The last one runs this
// CLI's `upgrade --rollback`, which finds the platform back on the previous
// release and marks the upgrade rolled back, lifting the rollout hold
// (#310); the previous release's CLI may not know how.
const recoveryText = `Sites keep serving while the control plane is down. To return to {{.From}}, run these commands on this host as root:

1. Stop the control plane:

    docker compose --project-name {{.Project}} --project-directory {{.PlatformDir}} stop control-plane

{{if .Postgres -}}
2. Restore the PostgreSQL state database from the backup you took before the upgrade, with your own PostgreSQL tooling. PressFleet doesn't back up PostgreSQL.
{{- else if .Backup -}}
2. Restore the state database from the backup the control plane took before migrating it, with the previous control plane image ({{.PreviousImage}}):

    docker compose --project-name {{.Project}} --project-directory {{.PreviousDir}} run --rm --no-deps control-plane state restore {{.Backup}}
{{- else if .BackupUnchecked -}}
2. This command couldn't check whether the control plane migrated the state database, so check it yourself by listing the state backups directory:

    docker compose --project-name {{.Project}} --project-directory {{.PreviousDir}} run --rm --no-deps --no-TTY --entrypoint ls control-plane -1 {{.BackupsDir}}

If no file is named pressfleet-<timestamp>-schemaNNNN.db with a timestamp of {{.Since}} or later (when the upgrade started), the control plane didn't migrate the database, and nothing needs restoring: go on to step 3. Otherwise restore the first such file, the backup from before the migration, with the previous control plane image ({{.PreviousImage}}):

    docker compose --project-name {{.Project}} --project-directory {{.PreviousDir}} run --rm --no-deps control-plane state restore {{.BackupsDir}}/<file name from the listing>

Don't skip the restore when there is such a file: {{.From}} would crash-loop with "database schema is newer than this pressfleet binary supports".
{{- else -}}
2. The state database needs no restore: the control plane didn't migrate it (it took no pre-migration backup during the upgrade).
{{- end}}

3. Put the previous platform project back:

    cp -a {{.PreviousDir}}/. {{.PlatformDir}}/

4. Start the previous release's services:

    docker compose --project-name {{.Project}} --project-directory {{.PlatformDir}} up --detach --wait --remove-orphans
{{- if .Forced}}
    docker compose --project-name {{.Project}} --project-directory {{.PlatformDir}} up --detach --wait --force-recreate {{.Forced}}
{{- end}}

5. Mark the upgrade rolled back with this CLI ({{.To}}), so that the control plane runs its WordPress core, image, and template rollouts again. It finds the platform back on {{.From}} and changes nothing else:

    pressfleet upgrade --rollback{{.ConfigFlag}}

Then use the pressfleet CLI of {{.From}} again. Anything recorded in the control plane after the upgrade started is lost with the restore.
`

var recoveryTemplate = template.Must(template.New("recovery").Parse(recoveryText))
