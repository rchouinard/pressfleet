import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import ClientGroupDeleteDialog from '../app/components/client-group/ClientGroupDeleteDialog.vue'
import ClientGroupRepositoryDeleteDialog from '../app/components/client-group/ClientGroupRepositoryDeleteDialog.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import { deleteQuery, nameConfirmed, repositoryCommands } from '../app/utils/client-group'
import { uiStubs } from './stubs'

const UModal = defineComponent({
  props: ['open', 'title'],
  setup(props: { open: boolean, title: string }, { slots }) {
    return () => (props.open ? h('div', { 'role': 'dialog', 'aria-label': props.title }, [slots.body?.(), slots.footer?.()]) : null)
  },
})
const NuxtLink = defineComponent({ props: ['to'], template: '<a :href="to"><slot /></a>' })
const global = { stubs: { ...uiStubs, UModal, NuxtLink }, components: { ProblemAlert } }

const ID = '01j9zq3v6t8k4m2n5p7r9s1x3y'
const withRepo = { id: ID, name: 'Acme', site_count: 0, backup_repository: { location: '/data/backups/repositories/' + ID } }
const bare = { ...withRepo, backup_repository: null }

describe('repository helpers', () => {
  it('fills the group ID into the documented commands', () => {
    const c = repositoryCommands(ID)
    expect(c.export).toBe(`docker compose exec control-plane pressfleet repository export ${ID} /var/lib/pressfleet/tmp/${ID}`)
    expect(c.delete).toBe(`docker compose exec control-plane pressfleet repository delete ${ID} --yes`)
  })

  it('confirms only the exact name, ignoring surrounding spaces', () => {
    expect(nameConfirmed(' Acme ', 'Acme')).toBe(true)
    expect(nameConfirmed('acme', 'Acme')).toBe(false)
    expect(nameConfirmed('', '')).toBe(false)
  })

  it('asks for the repository only when chosen', () => {
    expect(deleteQuery(false)).toEqual({})
    expect(deleteQuery(true)).toEqual({ delete_backup_repository: true })
  })
})

function confirmButton(w: ReturnType<typeof mount>) {
  return w.find('[data-testid=confirm]')
}

describe('ClientGroupDeleteDialog', () => {
  it('deletes a group without backups at once', async () => {
    const w = mount(ClientGroupDeleteDialog, { props: { open: true, group: bare, canOffboard: true }, global })
    expect(w.text()).not.toContain('backup repository')
    expect(confirmButton(w).attributes('disabled')).toBeUndefined()
    await confirmButton(w).trigger('click')
    expect(w.emitted('confirm')).toEqual([[false]])
  })

  it('refuses a group with sites', () => {
    const w = mount(ClientGroupDeleteDialog, { props: { open: true, group: { ...bare, site_count: 2 }, canOffboard: true }, global })
    expect(w.text()).toContain('2 sites')
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
  })

  it('shows the ID and commands for a group with backups, and links to the export', () => {
    const w = mount(ClientGroupDeleteDialog, { props: { open: true, group: withRepo, canOffboard: false }, global })
    expect(w.text()).toContain('backup repository')
    const values = w.findAll('input[readonly]').map(i => (i.element as HTMLInputElement).value)
    expect(values).toEqual([ID, repositoryCommands(ID).export, repositoryCommands(ID).delete])
    expect(w.find(`a[href="/client-groups/${ID}"]`).exists()).toBe(true)
    // Without client_group.offboard there is no way to delete it here.
    expect(w.find('input[type=checkbox]').exists()).toBe(false)
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
  })

  it('deletes the backups too only once chosen and the name is typed', async () => {
    const w = mount(ClientGroupDeleteDialog, { props: { open: true, group: withRepo, canOffboard: true }, global })
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
    await w.find('input[type=checkbox]').setValue(true)
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
    const name = w.findAll('input').find(i => !i.attributes('readonly') && i.attributes('type') !== 'checkbox')!
    await name.setValue('Acm')
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
    await name.setValue('Acme')
    expect(confirmButton(w).attributes('disabled')).toBeUndefined()
    expect(confirmButton(w).text()).toBe('Delete group and backups')
    await confirmButton(w).trigger('click')
    expect(w.emitted('confirm')).toEqual([[true]])
  })

  it('offers the repository options when the server reports one the list missed', () => {
    const error = new ApiError({ type: 'about:blank', title: 'Conflict', status: 409, detail: 'conflict: the client group still has a backup repository.' })
    const w = mount(ClientGroupDeleteDialog, { props: { open: true, group: bare, canOffboard: true, error }, global })
    expect(w.find('input[type=checkbox]').exists()).toBe(true)
    expect(w.find('[role=alert]').text()).toContain('backup repository')
  })
})

describe('ClientGroupRepositoryDeleteDialog', () => {
  it('needs the group name typed, and links to the export first', async () => {
    const w = mount(ClientGroupRepositoryDeleteDialog, {
      props: { open: true, group: withRepo, exportHref: '#deleted-backups-heading' },
      global,
    })
    expect(w.text()).toContain('permanently deletes every backup')
    expect(w.find('a[href="#deleted-backups-heading"]').exists()).toBe(true)
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
    await w.find('input').setValue('Acme')
    await confirmButton(w).trigger('click')
    expect(w.emitted('confirm')).toHaveLength(1)
  })

  it('starts over each time it opens', async () => {
    const w = mount(ClientGroupRepositoryDeleteDialog, { props: { open: true, group: withRepo }, global })
    await w.find('input').setValue('Acme')
    await w.setProps({ open: false })
    await w.setProps({ open: true })
    expect((w.find('input').element as HTMLInputElement).value).toBe('')
    expect(confirmButton(w).attributes('disabled')).toBeDefined()
  })
})
