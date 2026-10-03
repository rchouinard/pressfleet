import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import SiteFormModal from '../app/components/site/SiteFormModal.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import { PHP_DEFAULT_CHOICE, phpCreateOptions, type PhpVersionInfo } from '../app/utils/php'
import { createSitePayload, type SiteFormValues } from '../app/utils/site'
import { uiStubs } from './stubs'

// What GET /api/v1/catalog/php-versions answers on 2026-10-02.
const versions: PhpVersionInfo[] = [
  { version: '8.5', status: 'active', active_support_ends: '2027-12-31', security_support_ends: '2029-12-31' },
  { version: '8.4', status: 'active', active_support_ends: '2026-12-31', security_support_ends: '2028-12-31' },
  { version: '8.3', status: 'security-only', active_support_ends: '2025-12-31', security_support_ends: '2027-12-31' },
  { version: '8.2', status: 'eol-soon', active_support_ends: '2024-12-31', security_support_ends: '2026-12-31' },
  { version: '8.1', status: 'eol', active_support_ends: '2023-12-31', security_support_ends: '2025-12-31' },
]

describe('phpCreateOptions', () => {
  it('offers the platform default first, then the versions not past end of life', () => {
    const opts = phpCreateOptions(versions, '8.4')
    expect(opts.map(o => o.value)).toEqual([PHP_DEFAULT_CHOICE, '8.5', '8.4', '8.3', '8.2'])
    expect(opts[0]!.label).toBe('Platform default (PHP 8.4, supported)')
    expect(opts.find(o => o.value === '8.3')?.label).toContain('security fixes only')
    expect(opts.find(o => o.value === '8.2')?.label).toContain('end of life soon')
  })

  it('offers only the platform default without a catalog', () => {
    expect(phpCreateOptions()).toEqual([{ value: PHP_DEFAULT_CHOICE, label: 'Platform default' }])
    expect(phpCreateOptions([], undefined).map(o => o.value)).toEqual([PHP_DEFAULT_CHOICE])
  })
})

const UModal = defineComponent({
  props: ['open', 'title', 'description', 'dismissible'],
  setup(props: { open: boolean }, { slots }) {
    return () => (props.open ? h('div', { role: 'dialog' }, [slots.body?.(), slots.footer?.()]) : null)
  },
})
const global = { stubs: { ...uiStubs, UModal }, components: { ProblemAlert } }
const groups = [{ label: 'Acme', value: 'g1' }]

function setup(props: { phpVersions?: PhpVersionInfo[], phpDefault?: string, phpError?: ApiError } = {}) {
  const submit = vi.fn((_: SiteFormValues) => Promise.resolve())
  const wrapper = mount(SiteFormModal, {
    props: { open: true, groups, defaultGroupId: 'g1', submit, ...props },
    global,
  })
  return { wrapper, submit }
}

async function fillAndSubmit(wrapper: ReturnType<typeof setup>['wrapper']) {
  const inputs = wrapper.findAll('input')
  await inputs[0]!.setValue('Acme blog')
  await inputs[1]!.setValue('ed@acme.example')
  await wrapper.find('form').trigger('submit')
  await flushPromises()
}

describe('SiteFormModal PHP version', () => {
  it('preselects the platform default and lists no end-of-life versions', () => {
    const { wrapper } = setup({ phpVersions: versions, phpDefault: '8.4' })
    const select = wrapper.find('[data-testid="php-select"]')
    expect((select.element as HTMLSelectElement).value).toBe(PHP_DEFAULT_CHOICE)
    const values = select.findAll('option').map(o => o.attributes('value'))
    expect(values).toEqual([PHP_DEFAULT_CHOICE, '8.5', '8.4', '8.3', '8.2'])
    expect(wrapper.text()).not.toContain('8.1')
    expect(wrapper.find('[data-testid="php-catalog-error"]').exists()).toBe(false)
  })

  it('sends no version when the default stays selected, as a blank field did', async () => {
    const { wrapper, submit } = setup({ phpVersions: versions, phpDefault: '8.4' })
    await fillAndSubmit(wrapper)
    expect(submit).toHaveBeenCalledOnce()
    const values = submit.mock.calls[0]![0]
    expect(values.php_version).toBe('')
    expect(createSitePayload(values)).not.toHaveProperty('php_version')
  })

  it('sends the version picked', async () => {
    const { wrapper, submit } = setup({ phpVersions: versions, phpDefault: '8.4' })
    await wrapper.find('[data-testid="php-select"]').setValue('8.5')
    await fillAndSubmit(wrapper)
    expect(createSitePayload(submit.mock.calls[0]![0])).toMatchObject({ php_version: '8.5' })
  })

  it('warns when the chosen version is near end of life', async () => {
    const { wrapper } = setup({ phpVersions: versions, phpDefault: '8.4' })
    expect(wrapper.find('[data-testid="php-chosen-warning"]').exists()).toBe(false)
    await wrapper.find('[data-testid="php-select"]').setValue('8.2')
    expect(wrapper.find('[data-testid="php-chosen-warning"]').text()).toContain('2026-12-31')
  })

  it('falls back to the platform default only, with the error, when the catalog fails', async () => {
    const phpError = new ApiError({ type: 'about:blank', status: 503, title: 'Service Unavailable', detail: 'catalog down', request_id: 'req-1' })
    const { wrapper, submit } = setup({ phpError })
    const options = wrapper.find('[data-testid="php-select"]').findAll('option')
    expect(options.map(o => o.attributes('value'))).toEqual([PHP_DEFAULT_CHOICE])
    const error = wrapper.find('[data-testid="php-catalog-error"]')
    expect(error.text()).toContain("couldn't be loaded")
    expect(error.text()).toContain('catalog down')
    expect(error.text()).toContain('req-1')
    await fillAndSubmit(wrapper)
    expect(createSitePayload(submit.mock.calls[0]![0])).not.toHaveProperty('php_version')
  })
})
