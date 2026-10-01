import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AlertBadge from '../app/components/alert/AlertBadge.vue'
import AlertChannelTable from '../app/components/alert/AlertChannelTable.vue'
import AlertDeliveryTable from '../app/components/alert/AlertDeliveryTable.vue'
import AlertRuleTable from '../app/components/alert/AlertRuleTable.vue'
import AlertTable from '../app/components/alert/AlertTable.vue'
import { can } from '../app/utils/permissions'
import type { Alert, AlertDelivery } from '../app/utils/alert'
import { emailChannel, rule, webhookChannel } from './alert-fixtures'

// Nuxt UI and Nuxt components are not registered outside Nuxt; stand in for the ones used.
const stubs = {
  AlertBadge,
  UIcon: { props: ['name'], template: '<i :data-icon="name" />' },
  UBadge: { template: '<span data-badge><slot /></span>' },
  UButton: { template: '<button type="button"><slot /></button>' },
  NuxtLink: { props: ['to'], template: '<a :href="to"><slot /></a>' },
}
const opts = (props: Record<string, unknown>) => ({ props: props as never, global: { stubs } })

function alert(overrides: Partial<Alert> = {}): Alert {
  return {
    id: '01J0000000000000000000ALRT',
    rule_id: '01J0000000000000000000RULE',
    rule_name: 'Site down',
    state: 'firing',
    severity: 'critical',
    labels: {},
    site_id: '01J00000000000000000000SITE',
    client_group_id: null,
    summary: 'example.com is down',
    description: '',
    value: 0,
    started_at: '2026-10-01T10:00:00Z',
    fired_at: '2026-10-01T10:05:00Z',
    resolved_at: null,
    updated_at: '2026-10-01T10:05:00Z',
    ...overrides,
  }
}

function delivery(overrides: Partial<AlertDelivery> = {}): AlertDelivery {
  return {
    id: '01J00000000000000000000DLV1',
    alert_id: '01J0000000000000000000ALRT',
    event: 'firing',
    channel_id: '01J000000000000000000CHAN1',
    channel_name: 'Ops email',
    state: 'failed',
    attempts: 5,
    last_error: 'smtp: 535 authentication failed',
    next_attempt_at: null,
    delivered_at: null,
    created_at: '2026-10-01T10:05:00Z',
    updated_at: '2026-10-01T10:06:00Z',
    ...overrides,
  }
}

describe('role gating (permissions the pages use)', () => {
  it('lets operators and admins manage rules, but not viewers', () => {
    expect(can('viewer', 'alert_rule.manage')).toBe(false)
    expect(can('operator', 'alert_rule.manage')).toBe(true)
    expect(can('admin', 'alert_rule.manage')).toBe(true)
  })

  it('lets only admins manage channels', () => {
    expect(can('viewer', 'platform.settings.manage')).toBe(false)
    expect(can('operator', 'platform.settings.manage')).toBe(false)
    expect(can('admin', 'platform.settings.manage')).toBe(true)
    expect(can(undefined, 'platform.settings.manage')).toBe(false)
  })

  it('lets every role read alerts', () => {
    for (const role of ['viewer', 'operator', 'admin'] as const) expect(can(role, 'monitoring.view')).toBe(true)
  })
})

describe('AlertTable', () => {
  it('links the alert and its site, and shows state and severity as text', () => {
    const w = mount(AlertTable, opts({ alerts: [alert()], caption: 'Alerts' }))
    const text = w.text()
    expect(text).toContain('example.com is down')
    expect(text).toContain('Firing')
    expect(text).toContain('Critical')
    const hrefs = w.findAll('a').map(a => a.attributes('href'))
    expect(hrefs).toContain('/alerts/01J0000000000000000000ALRT')
    expect(hrefs).toContain('/sites/01J00000000000000000000SITE')
  })

  it('marks platform alerts and unresolved alerts without a link to a site', () => {
    const w = mount(AlertTable, opts({ alerts: [alert({ site_id: null })], caption: 'Alerts' }))
    expect(w.text()).toContain('Platform')
    expect(w.findAll('a').map(a => a.attributes('href'))).not.toContain('/sites/null')
  })
})

describe('AlertRuleTable', () => {
  const rules = [rule(), rule({ id: 'R2', name: 'Custom', builtin: false, enabled: false })]

  it('shows no controls to a role that cannot manage rules', () => {
    const w = mount(AlertRuleTable, opts({ rules, canManage: false, caption: 'Rules' }))
    expect(w.findAll('button')).toHaveLength(0)
    expect(w.text()).toContain('Disk nearly full')
    expect(w.text()).toContain('Disabled')
  })

  it('shows edit, enable/disable, and delete (custom rules only) when allowed', () => {
    const w = mount(AlertRuleTable, opts({ rules, canManage: true, caption: 'Rules' }))
    const labels = w.findAll('button').map(b => b.text())
    expect(labels.filter(l => l === 'Edit')).toHaveLength(2)
    expect(labels).toContain('Disable')
    expect(labels).toContain('Enable')
    expect(labels.filter(l => l === 'Delete')).toHaveLength(1)
  })

  it('emits the rule for each action', async () => {
    const w = mount(AlertRuleTable, opts({ rules: [rules[1]], canManage: true, caption: 'Rules' }))
    for (const b of w.findAll('button')) await b.trigger('click')
    expect(w.emitted('edit')?.[0]).toEqual([rules[1]])
    expect(w.emitted('toggle')?.[0]).toEqual([rules[1]])
    expect(w.emitted('delete')?.[0]).toEqual([rules[1]])
  })
})

describe('AlertChannelTable', () => {
  const channels = [emailChannel(), webhookChannel()]

  it('lists channels for a read-only role without controls or secrets', () => {
    const w = mount(AlertChannelTable, opts({ channels, canManage: false, caption: 'Channels' }))
    expect(w.findAll('button')).toHaveLength(0)
    expect(w.text()).toContain('ops@example.com via the platform relay')
    expect(w.text()).toContain('https://hooks.slack.com/…')
    expect(w.text()).toContain('Requests are signed')
  })

  it('offers test, edit, toggle and delete to admins', () => {
    const w = mount(AlertChannelTable, opts({ channels: [channels[0]], canManage: true, caption: 'Channels' }))
    expect(w.findAll('button').map(b => b.text())).toEqual(['Test', 'Edit', 'Disable', 'Delete'])
  })

  it('shows the test outcome: success and failure with the reason', () => {
    const tests = {
      [channels[0]!.id]: { state: 'done', success: true, durationMs: 120 },
      [channels[1]!.id]: { state: 'done', success: false, error: 'receiver returned 404' },
    } as const
    const w = mount(AlertChannelTable, opts({ channels, canManage: true, tests, caption: 'Channels' }))
    const statuses = w.findAll('[role="status"]').map(s => s.text())
    expect(statuses).toEqual(['Test delivered in 120 ms.', 'Test failed: receiver returned 404'])
  })

  it('names the rule and client group a channel is limited to', () => {
    const c = emailChannel({ min_severity: 'critical', rule_id: 'R1', client_group_id: 'G1' })
    const w = mount(AlertChannelTable, opts({ channels: [c], canManage: false, ruleNames: { R1: 'Site down' }, groupNames: { G1: 'Acme' }, caption: 'Channels' }))
    expect(w.text()).toContain('Critical and above; rule: Site down; group: Acme')
  })
})

describe('AlertDeliveryTable', () => {
  it('shows the failure reason as text and the state as a label', () => {
    const w = mount(AlertDeliveryTable, opts({ deliveries: [delivery({ last_error: '<b>oops</b>' })], caption: 'Deliveries' }))
    expect(w.text()).toContain('Failed')
    expect(w.text()).toContain('<b>oops</b>')
    expect(w.find('b').exists()).toBe(false)
  })

  it('shows when a retrying delivery is tried next', () => {
    const d = delivery({ state: 'pending', last_error: null, next_attempt_at: '2026-10-01T10:10:00Z', attempts: 1 })
    expect(mount(AlertDeliveryTable, opts({ deliveries: [d], caption: 'D' })).text()).toContain('Next attempt')
  })

  it('links to the alert only when asked', () => {
    expect(mount(AlertDeliveryTable, opts({ deliveries: [delivery()], caption: 'D' })).findAll('a')).toHaveLength(0)
    expect(mount(AlertDeliveryTable, opts({ deliveries: [delivery()], linkAlerts: true, caption: 'D' })).find('a').attributes('href')).toBe('/alerts/01J0000000000000000000ALRT')
  })
})
