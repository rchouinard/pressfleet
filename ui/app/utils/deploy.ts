import type { components } from '../api/schema.gen'
import type { Site } from './site'

export type SiteDeployment = components['schemas']['SiteDeployment']
export type DeployKind = SiteDeployment['kind']

export const DEPLOY_NAME_MAX = 100
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/

export const deployKindOptions: readonly { value: DeployKind, label: string, description: string }[] = [
  { value: 'theme', label: 'Theme', description: 'Goes to wp-content/themes.' },
  { value: 'plugin', label: 'Plugin', description: 'Goes to wp-content/plugins.' },
  { value: 'mu-plugin', label: 'Must-use plugin', description: 'Goes to wp-content/mu-plugins. WordPress only loads top-level files there, so a directory needs a loader file.' },
]

export interface DeployFormValues {
  kind: DeployKind
  name: string
  activate: boolean
}

/** Deploys need a running site; the API answers 409 otherwise. */
export function canDeployToSite(site: Pick<Site, 'status'>): boolean {
  return site.status === 'running'
}

/** A reason the deploy form is unavailable, or undefined. */
export function deployBlockedReason(site: Pick<Site, 'status'>): string | undefined {
  return canDeployToSite(site) ? undefined : 'The site must be running to deploy code. Start it first, or wait for the job working on it to finish.'
}

/** Whether "activate" applies: must-use plugins are always active. */
export function canActivate(kind: DeployKind): boolean {
  return kind !== 'mu-plugin'
}

/** Client-side checks mirroring the spec; the server's 422 stays authoritative. */
export function validateDeploy(v: DeployFormValues): Partial<Record<'name', string>> {
  const name = v.name.trim()
  if (name === '') return { name: 'Enter the directory name, for example the theme or plugin slug.' }
  if (name.length > DEPLOY_NAME_MAX) return { name: `Use at most ${DEPLOY_NAME_MAX} characters.` }
  if (!NAME_RE.test(name)) return { name: 'Start with a letter or digit; use letters, digits and . _ - only.' }
  return {}
}

/** The deploy body for a completed upload. */
export function deployPayload(v: DeployFormValues, uploadId: string): SiteDeployment {
  // The generated type makes `activate` required (it has a default); a
  // must-use plugin is always active, so it is sent as false.
  return { upload_id: uploadId, kind: v.kind, name: v.name.trim(), activate: canActivate(v.kind) && v.activate }
}

/** The pieces of a finished deployment job's result worth showing. */
export interface DeployResult {
  kind: string
  name: string
  path: string
  files: number | undefined
  bytes: number | undefined
  replaced: boolean
  activated: boolean
}

/** Reads the job result defensively: its shape isn't part of the typed schema. */
export function deployResult(result: Readonly<Record<string, unknown>> | null | undefined): DeployResult | undefined {
  if (!result || typeof result.name !== 'string') return undefined
  return {
    kind: String(result.kind ?? ''),
    name: result.name,
    path: String(result.path ?? ''),
    files: typeof result.files === 'number' ? result.files : undefined,
    bytes: typeof result.bytes === 'number' ? result.bytes : undefined,
    replaced: result.replaced === true,
    activated: result.activated === true,
  }
}
