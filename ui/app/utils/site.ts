import type { components } from '../api/schema.gen'
import type { BadgeColor } from './job'

export type Site = components['schemas']['Site']
export type SiteStatus = components['schemas']['SiteStatus']
export type SiteCreate = components['schemas']['SiteCreate']
export type SiteAdminCredentials = components['schemas']['SiteAdminCredentials']
export type SiteEnvironment = components['schemas']['SiteEnvironment']

/** Whether the site is a staging copy of a production site (design doc §5.11). */
export function isStaging(site: Pick<Site, 'environment'>): boolean {
  return site.environment === 'staging'
}

/** The production site a staging site was cloned from, or undefined for a production site. */
export function productionIdOf(site: Pick<Site, 'parent_site_id' | 'staging'>): string | undefined {
  return site.parent_site_id ?? site.staging?.production_site_id ?? undefined
}

/** A site's name for a picker, with its slug; staging sites say so, since they otherwise look like their production site. */
export function siteOptionLabel(site: Pick<Site, 'name' | 'slug' | 'environment'>): string {
  return `${site.name} (${site.slug})${isStaging(site) ? ' - staging' : ''}`
}

export interface SiteStatusView {
  label: string
  color: BadgeColor
  /** Always shown with the label: status is never colour alone. */
  icon: string
}

/** How a recorded site status looks. */
export function siteStatusView(status: SiteStatus): SiteStatusView {
  switch (status) {
    case 'provisioning':
      return { label: 'Provisioning', color: 'info', icon: 'i-lucide-loader-circle' }
    case 'running':
      return { label: 'Running', color: 'success', icon: 'i-lucide-circle-check' }
    case 'stopped':
      return { label: 'Stopped', color: 'neutral', icon: 'i-lucide-circle-stop' }
    case 'updating':
      return { label: 'Updating', color: 'info', icon: 'i-lucide-loader-circle' }
    case 'deleting':
      return { label: 'Deleting', color: 'warning', icon: 'i-lucide-loader-circle' }
    case 'failed':
      return { label: 'Failed', color: 'error', icon: 'i-lucide-circle-x' }
    default:
      // A status added after this UI was built (the spec allows new enum values).
      return { label: String(status), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

/** The "no filter" value of the list's select boxes. */
export const SITE_FILTER_ALL = 'all'

/** Options for the environment filter; the values are the API's `environment` filter. */
export const siteEnvironmentOptions: readonly { label: string, value: string }[] = [
  { label: 'All sites', value: SITE_FILTER_ALL },
  { label: 'Production sites', value: 'production' },
  { label: 'Staging sites', value: 'staging' },
]

/** Options for the status filter. */
export const siteStatusOptions: readonly { label: string, value: string }[] = [
  { label: 'Any status', value: SITE_FILTER_ALL },
  ...(['provisioning', 'running', 'stopped', 'updating', 'deleting', 'failed'] as const)
    .map(s => ({ label: siteStatusView(s).label, value: s })),
]

/**
 * Start and stop only apply to a settled site: running can be stopped,
 * stopped can be started. A failed site might have failed provisioning,
 * which the API rejects with 409, so the UI offers neither and shows the
 * reason instead.
 */
export function canStartSite(site: Pick<Site, 'status'>): boolean {
  return site.status === 'stopped'
}
export function canStopSite(site: Pick<Site, 'status'>): boolean {
  return site.status === 'running'
}
/** Delete is offered unless the site is already being deleted. */
export function canDeleteSite(site: Pick<Site, 'status'>): boolean {
  return site.status !== 'deleting'
}

/**
 * Whether the "reveal admin credentials" control shows. The API answers 409
 * while provisioning and 410 once retrieved or after failed provisioning, so
 * the control is offered only where it can work.
 */
export function showCredentialsAction(site: Pick<Site, 'status' | 'admin_credentials_available'>): boolean {
  return site.admin_credentials_available && (site.status === 'running' || site.status === 'stopped' || site.status === 'updating')
}

/** The provisioning form's values. */
export interface SiteFormValues {
  name: string
  client_group_id: string
  admin_email: string
  admin_username: string
  php_version: string
}

export const SITE_NAME_MAX = 100
export const ADMIN_USERNAME_MAX = 60
const USERNAME_RE = /^[A-Za-z0-9_.@-]+$/
const PHP_RE = /^[0-9]+\.[0-9]+$/
// Deliberately loose; the server validates the address properly.
const EMAIL_RE = /^[^\s@]+@[^\s@]+$/

/** Client-side checks that mirror the spec; the server's 422 stays authoritative. */
export function validateSite(v: SiteFormValues): Partial<Record<keyof SiteFormValues, string>> {
  const errors: Partial<Record<keyof SiteFormValues, string>> = {}
  const name = v.name.trim()
  if (name.length === 0) errors.name = 'Enter a name.'
  else if (name.length > SITE_NAME_MAX) errors.name = `Use at most ${SITE_NAME_MAX} characters.`
  if (!v.client_group_id) errors.client_group_id = 'Choose a client group.'
  const email = v.admin_email.trim()
  if (email.length === 0) errors.admin_email = 'Enter the admin email address.'
  else if (email.length > 254 || !EMAIL_RE.test(email)) errors.admin_email = 'Enter a valid email address.'
  const username = v.admin_username.trim()
  if (username.length > ADMIN_USERNAME_MAX) errors.admin_username = `Use at most ${ADMIN_USERNAME_MAX} characters.`
  else if (username !== '' && !USERNAME_RE.test(username)) errors.admin_username = 'Use letters, digits and _ . @ - only.'
  const php = v.php_version.trim()
  if (php !== '' && !PHP_RE.test(php)) errors.php_version = 'Use a version like 8.4.'
  return errors
}

/** The POST body; optional fields left blank are omitted so the server's defaults apply. */
export function createSitePayload(v: SiteFormValues): SiteCreate {
  const body: SiteCreate = {
    client_group_id: v.client_group_id,
    name: v.name.trim(),
    admin_email: v.admin_email.trim(),
  }
  if (v.admin_username.trim() !== '') body.admin_username = v.admin_username.trim()
  if (v.php_version.trim() !== '') body.php_version = v.php_version.trim()
  return body
}

/** The group's name for a site row, or a shortened ID when the group list didn't load. */
export function groupLabel(groupNames: Readonly<Record<string, string>>, id: string): string {
  return groupNames[id] ?? id.slice(0, 8)
}
