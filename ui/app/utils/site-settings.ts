import type { components } from '../api/schema.gen'
import type { Site } from './site'

export type SiteUpdate = components['schemas']['SiteUpdate']
export type ResourceLimits = components['schemas']['ResourceLimits']
export type PreviewCredentials = components['schemas']['PreviewCredentials']
export type SiteCore = components['schemas']['SiteCore']
export type CorePolicyUpdate = components['schemas']['CorePolicyUpdate']

/**
 * Why site settings can't be changed now, or undefined if they can. The API
 * answers 409 for these states, so the controls explain instead.
 */
export function settingsBlockedReason(site: Pick<Site, 'status'>): string | undefined {
  switch (site.status) {
    case 'provisioning': return 'The site is still being provisioned. Settings can be changed once it is ready.'
    case 'updating': return 'The site is being updated. Settings can be changed when the job has finished.'
    case 'deleting': return 'The site is being deleted.'
    case 'failed': return 'The site is in a failed state. Settings can be changed once it is repaired.'
    default: return undefined
  }
}

export interface ResourceFormValues {
  cpu: string
  memory_reservation_mb: string
  memory_limit_mb: string
}

export type ResourceFormErrors = Partial<Record<keyof ResourceFormValues, string>>

export function resourceFormValues(r: ResourceLimits): ResourceFormValues {
  return { cpu: r.cpu, memory_reservation_mb: String(r.memory_reservation_mb), memory_limit_mb: String(r.memory_limit_mb) }
}

const CPU_RE = /^[0-9]+(\.[0-9]+)?$/
const INT_RE = /^[0-9]+$/

/** Client-side checks that mirror the spec; the server's 422 stays authoritative. */
export function validateResources(v: ResourceFormValues): ResourceFormErrors {
  const errors: ResourceFormErrors = {}
  const cpu = v.cpu.trim()
  if (!CPU_RE.test(cpu)) errors.cpu = 'Use a number of cores such as 1 or 0.5.'
  else if (Number(cpu) <= 0) errors.cpu = 'Must be more than 0.'
  const res = v.memory_reservation_mb.trim()
  const lim = v.memory_limit_mb.trim()
  if (!INT_RE.test(res) || Number(res) < 32) errors.memory_reservation_mb = 'Whole MiB, at least 32.'
  if (!INT_RE.test(lim) || Number(lim) < 64) errors.memory_limit_mb = 'Whole MiB, at least 64.'
  else if (!errors.memory_reservation_mb && Number(lim) < Number(res)) errors.memory_limit_mb = 'Must be at least the reservation.'
  return errors
}

/** PATCH body with only the limits that changed, or undefined if none did. */
export function resourcesPayload(current: ResourceLimits, v: ResourceFormValues): SiteUpdate | undefined {
  const r: NonNullable<SiteUpdate['resources']> = {}
  if (v.cpu.trim() !== current.cpu) r.cpu = v.cpu.trim()
  if (Number(v.memory_reservation_mb) !== current.memory_reservation_mb) r.memory_reservation_mb = Number(v.memory_reservation_mb)
  if (Number(v.memory_limit_mb) !== current.memory_limit_mb) r.memory_limit_mb = Number(v.memory_limit_mb)
  return Object.keys(r).length > 0 ? { resources: r } : undefined
}

/** PATCH body for a PHP version change, or undefined when it is unchanged. */
export function phpPayload(current: string, next: string): SiteUpdate | undefined {
  return next !== '' && next !== current ? { php_version: next } : undefined
}

/** PATCH body that turns preview protection on or off. */
export function previewProtectionPayload(enabled: boolean): SiteUpdate {
  return { preview_protection: { enabled } }
}

/** Core policy bodies. `{}` clears the site's own policy so it follows its group. */
export const coreHold = (): CorePolicyUpdate => ({ hold: true })
export const coreApprove = (branch: string): CorePolicyUpdate => ({ approved_branch: branch })
export const coreFollowGroup = (): CorePolicyUpdate => ({})

/** Plain-language description of whose core policy applies. */
export function coreDecisionText(core: Pick<SiteCore, 'decided_by' | 'held' | 'approved_branch'>): string {
  const who = {
    default: 'the platform default',
    site: 'this site\'s own policy',
    client_group: 'its client group\'s policy',
    auto: 'automatic approval',
  }[core.decided_by] ?? core.decided_by
  if (core.held) return `Held on its current major branch by ${who}. Minor and security releases still roll out.`
  if (core.approved_branch) return `Major releases up to ${core.approved_branch} are approved by ${who}.`
  return `Minor and security releases roll out automatically; major releases wait for approval (${who}).`
}

/** Whether "Update now" can work: the API needs a target and a running, settled site. */
export function canUpdateCoreNow(site: Pick<Site, 'status'>, core: Pick<SiteCore, 'target'>): boolean {
  return core.target !== undefined && site.status === 'running'
}

/** Whether a site's own core policy is set (so "follow the client group" does something). */
export function hasOwnCorePolicy(core: Pick<SiteCore, 'policy'>): boolean {
  return core.policy.hold || core.policy.approved_branch !== undefined
}
