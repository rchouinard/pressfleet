import { describe, expect, it } from 'vitest'
import {
  arrangeSites, backupChoices, deleteBlockers, emptyStagingForm, missingProductionIds, outboundMailPayload,
  stagingBlockedReason, stagingPayload, validateStagingForm,
} from '../app/utils/staging'
import { isStaging, productionIdOf, siteEnvironmentOptions, siteOptionLabel } from '../app/utils/site'
import { jobTypeLabel } from '../app/utils/job'
import { can } from '../app/utils/permissions'
import type { Backup } from '../app/utils/backup'
import { site, stagingSite } from './site-fixtures'

describe('staging form', () => {
  it('sends nothing for a blank form, so the server copies production', () => {
    expect(stagingPayload(emptyStagingForm())).toEqual({})
  })

  it('sends only what was filled in', () => {
    expect(stagingPayload({ ...emptyStagingForm(), name: ' Try redesign ', cpu: '0.5', memory_limit_mb: '256' })).toEqual({
      name: 'Try redesign',
      resources: { cpu: '0.5', memory_limit_mb: 256 },
    })
  })

  it('sends the snapshot only for "an existing backup"', () => {
    expect(stagingPayload({ ...emptyStagingForm(), source: 'backup', snapshot_id: 'abc12345' })).toEqual({ snapshot_id: 'abc12345' })
    expect(stagingPayload({ ...emptyStagingForm(), source: 'fresh', snapshot_id: 'abc12345' })).toEqual({})
  })

  it('validates like the spec', () => {
    expect(validateStagingForm(emptyStagingForm())).toEqual({})
    expect(validateStagingForm({ ...emptyStagingForm(), name: 'a'.repeat(101) }).name).toBeDefined()
    expect(validateStagingForm({ ...emptyStagingForm(), source: 'backup' }).snapshot_id).toBeDefined()
    expect(validateStagingForm({ ...emptyStagingForm(), cpu: '0' }).cpu).toBeDefined()
    expect(validateStagingForm({ ...emptyStagingForm(), memory_reservation_mb: '16' }).memory_reservation_mb).toBeDefined()
    expect(validateStagingForm({ ...emptyStagingForm(), memory_reservation_mb: '256', memory_limit_mb: '128' }).memory_limit_mb).toBeDefined()
    expect(validateStagingForm({ ...emptyStagingForm(), cpu: '0.5', memory_reservation_mb: '64', memory_limit_mb: '128' })).toEqual({})
  })
})

describe('stagingBlockedReason', () => {
  it('allows running and stopped production sites only', () => {
    expect(stagingBlockedReason(site({ status: 'running' }))).toBeUndefined()
    expect(stagingBlockedReason(site({ status: 'stopped' }))).toBeUndefined()
    for (const status of ['provisioning', 'updating', 'deleting', 'failed'] as const) {
      expect(stagingBlockedReason(site({ status }))).toBeDefined()
    }
  })

  it('refuses staging sites, which cannot be nested', () => {
    expect(stagingBlockedReason(stagingSite('s2', 's1'))).toContain('staging sites of its own')
  })
})

describe('backupChoices', () => {
  const backup = (over: Partial<Backup>): Backup => ({
    id: 'b1', site_id: 's1', site_name: 'Acme', site_slug: 'acme', client_group_id: 'g1', kind: 'manual', status: 'succeeded',
    snapshot_id: 'snap0001', wp_core_version: '6.9.1', size_bytes: 1, added_bytes: 1, file_count: 1, message: '', job_id: null,
    created_at: '2026-10-01T10:00:00Z', started_at: '2026-10-01T10:00:00Z', finished_at: '2026-10-01T10:01:00Z', expired_at: null,
    ...over,
  })

  it('offers restorable snapshots only, labelled with their kind', () => {
    const choices = backupChoices([
      backup({ id: 'a', snapshot_id: 'snapA' }),
      backup({ id: 'b', status: 'failed', snapshot_id: null }),
      backup({ id: 'c', snapshot_id: 'snapC', expired_at: '2026-10-02T00:00:00Z' }),
      backup({ id: 'd', snapshot_id: 'snapD', kind: 'sync' }),
    ], 'en-US')
    expect(choices.map(c => c.value)).toEqual(['snapA', 'snapD'])
    expect(choices[0]!.label).toContain('Manual')
    expect(choices[1]!.label).toContain('Staging sync')
  })
})

describe('environment helpers', () => {
  it('tells staging from production and finds the production site', () => {
    const staging = stagingSite('s2', 's1')
    expect(isStaging(staging)).toBe(true)
    expect(isStaging(site())).toBe(false)
    expect(productionIdOf(staging)).toBe('s1')
    expect(productionIdOf(site())).toBeUndefined()
  })

  it('labels staging sites in pickers', () => {
    expect(siteOptionLabel(site())).toBe('Acme (acme)')
    expect(siteOptionLabel(stagingSite('s2', 's1'))).toBe('Staging s2 (acme-staging-s2) - staging')
  })

  it('offers the API\'s environment filter values', () => {
    expect(siteEnvironmentOptions.map(o => o.value)).toEqual(['all', 'production', 'staging'])
  })

  it('builds the outbound mail body', () => {
    expect(outboundMailPayload(true)).toEqual({ outbound_mail: true })
    expect(outboundMailPayload(false)).toEqual({ outbound_mail: false })
  })

  it('names the staging sites that block deleting a production site, except those being deleted', () => {
    const blockers = deleteBlockers([stagingSite('s2', 's1'), stagingSite('s3', 's1', { status: 'deleting' }), stagingSite('s4', 's1', { status: 'failed' })])
    expect(blockers.map(s => s.id)).toEqual(['s2', 's4'])
  })
})

describe('arrangeSites', () => {
  it('lists each staging site right under its production site', () => {
    const rows = arrangeSites([site({ id: 'p2', name: 'Other' }), stagingSite('s2', 'p1'), site({ id: 'p1' }), stagingSite('s3', 'p2')])
    expect(rows.map(r => [r.site.id, r.grouped])).toEqual([['p2', false], ['s3', true], ['p1', false], ['s2', true]])
  })

  it('keeps a staging site whose production site is not in the list, and says whose it is', () => {
    const rows = arrangeSites([site({ id: 'p1' }), stagingSite('s9', 'gone')])
    expect(rows.map(r => r.site.id)).toEqual(['p1', 's9'])
    expect(rows[1]).toMatchObject({ grouped: false, parentId: 'gone' })
    expect(rows).toHaveLength(2)
  })

  it('never drops or repeats a site', () => {
    const items = [stagingSite('s1', 'a'), stagingSite('s2', 'a'), site({ id: 'a' }), site({ id: 'b' }), stagingSite('s3', 'zzz')]
    expect(arrangeSites(items).map(r => r.site.id).sort()).toEqual(items.map(s => s.id).sort())
  })

  it('finds the production sites a list is missing, once each', () => {
    expect(missingProductionIds([site({ id: 'p1' }), stagingSite('s1', 'p1'), stagingSite('s2', 'gone'), stagingSite('s3', 'gone')])).toEqual(['gone'])
    expect(missingProductionIds([site()])).toEqual([])
  })
})

describe('staging actions and jobs', () => {
  it('lets operators, not viewers, create and delete staging sites', () => {
    expect(can('viewer', 'site.staging.create')).toBe(false)
    expect(can('operator', 'site.staging.create')).toBe(true)
    expect(can('operator', 'site.staging.delete')).toBe(true)
    // Deleting a production site stays an Admin action.
    expect(can('operator', 'site.delete')).toBe(false)
  })

  it('labels the clone job', () => {
    expect(jobTypeLabel('site.clone')).toBe('Create staging site')
  })
})
