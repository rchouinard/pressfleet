import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ExportDownloads from '../app/components/export/ExportDownloads.vue'
import type { Export } from '../app/utils/exports'
import { uiStubs } from './stubs'

const ready: Export = {
  id: '01exp', kind: 'snapshot', site_id: 's1', site_slug: 'acme', client_group_id: 'g1', status: 'ready',
  backup_id: '01bkp', snapshot_id: 'abcdef01',
  source: { site_url: 'https://acme.example', table_prefix: 'acme_', wp_core_version: '6.8.3', php_version: '', taken_at: '2026-09-30T02:00:00Z' },
  files: [{ name: 'database.sql.gz', size_bytes: 2048, download_url: 'ignored' }, { name: 'wp-content.tar.gz', size_bytes: 3 * 1024 * 1024, download_url: 'ignored' }],
  size_bytes: 3 * 1024 * 1024 + 2048, message: '', job_id: null, created_at: '2026-10-01T00:00:00Z', ready_at: '2026-10-01T00:01:00Z',
  expires_at: '2026-10-02T00:01:00Z', expired_at: null,
}

describe('ExportDownloads', () => {
  it('links each file by ID and says what the export holds', () => {
    const w = mount(ExportDownloads, { props: { exports: [ready] }, global: { stubs: uiStubs } })
    const links = w.findAll('a[download]')
    expect(links.map(a => a.attributes('href'))).toEqual([
      '/api/v1/exports/01exp/files/database.sql.gz',
      '/api/v1/exports/01exp/files/wp-content.tar.gz',
    ])
    expect(w.text()).toContain('Database dump (2.0 KiB)')
    expect(w.text()).toContain('wp-content archive (3.0 MiB)')
    expect(w.text()).toContain('Backup as of')
    expect(w.text()).toContain('https://acme.example')
    expect(w.text()).toContain('acme_')
    expect(w.text()).toContain('6.8.3 / -')
  })
})
