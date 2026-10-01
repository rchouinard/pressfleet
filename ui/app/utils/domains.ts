import type { components } from '../api/schema.gen'
import type { ApiError } from './api-error'
import type { Site } from './site'

export type SiteGoLive = components['schemas']['SiteGoLive']
export type SiteAliases = components['schemas']['SiteAliases']

/** The Problem type of a failed DNS check (`422`); see docs/configuration.md#dns-check. */
export const DNS_CHECK_PROBLEM_TYPE = 'https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#dns-check'
export const MAX_ALIASES = 20
const DOMAIN_MAX = 253
// Letters, digits and hyphens per label, at least two labels. Internationalized
// names have to be entered in their xn-- form, like the API requires.
const LABEL = '[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?'
const DOMAIN_RE = new RegExp(`^${LABEL}(?:\\.${LABEL})+$`)

/** Trims, lower-cases and drops one trailing dot, as the server stores domains. */
export function normalizeDomain(raw: string): string {
  return raw.trim().toLowerCase().replace(/\.$/, '')
}

/** Aliases typed one per line (commas and spaces work too); blanks and repeats are dropped. */
export function parseAliases(text: string): string[] {
  const out: string[] = []
  for (const part of text.split(/[\s,]+/)) {
    const d = normalizeDomain(part)
    if (d !== '' && !out.includes(d)) out.push(d)
  }
  return out
}

/** A message when the text is not a plain host name, otherwise undefined. */
export function domainProblem(domain: string): string | undefined {
  if (domain.length > DOMAIN_MAX) return `Use at most ${DOMAIN_MAX} characters.`
  if (!DOMAIN_RE.test(domain)) return 'Enter a host name such as www.example.com, without https://, a port, a path or a wildcard.'
  return undefined
}

export interface DomainFormErrors {
  primary?: string
  aliases?: string
}

/**
 * Client-side checks that mirror the spec; the server's 422 stays
 * authoritative. Pass `primaryRaw` undefined when only aliases are edited
 * (then an alias can't be compared with the primary here).
 */
export function validateDomains(primaryRaw: string | undefined, aliases: readonly string[], primaryForCompare?: string): DomainFormErrors {
  const errors: DomainFormErrors = {}
  const primary = primaryRaw === undefined ? primaryForCompare : normalizeDomain(primaryRaw)
  if (primaryRaw !== undefined) {
    const p = primary === '' ? 'Enter the primary domain.' : domainProblem(primary ?? '')
    if (p) errors.primary = p
  }
  if (aliases.length > MAX_ALIASES) {
    errors.aliases = `Use at most ${MAX_ALIASES} aliases.`
    return errors
  }
  for (const a of aliases) {
    const p = domainProblem(a)
    if (p) {
      errors.aliases = `${a}: ${p}`
      return errors
    }
    if (a === primary) {
      errors.aliases = `${a} is the primary domain; an alias must be a different domain.`
      return errors
    }
  }
  return errors
}

export function goLivePayload(primary: string, aliases: readonly string[], skipDnsCheck: boolean): SiteGoLive {
  const body: SiteGoLive = { primary_domain: normalizeDomain(primary), aliases: [...aliases], skip_dns_check: skipDnsCheck }
  return body
}

export function aliasesPayload(aliases: readonly string[], skipDnsCheck: boolean): SiteAliases {
  const body: SiteAliases = { aliases: [...aliases], skip_dns_check: skipDnsCheck }
  return body
}

/** Whether the site has been switched to a production domain. */
export function siteIsLive(site: Pick<Site, 'primary_domain'>): boolean {
  return site.primary_domain != null && site.primary_domain !== ''
}

/** Go-live (first time, or moving to another primary domain) needs a running site: the API answers 409 otherwise. */
export function canGoLive(site: Pick<Site, 'status'>): boolean {
  return site.status === 'running'
}

/** Aliases can change once the site is live and settled; a stopped site is fine. */
export function canEditAliases(site: Pick<Site, 'status' | 'primary_domain'>): boolean {
  return siteIsLive(site) && (site.status === 'running' || site.status === 'stopped')
}

/** Why the controls are unavailable right now, or undefined when something can be done. */
export function domainsBlockedReason(site: Pick<Site, 'status' | 'primary_domain'>): string | undefined {
  switch (site.status) {
    case 'provisioning': return 'Domains can be changed once the site has finished provisioning.'
    case 'updating': return 'The site is being updated. Domains can be changed when that finishes.'
    case 'deleting': return 'The site is being deleted.'
    case 'failed': return 'Domains can\'t be changed while the site is in a failed state.'
    case 'stopped': return siteIsLive(site) ? undefined : 'Start the site to go live; going live needs it running.'
    default: return undefined
  }
}

export interface DnsFailure {
  domain: string
  detail: string
}

/** Whether the error is a DNS check failure, with or without per-domain entries. */
export function isDnsCheckError(e: ApiError): boolean {
  return e.status === 422 && e.type === DNS_CHECK_PROBLEM_TYPE
}

/**
 * The per-domain lines of a DNS check failure. `errors[].field` is a JSON
 * Pointer into the request (`/primary_domain`, `/aliases/1`), which is mapped
 * back to the domain that was sent so the line can name it.
 */
export function dnsFailures(e: ApiError, sent: { primary?: string, aliases: readonly string[] }): DnsFailure[] {
  return e.errors.map((fe) => {
    let domain = fe.field
    if (fe.field === '/primary_domain' && sent.primary) domain = sent.primary
    const m = /^\/aliases\/(\d+)$/.exec(fe.field)
    if (m) domain = sent.aliases[Number(m[1])] ?? fe.field
    return { domain, detail: fe.detail }
  })
}
