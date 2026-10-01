import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import TokenForm from '../app/components/token/TokenForm.vue'
import TokenSecret from '../app/components/token/TokenSecret.vue'
import TokenTable from '../app/components/token/TokenTable.vue'
import { ApiError } from '../app/utils/api-error'
import type { ApiToken } from '../app/utils/tokens'
import { uiStubs } from './stubs'

const global = { stubs: uiStubs }

afterEach(() => {
  vi.unstubAllGlobals()
})

const token = (over: Partial<ApiToken> = {}): ApiToken => ({
  id: 't1',
  name: 'CI deploys',
  scope: 'full',
  prefix: 'pft_3kq9',
  created_at: '2026-09-01T00:00:00Z',
  expires_at: null,
  last_used_at: null,
  ...over,
})

describe('TokenSecret', () => {
  it('shows the token read-only and copies it to the clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })

    const wrapper = mount(TokenSecret, { props: { token: 'pft_secret_value' }, global })
    const input = wrapper.find('input')
    expect(input.element.value).toBe('pft_secret_value')
    expect(input.attributes('readonly')).toBeDefined()
    expect(wrapper.text()).toContain('shown only once')

    await wrapper.find('[data-testid=copy]').trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledWith('pft_secret_value')
    expect(wrapper.find('[role=status]').text()).toContain('Copied')
  })

  it('falls back to selecting the text when the clipboard is unavailable', async () => {
    vi.stubGlobal('navigator', {})
    const wrapper = mount(TokenSecret, { props: { token: 'pft_x' }, global })

    await wrapper.find('[data-testid=copy]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[role=status]').text()).toContain('Ctrl+C')
  })

  it('does not write the token to storage', () => {
    const set = vi.spyOn(Storage.prototype, 'setItem')
    mount(TokenSecret, { props: { token: 'pft_never_stored' }, global })
    expect(set).not.toHaveBeenCalled()
    set.mockRestore()
  })
})

describe('TokenTable', () => {
  it('lists name, prefix (never a secret), scope, status and last use', () => {
    const wrapper = mount(TokenTable, {
      props: { tokens: [token(), token({ id: 't2', name: 'Old', scope: 'read-only', expires_at: '2020-01-01T00:00:00Z', last_used_at: '2026-09-02T00:00:00Z' })], caption: 'Tokens' },
      global,
    })
    const text = wrapper.text()
    expect(text).toContain('CI deploys')
    expect(text).toContain('pft_3kq9...')
    expect(text).toContain('Read-only')
    expect(text).toContain('Expired')
    expect(text).toContain('Never used')
  })

  it('emits revoke with the row token and labels the button for screen readers', async () => {
    const t = token()
    const wrapper = mount(TokenTable, { props: { tokens: [t], caption: 'Tokens' }, global })
    const button = wrapper.find('[data-testid=revoke]')
    expect(button.attributes('aria-label')).toBe('Revoke token CI deploys')

    await button.trigger('click')

    expect(wrapper.emitted('revoke')).toEqual([[t]])
  })
})

describe('TokenForm', () => {
  it('blocks an empty name and does not emit', async () => {
    const wrapper = mount(TokenForm, { global })
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('submit')).toBeUndefined()
    expect(wrapper.find('[role=alert]').text()).toContain('name')
  })

  it('emits the request body with the chosen scope', async () => {
    const wrapper = mount(TokenForm, { global })
    await wrapper.find('input:not([type=radio])').setValue('Reports')
    await wrapper.findAll('input[type=radio]')[1]!.trigger('change')
    await wrapper.find('select').setValue('never')
    await wrapper.find('form').trigger('submit')

    expect(wrapper.emitted('submit')![0]).toEqual([{ name: 'Reports', scope: 'read-only', expires_at: null }])
  })

  it('shows server field errors next to the field', () => {
    const error = new ApiError({
      type: 'about:blank', title: 'Unprocessable Content', status: 422,
      errors: [{ field: '/name', detail: 'is too long' }],
    })
    const wrapper = mount(TokenForm, { props: { error }, global })
    expect(wrapper.text()).toContain('is too long')
  })
})
