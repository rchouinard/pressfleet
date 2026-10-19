import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import PasswordChangeForm from '../app/components/user/PasswordChangeForm.vue'
import UserForm from '../app/components/user/UserForm.vue'
import UserTable from '../app/components/user/UserTable.vue'
import { ApiError } from '../app/utils/api-error'
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
  display_name: '',
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
    expect(wrapper.find('[data-testid=delete]').exists()).toBe(false)
  })

  it('offers Delete when allowed, except for the signed-in user', async () => {
    const ann = user()
    const wrapper = mount(UserTable, {
      props: { users: [ann, user({ id: 'u2', email: 'bob@example.com', role: 'admin' })], caption: 'Users', currentUserId: 'u2', canDelete: true },
      global,
    })
    const rows = wrapper.findAll('tbody tr')
    expect(rows[1]!.find('[data-testid=delete]').exists()).toBe(false)
    const del = rows[0]!.find('[data-testid=delete]')
    expect(del.attributes('aria-label')).toBe('Delete ann@example.com')
    await del.trigger('click')
    expect(wrapper.emitted('delete')).toEqual([[ann]])
  })
})

describe('PasswordChangeForm', () => {
  async function fill(wrapper: ReturnType<typeof mount>, values: string[]) {
    const inputs = wrapper.findAll('input[type=password]')
    for (const [i, v] of values.entries()) await inputs[i]!.setValue(v)
  }

  it('validates before emitting', async () => {
    const wrapper = mount(PasswordChangeForm, { global })
    await fill(wrapper, ['', 'a new password', 'a different one'])
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('submit')).toBeUndefined()
    expect(wrapper.findAll('[role=alert]').length).toBe(2)
  })

  it('emits the request body, shows server field errors, and clears the fields when done', async () => {
    const wrapper = mount(PasswordChangeForm, { global })
    await fill(wrapper, ['the old password', 'a new password', 'a new password'])
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('submit')![0]).toEqual([{ current_password: 'the old password', new_password: 'a new password' }])

    await wrapper.setProps({
      error: new ApiError({ type: 'about:blank', status: 422, title: 'Invalid', errors: [{ field: '/current_password', detail: 'The current password is wrong.' }] }),
      done: 1,
    })
    expect(wrapper.text()).toContain('The current password is wrong.')
    for (const input of wrapper.findAll('input[type=password]')) expect((input.element as HTMLInputElement).value).toBe('')
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
    await inputs[2]!.setValue('a long enough password')
    await wrapper.find('select').setValue('operator')
    await wrapper.find('form').trigger('submit')

    expect(wrapper.emitted('create')![0]).toEqual([{ email: 'new@example.com', password: 'a long enough password', role: 'operator' }])
  })

  it('toggles password visibility with an accessible control', async () => {
    const wrapper = mount(UserForm, { global })
    const toggle = wrapper.find('button[aria-label="Show password"]')
    expect(wrapper.findAll('input')[2]!.attributes('type')).toBe('password')
    await toggle.trigger('click')
    expect(wrapper.findAll('input')[2]!.attributes('type')).toBe('text')
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

  it('emits a changed display name', async () => {
    const wrapper = mount(UserForm, { props: { user: user() }, global })
    await wrapper.findAll('input')[0]!.setValue('Ann Lee')
    await wrapper.find('form').trigger('submit')
    expect(wrapper.emitted('update')![0]).toEqual([{ display_name: 'Ann Lee' }])
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
