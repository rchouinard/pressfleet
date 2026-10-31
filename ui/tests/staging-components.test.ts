import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import JobProgressView from '../app/components/job/JobProgressView.vue'
import JobTable from '../app/components/job/JobTable.vue'
import SiteSettingsPanel from '../app/components/site/SiteSettingsPanel.vue'
import SiteStagingBanner from '../app/components/site/SiteStagingBanner.vue'
import SiteStagingFormModal from '../app/components/site/SiteStagingFormModal.vue'
import SiteStagingTable from '../app/components/site/SiteStagingTable.vue'
import SiteTable from '../app/components/site/SiteTable.vue'
import { ApiError } from '../app/utils/api-error'
import { waitsFor } from '../app/utils/job'
import type { StagingFormValues } from '../app/utils/staging'
import { job } from './job-fixtures'
import { site, stagingSite } from './site-fixtures'
import { uiStubs } from './stubs'

const NuxtLink = defineComponent({ props: ['to'], template: '<a :href="to"><slot /></a>' })
const UModal = defineComponent({
  props: ['open'],
  setup(props: { open: boolean }, { slots }) {
    return () => (props.open ? h('div', { role: 'dialog' }, [slots.body?.(), slots.footer?.()]) : null)
  },
})
const stubs = { ...uiStubs, NuxtLink, UModal }

describe('SiteTable with staging sites', () => {
  const mountTable = (sites: ReturnType<typeof site>[], productionNames: Record<string, string> = {}) =>
    mount(SiteTable, { props: { sites, groupNames: { g1: 'Acme Inc' }, productionNames }, global: { stubs } })

  it('badges staging sites and not production sites', () => {
    const w = mountTable([site({ id: 'p1' }), stagingSite('s1', 'p1')])
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0]!.find('[data-testid="staging-badge"]').exists()).toBe(false)
    expect(rows[1]!.find('[data-testid="staging-badge"]').text()).toContain('Staging')
  })

  it('lists a staging site under its production site, however the API ordered them', () => {
    const w = mountTable([stagingSite('s1', 'p1'), site({ id: 'p1', name: 'Production' })])
    expect(w.findAll('tbody tr').map(r => r.attributes('data-environment'))).toEqual(['production', 'staging'])
  })

  it('keeps a staging site whose production site is not on the page, and names that site', () => {
    const w = mountTable([stagingSite('s1', 'p-elsewhere')], { 'p-elsewhere': 'Elsewhere Inc' })
    expect(w.findAll('tbody tr')).toHaveLength(1)
    const of = w.find('[data-testid="staging-of"]')
    expect(of.text()).toContain('Elsewhere Inc')
    expect(of.find('a').attributes('href')).toBe('/sites/p-elsewhere')
  })

  it('falls back to a short ID while the production site\'s name is unknown', () => {
    const w = mountTable([stagingSite('s1', '0123456789abcdef')])
    expect(w.find('[data-testid="staging-of"]').text()).toContain('01234567')
  })
})

describe('SiteStagingTable', () => {
  it('shows status, preview URL, last refresh and last push', () => {
    const s = stagingSite('s1', 'p1', {
      url: 'https://acme-staging.preview.example',
      preview_domain: 'acme-staging.preview.example',
      staging: { production_site_id: 'p1', last_refreshed_at: '2026-10-01T10:00:00Z', last_snapshot_id: 'x', last_pushed_at: null, outbound_mail: false },
    })
    const w = mount(SiteStagingTable, { props: { sites: [s], canDelete: false }, global: { stubs } })
    expect(w.text()).toContain('Running')
    expect(w.find('a[href="https://acme-staging.preview.example"]').text()).toBe('acme-staging.preview.example')
    expect(w.text()).toContain('Never')
    expect(w.find('button').exists()).toBe(false)
  })

  it('shows why a failed clone failed, links to it, and lets it be deleted', async () => {
    const failed = stagingSite('s1', 'p1', { status: 'failed', status_message: 'restore: no space left on device' })
    const w = mount(SiteStagingTable, { props: { sites: [failed], canDelete: true }, global: { stubs } })
    expect(w.find('[data-testid="staging-error"]').text()).toContain('no space left on device')
    expect(w.text()).toContain('Failed')
    expect(w.find('a[href="/sites/s1"]').exists()).toBe(true)
    await w.find('button').trigger('click')
    expect(w.emitted('delete')![0]).toEqual([failed])
  })

  it('offers no delete for a site that is already being deleted', () => {
    const w = mount(SiteStagingTable, { props: { sites: [stagingSite('s1', 'p1', { status: 'deleting' })], canDelete: true }, global: { stubs } })
    expect(w.find('button').exists()).toBe(false)
  })
})

describe('SiteStagingBanner', () => {
  it('names the production site and links to it', () => {
    const s = stagingSite('s1', 'p1')
    const w = mount(SiteStagingBanner, { props: { site: s, productionName: 'Acme Inc site' }, global: { stubs } })
    const link = w.find('[data-testid="production-link"]')
    expect(link.text()).toBe('Acme Inc site')
    expect(link.attributes('href')).toBe('/sites/p1')
  })

  it('says when the copy was made and that nothing was pushed', () => {
    const s = stagingSite('s1', 'p1', {
      staging: { production_site_id: 'p1', last_refreshed_at: '2026-10-01T10:00:00Z', last_snapshot_id: 'x', last_pushed_at: null, outbound_mail: false },
    })
    const w = mount(SiteStagingBanner, { props: { site: s }, global: { stubs } })
    expect(w.find('[data-testid="staging-history"]').text()).toContain('Never pushed')
    expect(w.text()).toContain('its production site')
  })

  it('reports a copy that failed or is still running', () => {
    const failed = mount(SiteStagingBanner, { props: { site: stagingSite('s1', 'p1', { status: 'failed' }) }, global: { stubs } })
    expect(failed.text()).toContain('did not finish')
    const running = mount(SiteStagingBanner, { props: { site: stagingSite('s1', 'p1', { status: 'provisioning' }) }, global: { stubs } })
    expect(running.text()).toContain('has not finished yet')
  })

  it('leaves a slot for the refresh and push buttons', () => {
    const w = mount(SiteStagingBanner, {
      props: { site: stagingSite('s1', 'p1') },
      slots: { actions: '<button data-testid="later">Refresh</button>' },
      global: { stubs },
    })
    expect(w.find('[data-testid="later"]').exists()).toBe(true)
  })
})

describe('SiteStagingFormModal', () => {
  const setup = (props: Record<string, unknown> = {}) => {
    const submit = vi.fn((_: StagingFormValues) => Promise.resolve())
    const wrapper = mount(SiteStagingFormModal, {
      props: { open: true, siteName: 'Acme', backups: [{ label: 'Oct 1 - Manual', value: 'snap1' }], submit, ...props } as never,
      global: { stubs, components: { ProblemAlert } },
    })
    return { wrapper, submit }
  }

  it('says in one line that the copy carries production\'s users and data', () => {
    const { wrapper } = setup()
    expect(wrapper.find('[data-testid="staging-data-note"]').text()).toBe('The copy carries production\'s users and data.')
  })

  it('submits a fresh copy with nothing filled in', async () => {
    const { wrapper, submit } = setup()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).toHaveBeenCalledTimes(1)
    expect(submit.mock.calls[0]![0]).toMatchObject({ name: '', source: 'fresh', snapshot_id: '' })
    expect(wrapper.emitted('update:open')![0]).toEqual([false])
  })

  it('requires a backup once "an existing backup" is chosen, then submits it', async () => {
    const { wrapper, submit } = setup()
    await wrapper.findAll('input[type="radio"]')[1]!.setValue(true)
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('Choose a backup.')
    await wrapper.find('select').setValue('snap1')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit.mock.calls[0]![0]).toMatchObject({ source: 'backup', snapshot_id: 'snap1' })
  })

  it('explains an empty backup list and shows a failed one', async () => {
    const { wrapper } = setup({ backups: [], backupsError: new ApiError({ type: 'about:blank', title: 'Boom', status: 500 }) })
    await wrapper.findAll('input[type="radio"]')[1]!.setValue(true)
    expect(wrapper.text()).toContain('Boom')
    expect(wrapper.find('[data-testid="no-backups"]').exists()).toBe(false)
    const empty = setup({ backups: [] })
    await empty.wrapper.findAll('input[type="radio"]')[1]!.setValue(true)
    expect(empty.wrapper.find('[data-testid="no-backups"]').exists()).toBe(true)
  })

  it('keeps the dialog open and shows the server\'s answer on failure', async () => {
    const error = new ApiError({ type: 'about:blank', title: 'Conflict', status: 409, detail: 'The site is busy.' })
    const submit = vi.fn(() => Promise.reject(error))
    const { wrapper } = setup({ submit })
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('The site is busy.')
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })

  it('shows a 422 on the resource field it names', async () => {
    const error = new ApiError({
      type: 'about:blank', title: 'Invalid', status: 422,
      errors: [{ field: '/resources/memory_limit_mb', detail: 'Too small.' }],
    })
    const { wrapper } = setup({ submit: vi.fn(() => Promise.reject(error)) })
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.find('details').text()).toContain('Too small.')
  })
})

describe('outbound email in SiteSettingsPanel', () => {
  const limits = { cpu: '1.0', memory_reservation_mb: 128, memory_limit_mb: 512 }
  const panel = (siteOver: Record<string, unknown>, canUpdate = true) => {
    const update = vi.fn(async () => {})
    const wrapper = mount(SiteSettingsPanel, {
      props: {
        site: {
          status: 'running', php_version: '8.4', resources: limits, maintenance: false,
          preview_protection: { enabled: true, username: 'preview' }, preview_domain: 'a.preview.example', ...siteOver,
        },
        canUpdate, canMaintain: true, canPurge: true, phpVersions: [],
        update, setMaintenance: vi.fn(), purgeCache: vi.fn(), regeneratePassword: vi.fn(),
      } as never,
      global: { stubs: { ...uiStubs, ProblemAlert } },
    })
    return { wrapper, update }
  }
  const staging = (outbound_mail: boolean) => ({
    staging: { production_site_id: 'p1', last_refreshed_at: null, last_snapshot_id: null, last_pushed_at: null, outbound_mail },
  })

  it('has no email switch on a production site', () => {
    const { wrapper } = panel({})
    expect(wrapper.find('[data-testid="toggle-mail"]').exists()).toBe(false)
  })

  it('turns email on for a staging site, as an update job', async () => {
    const { wrapper, update } = panel(staging(false))
    expect(wrapper.find('[data-testid="mail-state"]').text()).toContain('Off')
    await wrapper.find('[data-testid="toggle-mail"]').trigger('click')
    await flushPromises()
    expect(update).toHaveBeenCalledWith({ outbound_mail: true })
  })

  it('turns it off again', async () => {
    const { wrapper, update } = panel(staging(true))
    expect(wrapper.find('[data-testid="mail-state"]').text()).toContain('On')
    await wrapper.find('[data-testid="toggle-mail"]').trigger('click')
    await flushPromises()
    expect(update).toHaveBeenCalledWith({ outbound_mail: false })
  })

  it('shows the state but no switch to a viewer, and none while the site is busy', () => {
    expect(panel(staging(false), false).wrapper.find('[data-testid="toggle-mail"]').exists()).toBe(false)
    expect(panel({ ...staging(false), status: 'updating' }).wrapper.find('[data-testid="toggle-mail"]').exists()).toBe(false)
    expect(panel(staging(false), false).wrapper.find('[data-testid="mail-state"]').exists()).toBe(true)
  })
})

describe('what a job waits for', () => {
  it('names the job a queued job waits for, and the job a started one ran after', () => {
    expect(waitsFor(job({ state: 'queued' }))).toBeUndefined()
    expect(waitsFor(job({ state: 'queued', after_job_id: 'J0' }))).toEqual({ jobId: 'J0', label: 'Waiting for job' })
    expect(waitsFor(job({ state: 'running', after_job_id: 'J0' }))).toEqual({ jobId: 'J0', label: 'Ran after job' })
    expect(waitsFor(job({ state: 'queued', after_job_id: null }))).toBeUndefined()
  })

  it('shows it on the job page', () => {
    const w = mount(JobProgressView, {
      props: { job: job({ type: 'site.clone', state: 'queued', started_at: null, after_job_id: '01J000000000000000000BACKUP' }) },
      global: { stubs: { UIcon: { template: '<i />' }, UBadge: { template: '<span><slot /></span>' }, UButton: { template: '<button />' } } },
    })
    expect(w.text()).toContain('Create staging site')
    expect(w.find('[data-testid="waits-for"]').text()).toContain('Waiting for job')
    expect(w.find('[data-testid="waits-for"]').text()).toContain('01J000000000000000000BACKUP')
  })

  it('shows it in job lists with a link', () => {
    const w = mount(JobTable, {
      props: { jobs: [job({ state: 'queued', after_job_id: '01J000000000000000000BACKUP' }), job({ id: 'J2' })], caption: 'Jobs' },
      global: { stubs: { NuxtLink, UIcon: { template: '<i />' }, UBadge: { template: '<span><slot /></span>' } } },
    })
    const waits = w.findAll('[data-testid="waits-for"]')
    expect(waits).toHaveLength(1)
    expect(waits[0]!.find('a').attributes('href')).toBe('/jobs/01J000000000000000000BACKUP')
  })
})
