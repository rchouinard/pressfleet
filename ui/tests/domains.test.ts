import { describe, expect, it } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import SiteDomainsPanel from '../app/components/site/SiteDomainsPanel.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import {
  aliasesPayload, canEditAliases, canGoLive, dnsFailures, DNS_CHECK_PROBLEM_TYPE, domainsBlockedReason,
  goLivePayload, isDnsCheckError, normalizeDomain, parseAliases, validateDomains,
} from '../app/utils/domains'
import { can } from '../app/utils/permissions'
import { uiStubs } from './stubs'

const dnsError = () => new ApiError({
  type: DNS_CHECK_PROBLEM_TYPE,
  title: 'DNS check failed',
  status: 422,
  errors: [
    { field: '/primary_domain', detail: 'resolves to 203.0.113.9; expected 198.51.100.1' },
    { field: '/aliases/1', detail: 'does not resolve' },
  ],
})

describe('domain helpers', () => {
  it('normalizes and parses', () => {
    expect(normalizeDomain(' WWW.Example.com. ')).toBe('www.example.com')
    expect(parseAliases('A.example.com, b.example.com\n\na.example.com')).toEqual(['a.example.com', 'b.example.com'])
  })

  it('validates domains', () => {
    expect(validateDomains('www.example.com', ['example.com'])).toEqual({})
    expect(validateDomains('', []).primary).toBeTruthy()
    expect(validateDomains('https://x.example.com', []).primary).toBeTruthy()
    expect(validateDomains('localhost', []).primary).toBeTruthy()
    expect(validateDomains('*.example.com', []).primary).toBeTruthy()
    expect(validateDomains('a.example.com', ['a.example.com']).aliases).toContain('primary')
    expect(validateDomains(undefined, ['a.example.com'], 'a.example.com').aliases).toContain('primary')
    expect(validateDomains('a.example.com', Array.from({ length: 21 }, (_, i) => `a${i}.example.com`)).aliases).toContain('at most 20')
  })

  it('builds payloads, always sending skip_dns_check', () => {
    expect(goLivePayload('WWW.example.com', ['a.example.com'], false)).toEqual({ primary_domain: 'www.example.com', aliases: ['a.example.com'], skip_dns_check: false })
    expect(goLivePayload('www.example.com', [], true).skip_dns_check).toBe(true)
    expect(aliasesPayload([], false)).toEqual({ aliases: [], skip_dns_check: false })
    expect(aliasesPayload(['a.example.com'], true)).toEqual({ aliases: ['a.example.com'], skip_dns_check: true })
  })

  it('gates by status and liveness', () => {
    expect(canGoLive({ status: 'running' })).toBe(true)
    expect(canGoLive({ status: 'stopped' })).toBe(false)
    expect(canEditAliases({ status: 'stopped', primary_domain: 'a.example.com' })).toBe(true)
    expect(canEditAliases({ status: 'running', primary_domain: null })).toBe(false)
    expect(canEditAliases({ status: 'updating', primary_domain: 'a.example.com' })).toBe(false)
    expect(domainsBlockedReason({ status: 'updating', primary_domain: null })).toBeTruthy()
    expect(domainsBlockedReason({ status: 'running', primary_domain: null })).toBeUndefined()
  })

  it('follows the role matrix: operators and admins manage domains, viewers do not', () => {
    expect(can('viewer', 'site.domains.manage')).toBe(false)
    expect(can('operator', 'site.domains.manage')).toBe(true)
  })

  it('maps DNS failures back to the domains sent', () => {
    const e = dnsError()
    expect(isDnsCheckError(e)).toBe(true)
    expect(dnsFailures(e, { primary: 'www.example.com', aliases: ['a.example.com', 'b.example.com'] })).toEqual([
      { domain: 'www.example.com', detail: 'resolves to 203.0.113.9; expected 198.51.100.1' },
      { domain: 'b.example.com', detail: 'does not resolve' },
    ])
  })

  it('names each domain once when the server leads the detail with it', () => {
    // The server's shape: every alias under /aliases, the domain first in the detail.
    const e = new ApiError({
      type: DNS_CHECK_PROBLEM_TYPE,
      title: 'DNS check failed',
      status: 422,
      errors: [
        { field: '/primary_domain', detail: 'www.example.com: it does not resolve (no such domain, or no A or AAAA record).' },
        { field: '/aliases', detail: 'b.example.com: it resolves to 203.0.113.9, not 198.51.100.1.' },
      ],
    })
    expect(dnsFailures(e, { primary: 'www.example.com', aliases: ['a.example.com', 'b.example.com'] })).toEqual([
      { domain: 'www.example.com', detail: 'it does not resolve (no such domain, or no A or AAAA record).' },
      { domain: 'b.example.com', detail: 'it resolves to 203.0.113.9, not 198.51.100.1.' },
    ])
    // A detail that names a domain that wasn't sent keeps the pointer.
    const other = new ApiError({ type: DNS_CHECK_PROBLEM_TYPE, title: 'DNS check failed', status: 422, errors: [{ field: '/aliases', detail: 'c.example.com: it does not resolve' }] })
    expect(dnsFailures(other, { aliases: ['a.example.com'] })).toEqual([{ domain: '/aliases', detail: 'c.example.com: it does not resolve' }])
  })
})

const UTextarea = {
  props: ['modelValue'],
  emits: ['update:modelValue'],
  template: '<textarea :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
}
const global = { stubs: { ...uiStubs, UTextarea, ProblemAlert } }
const preview = { status: 'running', primary_domain: null, aliases: [], preview_domain: 'acme.preview.test' } as const
const live = { status: 'running', primary_domain: 'www.example.com', aliases: ['example.com'], preview_domain: 'acme.preview.test' } as const

function panel(site: typeof preview | typeof live, over: Record<string, unknown> = {}) {
  const calls: unknown[] = []
  const wrapper = mount(SiteDomainsPanel, {
    props: {
      site,
      canManage: true,
      goLive: async (b: unknown) => { calls.push(['golive', b]) },
      setAliases: async (b: unknown) => { calls.push(['aliases', b]) },
      ...over,
    },
    global,
  })
  return { wrapper, calls }
}

describe('SiteDomainsPanel', () => {
  it('shows no actions to roles that cannot manage domains', () => {
    const { wrapper } = panel(preview, { canManage: false })
    expect(wrapper.find('[data-testid=open-golive]').exists()).toBe(false)
    expect(wrapper.text()).toContain('Not live yet')
  })

  it('offers go-live only, then submits the typed domains', async () => {
    const { wrapper, calls } = panel(preview)
    expect(wrapper.find('[data-testid=open-aliases]').exists()).toBe(false)
    await wrapper.find('[data-testid=open-golive]').trigger('click')
    const inputs = wrapper.findAll('input')
    await inputs[0]!.setValue('WWW.example.com')
    await wrapper.find('textarea').setValue('example.com\nshop.example.com')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(calls).toEqual([['golive', { primary_domain: 'www.example.com', aliases: ['example.com', 'shop.example.com'], skip_dns_check: false }]])
    expect(wrapper.find('form').exists()).toBe(false)
  })

  it('does not send an invalid domain', async () => {
    const { wrapper, calls } = panel(preview)
    await wrapper.find('[data-testid=open-golive]').trigger('click')
    await wrapper.findAll('input')[0]!.setValue('https://nope')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(calls).toEqual([])
    expect(wrapper.find('[role=alert]').exists()).toBe(true)
  })

  it('lists DNS check failures per domain and keeps the form for another try', async () => {
    let attempts = 0
    const { wrapper, calls } = panel(preview, {
      goLive: async (b: unknown) => {
        attempts++
        if (attempts === 1) throw dnsError()
        calls.push(b)
      },
    })
    await wrapper.find('[data-testid=open-golive]').trigger('click')
    await wrapper.findAll('input')[0]!.setValue('www.example.com')
    await wrapper.find('textarea').setValue('a.example.com\nb.example.com')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    const result = wrapper.find('[data-testid=dns-result]')
    expect(result.text()).toContain('www.example.com')
    expect(result.text()).toContain('b.example.com: does not resolve')
    expect(wrapper.find('[data-testid=submit]').text()).toBe('Check again')

    await wrapper.find('input[type=checkbox]').setValue(true)
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(calls).toEqual([{ primary_domain: 'www.example.com', aliases: ['a.example.com', 'b.example.com'], skip_dns_check: true }])
  })

  it('edits aliases of a live site, prefilled, and can clear them', async () => {
    const { wrapper, calls } = panel(live)
    await wrapper.find('[data-testid=open-aliases]').trigger('click')
    expect((wrapper.find('textarea').element as HTMLTextAreaElement).value).toBe('example.com')
    await wrapper.find('textarea').setValue('')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(calls).toEqual([['aliases', { aliases: [], skip_dns_check: false }]])
  })

  it('hides actions while the site is busy and says why', () => {
    const { wrapper } = panel({ ...live, status: 'updating' })
    expect(wrapper.find('[data-testid=open-golive]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=open-aliases]').exists()).toBe(false)
    expect(wrapper.text()).toContain('being updated')
  })
})
