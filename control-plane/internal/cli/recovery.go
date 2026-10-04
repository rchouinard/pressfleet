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
	// migrating; empty if no migration ran.
	Backup string
	// Postgres says the state database is PostgreSQL, which the operator
	// restores from their own backup.
	Postgres bool
	// Forced lists the services to recreate on the previous project's
	// files, space-separated; empty if none.
	Forced string
}

// recoveryText is printed by a failed `pressfleet upgrade`, and
// docs/upgrading.md shows it rendered with placeholders (a test keeps them
// in sync). Every command must work when pasted as is: the end-to-end test
// of the recovery (#142) runs them verbatim.
const recoveryText = `Sites keep serving while the control plane is down. To return to {{.From}}, run these commands on this host as root:

1. Stop the control plane:

    docker compose --project-name {{.Project}} --project-directory {{.PlatformDir}} stop control-plane

{{if .Postgres -}}
2. Restore the PostgreSQL state database from the backup you took before the upgrade, with your own PostgreSQL tooling. PressFleet doesn't back up PostgreSQL.
{{- else if .Backup -}}
2. Restore the state database from the backup the control plane took before migrating it, with the previous control plane image ({{.PreviousImage}}):

    docker compose --project-name {{.Project}} --project-directory {{.PreviousDir}} run --rm --no-deps control-plane state restore {{.Backup}}
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

Then use the pressfleet CLI of {{.From}} again. Anything recorded in the control plane after the upgrade started is lost with the restore.
`

var recoveryTemplate = template.Must(template.New("recovery").Parse(recoveryText))
