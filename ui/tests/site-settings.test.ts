import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SiteSettingsPanel from '../app/components/site/SiteSettingsPanel.vue'
import SitePreviewCredentials from '../app/components/site/SitePreviewCredentials.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import { phpOptions, phpStatus } from '../app/utils/php'
import {
  canUpdateCoreNow, coreApprove, coreDecisionText, coreFollowGroup, coreHold, hasOwnCorePolicy, phpPayload,
  previewProtectionPayload, resourceFormValues, resourcesPayload, settingsBlockedReason, validateResources,
  type SiteCore,
} from '../app/utils/site-settings'
import { can } from '../app/utils/permissions'
import { uiStubs } from './stubs'

const now = new Date('2026-10-02T12:00:00Z')
const limits = { cpu: '1.0', memory_reservation_mb: 128, memory_limit_mb: 512 }

describe('phpStatus', () => {
  it('classifies branches by date', () => {
    expect(phpStatus('8.5', now).status).toBe('active')
    expect(phpStatus('8.4', now).status).toBe('active')
    expect(phpStatus('8.3', now).status).toBe('security')
    expect(phpStatus('8.2', now).status).toBe('eol-soon')
    expect(phpStatus('8.1', now).status).toBe('eol')
    expect(phpStatus('9.9', now).status).toBe('unknown')
  })

  it('warns for end-of-life and soon, with text and an icon', () => {
    expect(phpStatus('8.1', now).warning).toContain('end of life')
    expect(phpStatus('8.2', now).warning).toContain('2026-12-31')
    expect(phpStatus('8.4', now).warning).toBeUndefined()
    for (const v of ['8.5', '8.2', '8.1']) {
      expect(phpStatus(v, now).icon).toMatch(/^i-lucide-/)
      expect(phpStatus(v, now).label).not.toBe('')
    }
  })

  it('counts the last day of support as supported', () => {
    expect(phpStatus('8.2', new Date('2026-12-31T20:00:00Z')).status).toBe('eol-soon')
    expect(phpStatus('8.2', new Date('2027-01-01T00:00:01Z')).status).toBe('eol')
  })

  it('offers no end-of-life versions except the current one', () => {
    expect(phpOptions('8.4', now).map(o => o.value)).toEqual(['8.5', '8.4', '8.3', '8.2'])
    expect(phpOptions('8.1', now).map(o => o.value)).toContain('8.1')
    expect(phpOptions('8.4', now).find(o => o.value === '8.4')?.label).toContain('current')
  })
})

describe('settings helpers', () => {
  it('explains blocked states', () => {
    expect(settingsBlockedReason({ status: 'running' })).toBeUndefined()
    expect(settingsBlockedReason({ status: 'stopped' })).toBeUndefined()
    for (const s of ['provisioning', 'updating', 'deleting', 'failed'] as const) expect(settingsBlockedReason({ status: s })).toBeTruthy()
  })

  it('validates resources like the spec', () => {
    expect(validateResources(resourceFormValues(limits))).toEqual({})
    expect(validateResources({ cpu: 'x', memory_reservation_mb: '128', memory_limit_mb: '512' }).cpu).toBeTruthy()
    expect(validateResources({ cpu: '0', memory_reservation_mb: '128', memory_limit_mb: '512' }).cpu).toBeTruthy()
    expect(validateResources({ cpu: '1', memory_reservation_mb: '16', memory_limit_mb: '512' }).memory_reservation_mb).toBeTruthy()
    expect(validateResources({ cpu: '1', memory_reservation_mb: '128', memory_limit_mb: '32' }).memory_limit_mb).toBeTruthy()
    expect(validateResources({ cpu: '1', memory_reservation_mb: '256', memory_limit_mb: '128' }).memory_limit_mb).toContain('reservation')
  })

  it('sends only what changed', () => {
    expect(resourcesPayload(limits, resourceFormValues(limits))).toBeUndefined()
    expect(resourcesPayload(limits, { cpu: '2', memory_reservation_mb: '128', memory_limit_mb: '1024' })).toEqual({ resources: { cpu: '2', memory_limit_mb: 1024 } })
    expect(phpPayload('8.3', '8.3')).toBeUndefined()
    expect(phpPayload('8.3', '8.4')).toEqual({ php_version: '8.4' })
    expect(previewProtectionPayload(true)).toEqual({ preview_protection: { enabled: true } })
  })

  it('follows the role matrix: operators change settings, purge and toggle maintenance; viewers do not', () => {
    for (const a of ['site.update', 'site.maintenance.set', 'site.cache.purge', 'core_update.approve'] as const) {
      expect(can('viewer', a)).toBe(false)
      expect(can('operator', a)).toBe(true)
    }
  })
})

const core = (over: Partial<SiteCore> = {}): SiteCore => ({
  version: '7.1.2', branch: '7.1', policy: { hold: false }, client_group_policy: { hold: false }, held: false,
  decided_by: 'default', warnings: [], ...over,
})

describe('core helpers', () => {
  it('builds policy bodies', () => {
    expect(coreHold()).toEqual({ hold: true })
    expect(coreApprove('7.2')).toEqual({ approved_branch: '7.2' })
    expect(coreFollowGroup()).toEqual({})
  })

  it('describes whose policy applies', () => {
    expect(coreDecisionText(core({ held: true, decided_by: 'client_group' }))).toContain('client group')
    expect(coreDecisionText(core({ approved_branch: '7.2', decided_by: 'site' }))).toContain('7.2')
    expect(coreDecisionText(core())).toContain('wait for approval')
  })

  it('gates update now and follow-the-group', () => {
    expect(canUpdateCoreNow({ status: 'running' }, core({ target: { version: '7.1.3', kind: 'minor' } }))).toBe(true)
    expect(canUpdateCoreNow({ status: 'stopped' }, core({ target: { version: '7.1.3', kind: 'minor' } }))).toBe(false)
    expect(canUpdateCoreNow({ status: 'running' }, core())).toBe(false)
    expect(hasOwnCorePolicy(core())).toBe(false)
    expect(hasOwnCorePolicy(core({ policy: { hold: true } }))).toBe(true)
    expect(hasOwnCorePolicy(core({ policy: { hold: false, approved_branch: '7.2' } }))).toBe(true)
  })
})

function panel(over: Record<string, unknown> = {}) {
  const fns = {
    update: vi.fn(async () => {}),
    setMaintenance: vi.fn(async () => {}),
    purgeCache: vi.fn(async () => {}),
    regeneratePassword: vi.fn(async () => {}),
  }
  const wrapper = mount(SiteSettingsPanel, {
    props: {
      site: {
        status: 'running', php_version: '8.4', resources: limits, maintenance: false,
        preview_protection: { enabled: false, username: null }, preview_domain: 'a.preview.example',
      },
      canUpdate: true, canMaintain: true, canPurge: true, ...fns, ...over,
    },
    global: { stubs: { ...uiStubs, ProblemAlert } },
  })
  return { wrapper, ...fns }
}

describe('SiteSettingsPanel', () => {
  it('shows an end-of-life warning for the current PHP version', () => {
    const { wrapper } = panel({ site: { status: 'running', php_version: '8.1', resources: limits, maintenance: false, preview_protection: { enabled: false, username: null }, preview_domain: 'a.example' } })
    expect(wrapper.find('[data-testid="php-warning"]').text()).toContain('end of life')
  })

  it('changes the PHP version only when it differs', async () => {
    const { wrapper, update } = panel()
    expect(wrapper.find('[data-testid="save-php"]').attributes('disabled')).toBeDefined()
    await wrapper.find('select').setValue('8.5')
    await wrapper.find('[data-testid="save-php"]').trigger('click')
    await flushPromises()
    expect(update).toHaveBeenCalledWith({ php_version: '8.5' })
  })

  it('validates limits before sending and sends only changes', async () => {
    const { wrapper, update } = panel()
    const inputs = wrapper.findAll('input')
    await inputs[2]!.setValue('16')
    await wrapper.find('form').trigger('submit')
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('at least 64')
    await inputs[2]!.setValue('1024')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(update).toHaveBeenCalledWith({ resources: { memory_limit_mb: 1024 } })
  })

  it('shows a server field error next to the PHP field', async () => {
    const err = new ApiError({ type: 'about:blank', title: 'Invalid', status: 422, errors: [{ field: '/php_version', detail: 'no images for 8.9' }] })
    const { wrapper } = panel({ update: vi.fn(async () => { throw err }) })
    await wrapper.find('select').setValue('8.3')
    await wrapper.find('[data-testid="save-php"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('no images for 8.9')
  })

  it('toggles maintenance and purges the cache', async () => {
    const { wrapper, setMaintenance, purgeCache } = panel()
    await wrapper.find('[data-testid="toggle-maintenance"]').trigger('click')
    await wrapper.find('[data-testid="purge-cache"]').trigger('click')
    await flushPromises()
    expect(setMaintenance).toHaveBeenCalledWith(true)
    expect(purgeCache).toHaveBeenCalled()
    expect(wrapper.text()).toContain('Cache purge queued')
  })

  it('turns preview protection on and asks before replacing the password', async () => {
    const { wrapper, update, regeneratePassword } = panel()
    await wrapper.find('[data-testid="toggle-preview"]').trigger('click')
    expect(update).toHaveBeenCalledWith({ preview_protection: { enabled: true } })
    await wrapper.find('[data-testid="regenerate-preview"]').trigger('click')
    expect(regeneratePassword).not.toHaveBeenCalled()
    await wrapper.find('[data-testid="confirm-regenerate"]').trigger('click')
    await flushPromises()
    expect(regeneratePassword).toHaveBeenCalledTimes(1)
  })

  it('hides controls the role lacks and explains blocked states', () => {
    const { wrapper } = panel({ canUpdate: false, canMaintain: false, canPurge: false })
    expect(wrapper.find('[data-testid="save-php"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="toggle-maintenance"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="purge-cache"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="toggle-preview"]').exists()).toBe(false)
    const b = panel({ site: { status: 'updating', php_version: '8.4', resources: limits, maintenance: false, preview_protection: { enabled: false, username: null }, preview_domain: 'a.example' } })
    expect(b.wrapper.find('[data-testid="blocked"]').exists()).toBe(true)
    expect(b.wrapper.find('[data-testid="save-php"]').exists()).toBe(false)
  })
})

describe('SitePreviewCredentials', () => {
  it('shows the one-time password with a warning', () => {
    const w = mount(SitePreviewCredentials, {
      props: { credentials: { username: 'acme', password: 's3cret-pass' }, previewDomain: 'acme.preview.example' },
      global: { stubs: uiStubs },
    })
    const values = w.findAll('input').map(i => (i.element as HTMLInputElement).value)
    expect(values).toEqual(['acme', 's3cret-pass'])
    expect(w.text()).toContain('shown only once')
    expect(w.text()).not.toContain('s3cret-pass')
  })
})
