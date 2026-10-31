import type { Site } from '../app/utils/site'

export function site(over: Partial<Site> = {}): Site {
  return {
    id: 's1', client_group_id: 'g1', name: 'Acme', slug: 'acme', preview_domain: 'acme.preview.example',
    primary_domain: null, aliases: [], php_version: '8.4', wp_core_version: '6.9.1',
    resources: { cpu: '1.0', memory_reservation_mb: 128, memory_limit_mb: 512 },
    preview_protection: { enabled: false }, maintenance: false, status: 'running',
    environment: 'production', parent_site_id: null, staging: null,
    admin_credentials_available: false, created_at: '2026-01-02T00:00:00Z', updated_at: '2026-01-02T00:00:00Z',
    ...over,
  } as Site
}

export function stagingSite(id: string, parent: string, over: Partial<Site> = {}): Site {
  return site({
    id, name: `Staging ${id}`, slug: `acme-staging-${id}`, environment: 'staging', parent_site_id: parent,
    staging: { production_site_id: parent, last_refreshed_at: null, last_snapshot_id: null, last_pushed_at: null, outbound_mail: false },
    ...over,
  })
}
