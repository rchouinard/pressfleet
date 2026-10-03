import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import SiteAdminLogin from '../app/components/site/SiteAdminLogin.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import {
  CHOOSE_USER_PROBLEM_TYPE, adminLoginBlockedReason, administratorLabel, closeLoginTab, isChooseUserError,
  openLoginTab, previewCredentialsNote, submitLoginForm, type LoginTab,
} from '../app/utils/admin-login'
import { uiStubs } from './stubs'

const problem = (status: number, type = 'about:blank') => new ApiError({ type, title: 'Conflict', status })

const running = { status: 'running', maintenance: false, primary_domain: 'example.com', preview_protection: { enabled: false, username: null } } as const

describe('isChooseUserError', () => {
  it('matches only the 409 of the choose-a-user type', () => {
    expect(isChooseUserError(problem(409, CHOOSE_USER_PROBLEM_TYPE))).toBe(true)
    expect(isChooseUserError(problem(409))).toBe(false)
    expect(isChooseUserError(problem(422, CHOOSE_USER_PROBLEM_TYPE))).toBe(false)
  })
})

describe('adminLoginBlockedReason', () => {
  it('allows a running site and says why otherwise', () => {
    expect(adminLoginBlockedReason({ status: 'running', maintenance: false })).toBeUndefined()
    expect(adminLoginBlockedReason({ status: 'stopped', maintenance: false })).toMatch(/Start the site/)
    expect(adminLoginBlockedReason({ status: 'provisioning', maintenance: false })).toMatch(/Start the site/)
    expect(adminLoginBlockedReason({ status: 'running', maintenance: true })).toMatch(/maintenance/)
  })
})

describe('previewCredentialsNote', () => {
  it('applies only to a protected site that is not live', () => {
    const on = { enabled: true, username: 'u' }
    expect(previewCredentialsNote({ primary_domain: null, preview_protection: on })).toMatch(/preview credentials/)
    expect(previewCredentialsNote({ primary_domain: 'example.com', preview_protection: on })).toBeUndefined()
    expect(previewCredentialsNote({ primary_domain: null, preview_protection: { enabled: false, username: null } })).toBeUndefined()
  })
})

describe('administratorLabel', () => {
  it('adds name and email only when they say something new', () => {
    expect(administratorLabel({ login: 'admin', display_name: 'admin', email: '' })).toBe('admin')
    expect(administratorLabel({ login: 'ann', display_name: 'Ann A', email: 'ann@example.com' })).toBe('ann (Ann A, ann@example.com)')
  })
})

function fakeTab(): LoginTab {
  return { closed: false, name: 'tab-1', close: vi.fn(), opener: {}, document: document.implementation.createHTMLDocument('x') }
}

describe('openLoginTab / closeLoginTab', () => {
  it('opens a named tab without a URL, clears opener and returns null when blocked', () => {
    const tab = fakeTab()
    const open = vi.fn().mockReturnValue(tab)
    expect(openLoginTab({ open })).toBe(tab)
    expect(open.mock.calls[0]![0]).toBe('')
    expect(tab.opener).toBeNull()
    expect(openLoginTab({ open: () => null })).toBeNull()
  })

  it('closes an open tab only', () => {
    const tab = fakeTab()
    closeLoginTab(tab)
    expect(tab.close).toHaveBeenCalledTimes(1)
    const gone = { ...fakeTab(), closed: true }
    closeLoginTab(gone)
    expect(gone.close).not.toHaveBeenCalled()
    closeLoginTab(null)
  })
})

describe('submitLoginForm', () => {
  it('POSTs the token in a hidden field to the tab, not in the URL, and cleans up', () => {
    let seen: { method: string, action: string, target: string, token: string, hidden: boolean } | undefined
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(function (this: HTMLFormElement) {
      seen = {
        method: this.method.toLowerCase(), action: this.action, target: this.target,
        token: (this.elements.namedItem('token') as HTMLInputElement).value, hidden: this.hidden,
      }
    })
    submitLoginForm(document, { name: 'tab-1' }, { url: 'https://site.example/wp-login.php?action=pressfleet-login', token: 'secret-token' })
    submit.mockRestore()
    expect(seen).toEqual({
      method: 'post', action: 'https://site.example/wp-login.php?action=pressfleet-login', target: 'tab-1', token: 'secret-token', hidden: true,
    })
    expect(seen!.action).not.toContain('secret-token')
    expect(document.querySelector('form')).toBeNull()
  })
})

// UDropdownMenu stand-in: renders the trigger and the items as buttons.
const UDropdownMenu = defineComponent({
  props: ['items', 'disabled'],
  setup(props: { items: { label: string, onSelect: () => void }[][] }, { slots }) {
    return () => h('div', [
      slots.default?.(),
      ...props.items.flat().map(i => h('button', { 'data-testid': 'menu-item', onClick: i.onSelect }, i.label)),
    ])
  },
})
const UModal = defineComponent({
  props: ['open', 'title'],
  setup(props: { open: boolean }, { slots }) {
    return () => (props.open ? h('div', { role: 'dialog' }, slots.body?.()) : null)
  },
})
const global = { stubs: { ...uiStubs, UDropdownMenu, UModal }, components: { ProblemAlert } }

const login = { url: 'https://site.example/wp-login.php', token: 'tok', user: 'admin', expires_at: '2026-10-02T00:01:00Z' }

function setup(over: { site?: object, createLogin?: (u?: string) => Promise<typeof login>, administrators?: object[] } = {}) {
  const tab = fakeTab()
  const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window)
  const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {})
  const createLogin = vi.fn(over.createLogin ?? (() => Promise.resolve(login)))
  const listAdministrators = vi.fn(() => Promise.resolve((over.administrators ?? []) as never))
  const wrapper = mount(SiteAdminLogin, {
    props: { site: { ...running, ...over.site } as never, createLogin, listAdministrators },
    global,
  })
  return { wrapper, tab, open, submit, createLogin, listAdministrators }
}

describe('SiteAdminLogin', () => {
  it('opens the tab in the click itself, then submits the token to it', async () => {
    const { wrapper, tab, open, submit, createLogin } = setup()
    const order: string[] = []
    open.mockImplementation(() => {
      order.push('open')
      return tab as unknown as Window
    })
    createLogin.mockImplementation(() => {
      order.push('api')
      return Promise.resolve(login)
    })
    await wrapper.find('[data-testid=admin-login]').trigger('click')
    // The tab is open before the API call even started.
    expect(order).toEqual(['open', 'api'])
    await flushPromises()
    expect(createLogin).toHaveBeenCalledWith(undefined)
    expect(submit).toHaveBeenCalledTimes(1)
    expect(tab.close).not.toHaveBeenCalled()
    vi.restoreAllMocks()
  })

  it('closes the tab and shows the error when the call fails', async () => {
    const { wrapper, tab, submit } = setup({ createLogin: () => Promise.reject(problem(409)) })
    await wrapper.find('[data-testid=admin-login]').trigger('click')
    await flushPromises()
    expect(tab.close).toHaveBeenCalled()
    expect(submit).not.toHaveBeenCalled()
    expect(wrapper.find('[role=alert]').text()).toContain('Conflict')
    vi.restoreAllMocks()
  })

  it('does not call the API when a popup blocker refuses the tab', async () => {
    const { wrapper, open, createLogin } = setup()
    open.mockReturnValue(null)
    await wrapper.find('[data-testid=admin-login]').trigger('click')
    await flushPromises()
    expect(createLogin).not.toHaveBeenCalled()
    expect(wrapper.find('[role=alert]').text()).toContain('blocked')
    vi.restoreAllMocks()
  })

  it('offers a picker on the choose-a-user 409 and retries with the chosen login', async () => {
    const admins = [
      { login: 'ann', display_name: 'Ann', email: 'ann@example.com', default: false },
      { login: 'bob', display_name: 'bob', email: '', default: false },
    ]
    let calls = 0
    const { wrapper, tab, submit, createLogin, listAdministrators } = setup({
      administrators: admins,
      createLogin: (u) => {
        calls++
        return u === undefined ? Promise.reject(problem(409, CHOOSE_USER_PROBLEM_TYPE)) : Promise.resolve({ ...login, user: u })
      },
    })
    await wrapper.find('[data-testid=admin-login]').trigger('click')
    await flushPromises()
    expect(tab.close).toHaveBeenCalledTimes(1)
    expect(listAdministrators).toHaveBeenCalledTimes(1)
    const options = wrapper.findAll('[data-testid=administrator]')
    expect(options.map(o => o.text())).toEqual(['ann (Ann, ann@example.com)', 'bob'])

    await options[1]!.trigger('click')
    await flushPromises()
    expect(createLogin).toHaveBeenLastCalledWith('bob')
    expect(calls).toBe(2)
    expect(submit).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[role=dialog]').exists()).toBe(false)
    vi.restoreAllMocks()
  })

  it('"Log in as..." lists administrators even when a default exists', async () => {
    const { wrapper, createLogin, listAdministrators } = setup({
      administrators: [{ login: 'admin', display_name: 'admin', email: '', default: true }],
    })
    await wrapper.find('[data-testid=menu-item]').trigger('click')
    await flushPromises()
    expect(listAdministrators).toHaveBeenCalled()
    expect(createLogin).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid=administrator]').text()).toContain('Default')
    vi.restoreAllMocks()
  })

  it('is disabled with the reason while the site is not running or in maintenance', () => {
    for (const [site, text] of [
      [{ status: 'stopped' }, 'Start the site'],
      [{ maintenance: true }, 'maintenance'],
    ] as const) {
      const { wrapper } = setup({ site })
      expect(wrapper.find('[data-testid=admin-login]').attributes('disabled')).toBeDefined()
      expect(wrapper.find('#admin-login-hint').text()).toContain(text)
      vi.restoreAllMocks()
    }
  })

  it('says the browser will ask for the preview credentials', () => {
    const { wrapper } = setup({ site: { primary_domain: null, preview_protection: { enabled: true, username: 'u' } } })
    expect(wrapper.find('#admin-login-hint').text()).toContain('preview credentials')
    vi.restoreAllMocks()
  })
})
