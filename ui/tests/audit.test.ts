import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AuditTable from '../app/components/audit/AuditTable.vue'
import {
  actorLabel, auditQuery, detailEntries, emptyAuditFilters, localToTimestamp, resultView, targetRoute, targetTypeLabel, validateAuditRange,
  type AuditEvent,
} from '../app/utils/audit'
import { uiStubs } from './stubs'

const event = (over: Partial<AuditEvent> = {}): AuditEvent => ({
  id: '01j9zq3v6t8k4m2n5p7r9s1x3y',
  occurred_at: '2026-10-02T12:00:00Z',
  actor_user_id: 'u1',
  actor_token_id: null,
  action: 'site.delete',
  target_type: 'site',
  target_id: '01site0000000000000000000a',
  result: 'success',
  source_ip: '203.0.113.7',
  ...over,
})

describe('auditQuery', () => {
  it('leaves blank filters out', () => {
    expect(auditQuery(emptyAuditFilters())).toEqual({})
    expect(auditQuery({ ...emptyAuditFilters(), action: '  ', targetId: ' ' })).toEqual({})
  })

  it('maps the filters to the API parameters', () => {
    const q = auditQuery({ actorUserId: 'u1', action: ' site.delete ', targetType: 'site', targetId: 'abc', since: '2026-10-01T09:30', until: '2026-10-02T09:30' })
    expect(q).toMatchObject({ actor_user_id: 'u1', action: 'site.delete', target_type: 'site', target_id: 'abc' })
    expect(q.since).toBe(new Date('2026-10-01T09:30').toISOString())
    expect(q.until).toBe(new Date('2026-10-02T09:30').toISOString())
  })
})

describe('time range', () => {
  it('converts local times to UTC timestamps', () => {
    expect(localToTimestamp('')).toBeUndefined()
    expect(localToTimestamp('nope')).toBeUndefined()
    expect(localToTimestamp('2026-10-01T09:30')).toMatch(/^2026-10-01T\d\d:30:00\.000Z$|^2026-10-0[12]T/)
  })

  it('rejects an end that is not after the start', () => {
    const f = { since: '2026-10-02T10:00', until: '2026-10-02T09:00' }
    expect(validateAuditRange(f)).toContain('after')
    expect(validateAuditRange({ since: '2026-10-02T10:00', until: '2026-10-02T10:00' })).toBeTruthy()
    expect(validateAuditRange({ since: '2026-10-02T09:00', until: '2026-10-02T10:00' })).toBeUndefined()
    expect(validateAuditRange({ since: '', until: '2026-10-02T10:00' })).toBeUndefined()
    expect(validateAuditRange({ since: '', until: '' })).toBeUndefined()
  })
})

describe('labels', () => {
  it('shows every result with text and an icon', () => {
    for (const r of ['success', 'failure', 'denied']) {
      const v = resultView(r)
      expect(v.label).toBeTruthy()
      expect(v.icon).toMatch(/^i-lucide-/)
    }
    expect(resultView('denied').color).toBe('error')
    expect(resultView('new-result').label).toBe('new-result')
  })

  it('names the actor, a deleted actor, and a missing one', () => {
    expect(actorLabel(event(), { u1: 'Alex' })).toBe('Alex')
    expect(actorLabel(event({ actor_user_id: '01abcdefghij' }), {})).toBe('Deleted user 01abcdef')
    expect(actorLabel(event({ actor_user_id: null }), {})).toBe('Not signed in')
  })

  it('names target types and links the ones with a page', () => {
    expect(targetTypeLabel(null)).toBe('Platform')
    expect(targetTypeLabel('client_group')).toBe('Client group')
    expect(targetTypeLabel('future_thing')).toBe('future_thing')
    expect(targetRoute('site', 's1')).toBe('/sites/s1')
    expect(targetRoute('job', 'j1')).toBe('/jobs/j1')
    expect(targetRoute('api_token', 't1')).toBeUndefined()
    expect(targetRoute(null, null)).toBeUndefined()
  })

  it('sorts event details', () => {
    expect(detailEntries(event())).toEqual([])
    expect(detailEntries(event({ details: { reason: 'closed', bytes_sent: '12' } }))).toEqual([['bytes_sent', '12'], ['reason', 'closed']])
  })
})

describe('AuditTable', () => {
  const NuxtLink = defineComponent({ props: ['to'], template: '<a :href="to"><slot /></a>' })
  const render = (events: AuditEvent[]) => mount(AuditTable, { props: { events, caption: 'Events', names: { u1: 'Alex' } }, global: { stubs: { ...uiStubs, NuxtLink } } })

  it('shows who did what to which target, with the result', () => {
    const w = render([event(), event({ id: 'e2', actor_user_id: null, action: 'self.login', target_type: null, target_id: null, result: 'failure' })])
    const rows = w.findAll('[data-testid=event]')
    expect(rows).toHaveLength(2)
    expect(rows[0]!.text()).toContain('Alex')
    expect(rows[0]!.text()).toContain('site.delete')
    expect(rows[0]!.text()).toContain('Success')
    expect(rows[0]!.find('a').attributes('href')).toBe('/sites/01site0000000000000000000a')
    expect(rows[1]!.text()).toContain('Not signed in')
    expect(rows[1]!.text()).toContain('Platform')
    expect(rows[1]!.text()).toContain('Failed')
  })

  it('notes API token use and lists details and the address', () => {
    const w = render([event({ actor_token_id: 't1', details: { session_id: 's9' } })])
    expect(w.text()).toContain('via API token')
    expect(w.text()).toContain('203.0.113.7')
    expect(w.text()).toContain('session_id')
    expect(w.text()).toContain('s9')
  })
})
