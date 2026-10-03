import type { components } from '../api/schema.gen'

/**
 * PHP versions, their support status and end-of-life dates, as listed by
 * `GET /api/v1/catalog/php-versions` (design doc §5.5). The server holds the
 * only copy (the image manifest) and computes each status; this file only
 * decides how a status looks and what to warn about.
 */
export type PhpVersionInfo = components['schemas']['PHPVersionInfo']
export type PhpVersionCatalog = components['schemas']['PHPVersionCatalog']

export type PhpStatus = 'active' | 'security' | 'eol-soon' | 'eol' | 'unknown'

export interface PhpStatusView {
  status: PhpStatus
  /** Short text; shown with an icon, never colour alone. */
  label: string
  color: 'success' | 'info' | 'warning' | 'error' | 'neutral'
  icon: string
  /** A sentence for the warning line; absent when there is nothing to warn about. */
  warning?: string
}

const views: Record<PhpVersionInfo['status'], PhpStatusView> = {
  'active': { status: 'active', label: 'Supported', color: 'success', icon: 'i-lucide-circle-check' },
  'security-only': { status: 'security', label: 'Security fixes only', color: 'info', icon: 'i-lucide-shield' },
  'eol-soon': { status: 'eol-soon', label: 'End of life soon', color: 'warning', icon: 'i-lucide-triangle-alert' },
  'eol': { status: 'eol', label: 'End of life', color: 'error', icon: 'i-lucide-circle-x' },
}

/**
 * The support status of a PHP version ("8.4") among the versions the API
 * listed. A version it doesn't list, or a list not loaded yet, is unknown.
 */
export function phpStatus(version: string, versions: readonly PhpVersionInfo[] = []): PhpStatusView {
  const b = versions.find(p => p.version === version)
  if (!b) return { status: 'unknown', label: 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help' }
  const view = { ...views[b.status] }
  if (b.status === 'eol') {
    view.warning = `PHP ${version} reached end of life on ${b.security_support_ends} and gets no security fixes. Move this site to a supported version.`
  } else if (b.status === 'eol-soon') {
    view.warning = `PHP ${version} gets security fixes only, until ${b.security_support_ends}. Plan a move to a newer version.`
  }
  return view
}

/**
 * The versions to offer in the selector: those not past end of life, plus the
 * site's current one (so the select can show it), newest first. A past-EOL
 * target is rejected by the server (`422`), so it is never offered.
 */
export function phpOptions(current: string, versions: readonly PhpVersionInfo[] = []): { label: string, value: string }[] {
  const offered = versions.filter(b => b.status !== 'eol').map(b => b.version)
  if (!offered.includes(current)) offered.push(current)
  return offered.map((v) => {
    const s = phpStatus(v, versions)
    return { value: v, label: v === current ? `${v} (current, ${s.label.toLowerCase()})` : `${v} (${s.label.toLowerCase()})` }
  })
}

/** The create form's choice for "no version chosen": the request omits `php_version`. */
export const PHP_DEFAULT_CHOICE = 'default'

/**
 * The versions to offer when creating a site: "Platform default" first (the
 * preselected choice, which sends no version), then every version not past
 * end of life, newest first, each with its status. EOL versions are left out
 * as in the settings selector, because the server rejects them (`422`). With
 * no catalog (still loading, or failed) only the platform default is offered.
 */
export function phpCreateOptions(versions: readonly PhpVersionInfo[] = [], defaultVersion?: string): { label: string, value: string }[] {
  const def = defaultVersion
    ? `Platform default (PHP ${defaultVersion}, ${phpStatus(defaultVersion, versions).label.toLowerCase()})`
    : 'Platform default'
  return [
    { value: PHP_DEFAULT_CHOICE, label: def },
    ...versions.filter(b => b.status !== 'eol').map(b => ({
      value: b.version,
      label: `PHP ${b.version} (${phpStatus(b.version, versions).label.toLowerCase()})`,
    })),
  ]
}
