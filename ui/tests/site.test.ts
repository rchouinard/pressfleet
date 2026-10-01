import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import {
  activeJob, canDeleteSite, canStartSite, canStopSite, createSitePayload, groupLabel,
  showCredentialsAction, siteStatusOptions, siteStatusView, validateSite,
  type Site, type SiteFormValues, type SiteStatus,
} from '../app/utils/site'
import { can } from '../app/utils/permissions'
import SiteTable from '../app/components/site/SiteTable.vue'
import SiteCredentials from '../app/components/site/SiteCredentials.vue'
import { uiStubs } from './stubs'
import type { Job } from '../app/utils/job'

const statuses: SiteStatus[] = ['provisioning', 'running', 'stopped', 'updating', 'deleting', 'failed']
const valid: SiteFormValues = { name: 'Acme', client_group_id: 'g1', admin_email: 'a@acme.example', admin_username: '', php_version: '' }

describe('siteStatusView', () => {
  it('gives every status a label and an icon, never colour alone', () => {
    for (const s of statuses) {
      const v = siteStatusView(s)
      expect(v.label).not.toBe('')
      expect(v.icon).toMatch(/^i-lucide-/)
    }
    expect(siteStatusView('failed').color).toBe('error')
    expect(siteStatusView('running').color).toBe('success')
  })

  it('shows an unknown future status as sent', () => {
    expect(siteStatusView('hibernating' as SiteStatus).label).toBe('hibernating')
  })

  it('offers each status in the filter, after "any"', () => {
    expect(siteStatusOptions[0]!.value).toBe('all')
    expect(siteStatusOptions.slice(1).map(o => o.value)).toEqual(statuses)
  })
})

describe('start, stop and delete availability', () => {
  it('starts only stopped sites and stops only running ones', () => {
    expect(statuses.filter(s => canStartSite({ status: s }))).toEqual(['stopped'])
    expect(statuses.filter(s => canStopSite({ status: s }))).toEqual(['running'])
  })

  it('offers delete unless the site is already being deleted', () => {
    expect(statuses.filter(s => !canDeleteSite({ status: s }))).toEqual(['deleting'])
  })
})

describe('showCredentialsAction', () => {
  it('needs credentials that can still be retrieved on a provisioned site', () => {
    expect(showCredentialsAction({ status: 'running', admin_credentials_available: true })).toBe(true)
    expect(showCredentialsAction({ status: 'running', admin_credentials_available: false })).toBe(false)
    // 409 while provisioning, 410 after failed provisioning.
    expect(showCredentialsAction({ status: 'provisioning', admin_credentials_available: true })).toBe(false)
    expect(showCredentialsAction({ status: 'failed', admin_credentials_available: true })).toBe(false)
  })
})

describe('activeJob', () => {
  const job = (id: string, state: Job['state']) => ({ id, state }) as Job
  it('picks the newest job that has not finished', () => {
    expect(activeJob([job('a', 'succeeded'), job('b', 'running'), job('c', 'queued')])?.id).toBe('b')
    expect(activeJob([job('a', 'failed'), job('b', 'rollback_failed')])).toBeUndefined()
    expect(activeJob([])).toBeUndefined()
  })
})

describe('validateSite', () => {
  it('accepts the minimal form', () => {
    expect(validateSite(valid)).toEqual({})
  })

  it('requires name, group and a plausible email', () => {
    const e = validateSite({ ...valid, name: ' ', client_group_id: '', admin_email: 'nope' })
    expect(Object.keys(e).sort()).toEqual(['admin_email', 'client_group_id', 'name'])
    expect(validateSite({ ...valid, admin_email: '' }).admin_email).toBeDefined()
  })

  it('bounds the name and checks the optional username and PHP version like the spec', () => {
    expect(validateSite({ ...valid, name: 'a'.repeat(101) }).name).toBeDefined()
    expect(validateSite({ ...valid, name: 'a'.repeat(100) })).toEqual({})
    expect(validateSite({ ...valid, admin_username: 'bad name' }).admin_username).toBeDefined()
    expect(validateSite({ ...valid, admin_username: 'a'.repeat(61) }).admin_username).toBeDefined()
    expect(validateSite({ ...valid, admin_username: 'ed.it_or@x-y' })).toEqual({})
    expect(validateSite({ ...valid, php_version: '8' }).php_version).toBeDefined()
    expect(validateSite({ ...valid, php_version: '8.4' })).toEqual({})
  })
})

describe('createSitePayload', () => {
  it('trims and leaves blank optional fields out', () => {
    expect(createSitePayload({ ...valid, name: ' Acme ', admin_email: ' a@acme.example ' })).toEqual({
      client_group_id: 'g1', name: 'Acme', admin_email: 'a@acme.example',
    })
  })

  it('sends the optional fields when given', () => {
    expect(createSitePayload({ ...valid, admin_username: 'ed', php_version: '8.4' })).toMatchObject({ admin_username: 'ed', php_version: '8.4' })
  })
})

describe('groupLabel', () => {
  it('falls back to a short ID when the group list did not load', () => {
    expect(groupLabel({ g1: 'Acme' }, 'g1')).toBe('Acme')
    expect(groupLabel({}, '0123456789abcdef')).toBe('01234567')
  })
})

describe('site role gating', () => {
  it('viewers read, operators create and start/stop, only admins delete', () => {
    const rows = [
      ['viewer', true, false, false, false],
      ['operator', true, true, true, false],
      ['admin', true, true, true, true],
    ] as const
    for (const [role, view, create, power, del] of rows) {
      expect(can(role, 'site.view')).toBe(view)
      expect(can(role, 'site.create')).toBe(create)
      expect(can(role, 'site.start')).toBe(power)
      expect(can(role, 'site.stop')).toBe(power)
      expect(can(role, 'site.credentials.retrieve')).toBe(create)
      expect(can(role, 'site.delete')).toBe(del)
    }
  })
})

const NuxtLink = defineComponent({ props: ['to'], template: '<a :href="to"><slot /></a>' })

describe('SiteTable', () => {
  const site = (over: Partial<Site>): Site => ({
    id: 's1', client_group_id: 'g1', name: 'Acme site', slug: 'acme', preview_domain: 'acme.preview.example',
    primary_domain: null, aliases: [], php_version: '8.4', wp_core_version: '6.9.1',
    resources: { cpu: '1.0', memory_reservation_mb: 128, memory_limit_mb: 512 },
    preview_protection: { enabled: false }, maintenance: false, status: 'running', parent_site_id: null,
    admin_credentials_available: true, created_at: '2026-01-02T00:00:00Z', updated_at: '2026-01-02T00:00:00Z',
    ...over,
  }) as Site

  it('shows name link, status text, group name and the preview domain until live', () => {
    const w = mount(SiteTable, {
      props: { sites: [site({}), site({ id: 's2', name: 'Live', primary_domain: 'www.acme.example', status: 'stopped' })], groupNames: { g1: 'Acme Inc' } },
      global: { stubs: { ...uiStubs, NuxtLink } },
    })
    expect(w.find('a[href="/sites/s1"]').text()).toBe('Acme site')
    expect(w.text()).toContain('Running')
    expect(w.text()).toContain('Stopped')
    expect(w.text()).toContain('Acme Inc')
    expect(w.text()).toContain('acme.preview.example')
    expect(w.text()).toContain('www.acme.example')
  })
})

describe('SiteCredentials', () => {
  const credentials = { username: 'admin', email: 'a@acme.example', password: 's3cret-pw', login_url: 'https://x.example/wp-login.php' }

  it('shows the password with the show-once warning and copies it', async () => {
    const copied: string[] = []
    Object.defineProperty(globalThis.navigator, 'clipboard', { value: { writeText: (t: string) => { copied.push(t); return Promise.resolve() } }, configurable: true })
    const w = mount(SiteCredentials, { props: { credentials }, global: { stubs: uiStubs } })
    expect((w.find('#site-cred-password').element as HTMLInputElement).value).toBe('s3cret-pw')
    expect(w.text()).toContain('only once')
    await w.find('[data-testid="copy-password"]').trigger('click')
    await Promise.resolve()
    expect(copied).toEqual(['s3cret-pw'])
    expect(w.text()).toContain('Password copied')
  })
})
