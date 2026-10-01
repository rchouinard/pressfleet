import { describe, expect, it } from 'vitest'
import {
  alertStateView, dayAfter, dayStart, deliveryStateView, formatForSeconds, ruleCondition, ruleFormFrom, ruleUpdateBody, severityView,
  validateRuleForm,
} from '../app/utils/alert'
import { rule } from './alert-fixtures'

describe('state and severity views', () => {
  it('give every state a label and an icon, not colour alone', () => {
    for (const v of [alertStateView('pending'), alertStateView('firing'), alertStateView('resolved'), severityView('info'), severityView('warning'), severityView('critical'), deliveryStateView('pending'), deliveryStateView('delivered'), deliveryStateView('failed')]) {
      expect(v.label).not.toBe('')
      expect(v.icon).toMatch(/^i-lucide-/)
    }
    expect(alertStateView('firing').color).toBe('error')
    expect(alertStateView('resolved').color).toBe('success')
  })

  it('shows an unknown state as sent', () => {
    expect(alertStateView('snoozed' as never).label).toBe('snoozed')
    expect(deliveryStateView('bounced' as never).label).toBe('bounced')
  })
})

describe('ruleCondition and formatForSeconds', () => {
  it('formats threshold rules and expression rules', () => {
    expect(ruleCondition(rule())).toBe('> 0.9')
    expect(ruleCondition(rule({ operator: 'lte', threshold: 5 }))).toBe('<= 5')
    expect(ruleCondition(rule({ kind: 'expression', operator: null, threshold: null }))).toBe('Expression')
  })

  it('formats durations', () => {
    expect(formatForSeconds(0)).toBe('Immediately')
    expect(formatForSeconds(30)).toBe('30 s')
    expect(formatForSeconds(600)).toBe('10 min')
    expect(formatForSeconds(3600 * 26 + 60)).toBe('1 d 2 h 1 min')
  })
})

describe('date filters', () => {
  it('turns a local day into the start of that day and the day after', () => {
    expect(dayStart('2026-10-01')).toBe(new Date(2026, 9, 1).toISOString())
    expect(dayAfter('2026-10-31')).toBe(new Date(2026, 10, 1).toISOString())
  })

  it('ignores blank or malformed input', () => {
    expect(dayStart('')).toBeUndefined()
    expect(dayStart('10/01/2026')).toBeUndefined()
  })
})

describe('rule edit form', () => {
  it('sends only what changed', () => {
    const r = rule()
    const f = ruleFormFrom(r)
    expect(ruleUpdateBody(r, f)).toEqual({})
    f.threshold = '0.95'
    f.forSeconds = '0'
    f.severity = 'critical'
    expect(ruleUpdateBody(r, f)).toEqual({ threshold: 0.95, for_seconds: 0, severity: 'critical' })
  })

  it('leaves the threshold alone for expression rules', () => {
    const r = rule({ kind: 'expression', operator: null, threshold: null })
    const f = ruleFormFrom(r)
    f.name = 'Site down'
    expect(ruleUpdateBody(r, f)).toEqual({ name: 'Site down' })
    expect(validateRuleForm(r, f)).toEqual({})
  })

  it('validates name, threshold and duration with JSON Pointer keys', () => {
    const r = rule()
    const f = ruleFormFrom(r)
    f.name = ' '
    f.threshold = 'abc'
    f.forSeconds = '1.5'
    expect(Object.keys(validateRuleForm(r, f)).sort()).toEqual(['/for_seconds', '/name', '/threshold'])
    f.name = 'x'
    f.threshold = '1e3'
    f.forSeconds = '604801'
    expect(Object.keys(validateRuleForm(r, f))).toEqual(['/for_seconds'])
  })
})
