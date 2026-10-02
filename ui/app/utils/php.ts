/**
 * PHP branch support dates, for the end-of-life warnings of design doc §5.5.
 *
 * The API has no endpoint that lists the PHP versions the platform builds
 * images for (`PHPVersion` is only a pattern), so the dates are kept here,
 * copied from `images/manifest.json` (and php.net for older branches). The
 * server stays authoritative: it rejects versions without images (`422`),
 * and this table only decides what to warn about and offer. Keep it in step
 * with the manifest when a branch is added.
 */
export interface PhpBranch {
  version: string
  /** Last day of active support (bug fixes). */
  activeSupportEnds: string
  /** Last day of security support: the branch's end of life. */
  securitySupportEnds: string
}

export const phpBranches: readonly PhpBranch[] = [
  { version: '8.5', activeSupportEnds: '2027-12-31', securitySupportEnds: '2029-12-31' },
  { version: '8.4', activeSupportEnds: '2026-12-31', securitySupportEnds: '2028-12-31' },
  { version: '8.3', activeSupportEnds: '2025-12-31', securitySupportEnds: '2027-12-31' },
  { version: '8.2', activeSupportEnds: '2024-12-31', securitySupportEnds: '2026-12-31' },
  { version: '8.1', activeSupportEnds: '2023-12-31', securitySupportEnds: '2025-12-31' },
  { version: '8.0', activeSupportEnds: '2023-11-26', securitySupportEnds: '2023-11-26' },
  { version: '7.4', activeSupportEnds: '2021-11-28', securitySupportEnds: '2022-11-28' },
]

/** Warn this long before security support ends (the server's `EOLSoonWindow`). */
export const EOL_SOON_DAYS = 180

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

const DAY_MS = 24 * 60 * 60 * 1000

/** End of a date-only string as a timestamp (the day itself still counts as supported). */
function endOfDay(date: string): number {
  return Date.parse(`${date}T23:59:59Z`)
}

/** The support status of a PHP version ("8.4") on `now`. */
export function phpStatus(version: string, now: Date = new Date()): PhpStatusView {
  const b = phpBranches.find(p => p.version === version)
  if (!b) return { status: 'unknown', label: 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help' }
  const t = now.getTime()
  const security = endOfDay(b.securitySupportEnds)
  if (t > security) {
    return {
      status: 'eol',
      label: 'End of life',
      color: 'error',
      icon: 'i-lucide-circle-x',
      warning: `PHP ${version} reached end of life on ${b.securitySupportEnds} and gets no security fixes. Move this site to a supported version.`,
    }
  }
  if (security - t <= EOL_SOON_DAYS * DAY_MS) {
    return {
      status: 'eol-soon',
      label: 'End of life soon',
      color: 'warning',
      icon: 'i-lucide-triangle-alert',
      warning: `PHP ${version} gets security fixes only, until ${b.securitySupportEnds}. Plan a move to a newer version.`,
    }
  }
  if (t > endOfDay(b.activeSupportEnds)) {
    return { status: 'security', label: 'Security fixes only', color: 'info', icon: 'i-lucide-shield' }
  }
  return { status: 'active', label: 'Supported', color: 'success', icon: 'i-lucide-circle-check' }
}

/**
 * The versions to offer in the selector: those not past end of life, plus the
 * site's current one (so the select can show it), newest first. A past-EOL
 * target is rejected by the server (`422`), so it is never offered.
 */
export function phpOptions(current: string, now: Date = new Date()): { label: string, value: string }[] {
  const versions = phpBranches.filter(b => phpStatus(b.version, now).status !== 'eol').map(b => b.version)
  if (!versions.includes(current)) versions.push(current)
  return versions.map((v) => {
    const s = phpStatus(v, now)
    return { value: v, label: v === current ? `${v} (current, ${s.label.toLowerCase()})` : `${v} (${s.label.toLowerCase()})` }
  })
}
