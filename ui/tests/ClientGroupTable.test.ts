import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import ClientGroupTable from '../app/components/client-group/ClientGroupTable.vue'
import type { ClientGroup } from '../app/utils/client-group'

// UButton is a Nuxt UI auto-import; a plain button keeps the attributes we assert on.
const UButton = defineComponent({ template: '<button type="button"><slot /></button>' })

const groups: ClientGroup[] = [
  { id: 'g1', name: 'Acme', notes: 'VIP', site_count: 2, created_at: '2026-01-02T00:00:00Z', updated_at: '2026-01-02T00:00:00Z' },
  { id: 'g2', name: 'Globex', notes: '', site_count: 1, created_at: '2026-01-03T00:00:00Z', updated_at: '2026-01-03T00:00:00Z' },
]

function render(canUpdate: boolean, canDelete: boolean) {
  return mount(ClientGroupTable, {
    props: { groups, canUpdate, canDelete },
    global: { stubs: { UButton } },
  })
}

describe('ClientGroupTable', () => {
  it('lists names, site counts and notes', () => {
    const w = render(false, false)
    expect(w.text()).toContain('Acme')
    expect(w.text()).toContain('2 sites')
    expect(w.text()).toContain('1 site')
    expect(w.text()).toContain('VIP')
  })

  it('shows no actions column for a viewer', () => {
    const w = render(false, false)
    expect(w.findAll('button')).toHaveLength(0)
    expect(w.text()).not.toContain('Actions')
  })

  it('shows edit but not delete for an operator', () => {
    const w = render(true, false)
    expect(w.find('[aria-label="Edit Acme"]').exists()).toBe(true)
    expect(w.find('[aria-label="Delete Acme"]').exists()).toBe(false)
  })

  it('emits the group for edit and delete for an admin', async () => {
    const w = render(true, true)
    await w.find('[aria-label="Edit Globex"]').trigger('click')
    await w.find('[aria-label="Delete Acme"]').trigger('click')
    expect(w.emitted('edit')![0]).toEqual([groups[1]])
    expect(w.emitted('delete')![0]).toEqual([groups[0]])
  })
})
