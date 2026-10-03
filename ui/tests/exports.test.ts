import { describe, expect, it } from 'vitest'
import {
  canExportSite, downloadableExports, exportFileLabel, exportFileUrl, exportIdOf, exportKindLabel, isDownloadable,
  isExportable, isSiteExportJob, type Export,
} from '../app/utils/exports'

function exp(over: Partial<Export> = {}): Export {
  return {
    id: '01j9z3k4m5n6p7q8r9s0t1v2w3', kind: 'site', site_id: 's1', site_slug: 'acme', client_group_id: 'g1', status: 'ready',
    backup_id: null, snapshot_id: null,
    source: { site_url: 'https://acme.example', table_prefix: 'wp_', wp_core_version: '6.9.1', php_version: '8.4', taken_at: null },
    files: [], size_bytes: 10, message: '', job_id: null, created_at: '2026-10-01T00:00:00Z', ready_at: '2026-10-01T00:01:00Z',
    expires_at: '2026-10-02T00:01:00Z', expired_at: null,
    ...over,
  }
}

describe('exports', () => {
  it('builds same-origin download links from the IDs', () => {
    expect(exportFileUrl('01abc', 'wp-content.tar.gz')).toBe('/api/v1/exports/01abc/files/wp-content.tar.gz')
    expect(exportFileUrl('a/b', 'x y')).toBe('/api/v1/exports/a%2Fb/files/x%20y')
  })

  it('names files and kinds', () => {
    expect(exportFileLabel('wp-content.tar.gz')).toBe('wp-content archive')
    expect(exportFileLabel('database.sql.gz')).toBe('Database dump')
    expect(exportFileLabel('other')).toBe('other')
    expect(exportKindLabel({ kind: 'site' })).toBe('Live site')
    expect(exportKindLabel({ kind: 'snapshot' })).toBe('Backup')
  })

  it('offers only ready exports before their expiry', () => {
    const now = Date.parse('2026-10-01T12:00:00Z')
    expect(isDownloadable(exp(), now)).toBe(true)
    expect(isDownloadable(exp({ expires_at: '2026-10-01T12:00:00Z' }), now)).toBe(false)
    expect(isDownloadable(exp({ status: 'running', expires_at: null }), now)).toBe(false)
    expect(isDownloadable(exp({ status: 'expired' }), now)).toBe(false)
    const list = [exp({ id: 'a' }), exp({ id: 'b', status: 'failed' }), exp({ id: 'c', expires_at: '2026-10-01T00:00:00Z' })]
    expect(downloadableExports(list, now).map(e => e.id)).toEqual(['a'])
  })

  it('knows which sites and backups can be exported', () => {
    expect(canExportSite({ status: 'running' })).toBe(true)
    expect(canExportSite({ status: 'stopped' })).toBe(true)
    expect(canExportSite({ status: 'provisioning' })).toBe(false)
    expect(canExportSite({ status: 'failed' })).toBe(false)
    expect(isExportable({ status: 'succeeded', snapshot_id: 'abc', expired_at: null })).toBe(true)
    expect(isExportable({ status: 'succeeded', snapshot_id: 'abc', expired_at: '2026-10-01T00:00:00Z' })).toBe(false)
    expect(isExportable({ status: 'running', snapshot_id: null, expired_at: null })).toBe(false)
  })

  it('reads export jobs', () => {
    expect(isSiteExportJob({ type: 'site.export' })).toBe(true)
    expect(isSiteExportJob({ type: 'backup.export' })).toBe(true)
    expect(isSiteExportJob({ type: 'site.database_export' })).toBe(false)
    expect(exportIdOf({ state: 'succeeded', result: { export_id: 'x' } })).toBe('x')
    expect(exportIdOf({ state: 'running', result: { export_id: 'x' } })).toBeUndefined()
    expect(exportIdOf({ state: 'succeeded', result: null })).toBeUndefined()
  })
})
