import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import UserForm from '../app/components/user/UserForm.vue'
import UserTable from '../app/components/user/UserTable.vue'
import type { User } from '../app/utils/users'
import { uiStubs } from './stubs'

const global = { stubs: uiStubs }

const user = (over: Partial<User> = {}): User => ({
  id: 'u1',
  email: 'ann@example.com',
  role: 'viewer',
  disabled: false,
  created_at: '2026-09-01T00:00:00Z',
  updated_at: '2026-09-01T00:00:00Z',
  last_login_at: null,
  ...over,
})

describe('UserTable', () => {
  it('lists users with role and status, marks the signed-in one, and emits edit', async () => {
    const ann = user()
    const wrapper = mount(UserTable, {
      props: { users: [ann, user({ id: 'u2', email: 'bob@example.com', role: 'admin', disabled: true })], caption: 'Users', currentUserId: 'u2' },
      global,
    })
    const rows = wrapper.findAll('tbody tr')
    expect(rows[0]!.text()).toContain('Active')
    expect(rows[0]!.text()).toContain('Never')
    expect(rows[1]!.text()).toContain('Disabled')
    expect(rows[1]!.text()).toContain('You')
    expect(rows[0]!.text()).not.toContain('You')

    const edit = rows[0]!.find('[data-testid=edit]')
    expect(edit.attributes('aria-label')).toBe('Edit ann@example.com')
    await edit.trigger('click')
    expect(wrapper.emitted('edit')).toEqual([[ann]])
  })
})

describe('UserForm (create)', () => {
  it('validates before emitting', async () => {
    const wrapper = mount(UserForm, { global })
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('create')).toBeUndefined()
    expect(wrapper.findAll('[role=alert]').length).toBe(2)
  })

  it('emits the create body with the chosen role', async () => {
    const wrapper = mount(UserForm, { global })
    const inputs = wrapper.findAll('input')
    await inputs[0]!.setValue(' new@example.com ')
    await inputs[1]!.setValue('a long enough password')
    await wrapper.find('select').setValue('operator')
    await wrapper.find('form').trigger('submit')

    expect(wrapper.emitted('create')![0]).toEqual([{ email: 'new@example.com', password: 'a long enough password', role: 'operator' }])
  })

  it('toggles password visibility with an accessible control', async () => {
    const wrapper = mount(UserForm, { global })
    const toggle = wrapper.find('button[aria-label="Show password"]')
    expect(wrapper.findAll('input')[1]!.attributes('type')).toBe('password')
    await toggle.trigger('click')
    expect(wrapper.findAll('input')[1]!.attributes('type')).toBe('text')
    expect(wrapper.find('button[aria-label="Hide password"]').exists()).toBe(true)
  })
})

describe('UserForm (edit)', () => {
  it('emits only the changed fields', async () => {
    const wrapper = mount(UserForm, { props: { user: user() }, global })
    await wrapper.find('select').setValue('admin')
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('update')![0]).toEqual([{ role: 'admin' }])
  })

  it('says so and emits nothing when nothing changed', async () => {
    const wrapper = mount(UserForm, { props: { user: user() }, global })
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('update')).toBeUndefined()
    expect(wrapper.text()).toContain('no changes')
  })

  it('locks role and disabling for your own account', () => {
    const wrapper = mount(UserForm, { props: { user: user({ role: 'admin' }), self: true }, global })
    expect(wrapper.find('select').attributes('disabled')).toBeDefined()
    expect(wrapper.find('input[type=checkbox]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('button[type=submit]').attributes('disabled')).toBeDefined()
  })
})
