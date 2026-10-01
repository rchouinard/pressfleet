import type { components } from '../api/schema.gen'
import type { BadgeColor, StateView } from './job'

export type Alert = components['schemas']['Alert']
export type AlertState = components['schemas']['AlertState']
export type AlertSeverity = components['schemas']['AlertSeverity']
export type AlertRule = components['schemas']['AlertRule']
export type AlertRuleUpdate = components['schemas']['AlertRuleUpdate']
export type AlertOperator = components['schemas']['AlertOperator']
export type AlertDelivery = components['schemas']['AlertDelivery']
export type AlertDeliveryState = components['schemas']['AlertDeliveryState']

/** How an alert state looks: always a label and an icon, never colour alone. */
export function alertStateView(state: AlertState): StateView {
  switch (state) {
    case 'pending':
      return { label: 'Pending', color: 'neutral', icon: 'i-lucide-hourglass' }
    case 'firing':
      return { label: 'Firing', color: 'error', icon: 'i-lucide-flame' }
    case 'resolved':
      return { label: 'Resolved', color: 'success', icon: 'i-lucide-circle-check' }
    default:
      return { label: String(state), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

export function severityView(severity: AlertSeverity): StateView {
  switch (severity) {
    case 'critical':
      return { label: 'Critical', color: 'error', icon: 'i-lucide-octagon-alert' }
    case 'warning':
      return { label: 'Warning', color: 'warning', icon: 'i-lucide-triangle-alert' }
    case 'info':
      return { label: 'Info', color: 'info', icon: 'i-lucide-info' }
    default:
      return { label: String(severity), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

export function deliveryStateView(state: AlertDeliveryState): StateView {
  const view: Record<string, { label: string, color: BadgeColor, icon: string }> = {
    pending: { label: 'Retrying', color: 'warning', icon: 'i-lucide-rotate-cw' },
    delivered: { label: 'Delivered', color: 'success', icon: 'i-lucide-circle-check' },
    failed: { label: 'Failed', color: 'error', icon: 'i-lucide-circle-x' },
  }
  return view[state] ?? { label: String(state), color: 'neutral', icon: 'i-lucide-circle-help' }
}

const operatorSymbols: Record<AlertOperator, string> = { gt: '>', gte: '>=', lt: '<', lte: '<=', eq: '=', ne: '!=' }

/** `> 0.9` for a threshold rule; "Expression" for an expression rule. */
export function ruleCondition(rule: Pick<AlertRule, 'kind' | 'operator' | 'threshold'>): string {
  if (rule.kind === 'expression' || rule.operator == null || rule.threshold == null) return 'Expression'
  return `${operatorSymbols[rule.operator] ?? rule.operator} ${rule.threshold}`
}

/** Seconds as "Immediately", "30 s", "10 min", "2 h", "1 d 2 h". */
export function formatForSeconds(seconds: number): string {
  if (seconds <= 0) return 'Immediately'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = seconds % 60
  const parts = [d && `${d} d`, h && `${h} h`, m && `${m} min`, s && `${s} s`].filter(Boolean)
  return parts.join(' ')
}

/** A timestamp for people: local date and time, or the raw text if it won't parse. */
export function formatWhen(iso: string | null | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

/** Start of the given local `YYYY-MM-DD` day as RFC 3339 UTC, or undefined for blank/invalid input. */
export function dayStart(day: string): string | undefined {
  return localDay(day, 0)
}

/** Start of the day after `day`, so "until" includes the whole chosen day. */
export function dayAfter(day: string): string | undefined {
  return localDay(day, 1)
}

function localDay(day: string, plus: number): string | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day)
  if (!m) return undefined
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + plus)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

/** A rule edit form's state. Numbers are strings while being typed. */
export interface RuleFormState {
  name: string
  description: string
  severity: AlertSeverity
  operator: AlertOperator
  threshold: string
  forSeconds: string
}

export function ruleFormFrom(rule: AlertRule): RuleFormState {
  return {
    name: rule.name,
    description: rule.description,
    severity: rule.severity,
    operator: rule.operator ?? 'gt',
    threshold: rule.threshold == null ? '' : String(rule.threshold),
    forSeconds: String(rule.for_seconds),
  }
}

/** Client-side checks that save a round trip; the server still validates. Keys are JSON Pointers like the API's. */
export function validateRuleForm(rule: Pick<AlertRule, 'kind'>, f: RuleFormState): Record<string, string> {
  const errors: Record<string, string> = {}
  if (!f.name.trim()) errors['/name'] = 'A name is required.'
  if (rule.kind === 'threshold') {
    if (f.threshold.trim() === '' || !Number.isFinite(Number(f.threshold))) errors['/threshold'] = 'Enter a number.'
  }
  const secs = Number(f.forSeconds)
  if (f.forSeconds.trim() === '' || !Number.isInteger(secs) || secs < 0 || secs > 604800) {
    errors['/for_seconds'] = 'Enter whole seconds from 0 to 604800 (7 days).'
  }
  return errors
}

/** The PATCH body: only what changed. Empty when nothing did. */
export function ruleUpdateBody(rule: AlertRule, f: RuleFormState): AlertRuleUpdate {
  const body: AlertRuleUpdate = {}
  if (f.name.trim() !== rule.name) body.name = f.name.trim()
  if (f.description !== rule.description) body.description = f.description
  if (f.severity !== rule.severity) body.severity = f.severity
  if (rule.kind === 'threshold') {
    if (f.operator !== rule.operator) body.operator = f.operator
    if (Number(f.threshold) !== rule.threshold) body.threshold = Number(f.threshold)
  }
  if (Number(f.forSeconds) !== rule.for_seconds) body.for_seconds = Number(f.forSeconds)
  return body
}
