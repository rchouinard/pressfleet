import type { components } from '../api/schema.gen'
import type { AlertSeverity } from './alert'

export type AlertChannel = components['schemas']['AlertChannel']
export type AlertChannelCreate = components['schemas']['AlertChannelCreate']
export type AlertChannelUpdate = components['schemas']['AlertChannelUpdate']
export type AlertChannelType = components['schemas']['AlertChannelType']
export type WebhookFormat = components['schemas']['WebhookFormat']
export type Encryption = 'starttls' | 'tls' | 'none'

/** The latest test-send outcome of a channel, for the channel table. */
export interface TestOutcome {
  state: 'testing' | 'done'
  success?: boolean
  error?: string | null
  durationMs?: number
}

/** Select items can't use the empty string, so "no limit" is this value in the form. */
export const ANY = 'any'

export interface ChannelFormState {
  name: string
  type: AlertChannelType
  enabled: boolean
  minSeverity: AlertSeverity
  /** `ANY` or a rule ID. */
  ruleId: string
  /** `ANY` or a client group ID. */
  clientGroupId: string
  /** Recipients, separated by commas, semicolons, spaces or new lines. */
  emailTo: string
  usePlatformRelay: boolean
  host: string
  port: string
  encryption: Encryption
  username: string
  /** Write-only: always starts empty, even when editing. */
  password: string
  clearPassword: boolean
  fromEmail: string
  fromName: string
  /** Write-only: always starts empty, even when editing. */
  webhookUrl: string
  format: WebhookFormat
  /** Write-only: always starts empty, even when editing. */
  signingSecret: string
  clearSigningSecret: boolean
}

export function emptyChannelForm(type: AlertChannelType = 'email'): ChannelFormState {
  return {
    name: '',
    type,
    enabled: true,
    minSeverity: 'info',
    ruleId: ANY,
    clientGroupId: ANY,
    emailTo: '',
    usePlatformRelay: true,
    host: '',
    port: '',
    encryption: 'starttls',
    username: '',
    password: '',
    clearPassword: false,
    fromEmail: '',
    fromName: '',
    webhookUrl: '',
    format: 'generic',
    signingSecret: '',
    clearSigningSecret: false,
  }
}

/** The edit form for a channel. Secrets are never filled in: the API doesn't return them. */
export function channelFormFrom(c: AlertChannel): ChannelFormState {
  const f = emptyChannelForm(c.type)
  f.name = c.name
  f.enabled = c.enabled
  f.minSeverity = c.min_severity
  f.ruleId = c.rule_id ?? ANY
  f.clientGroupId = c.client_group_id ?? ANY
  if (c.email) {
    f.emailTo = c.email.to.join('\n')
    f.usePlatformRelay = c.email.uses_platform_relay
    f.host = c.email.host
    f.port = c.email.port ? String(c.email.port) : ''
    f.encryption = (c.email.encryption || 'starttls') as Encryption
    f.username = c.email.username
    f.fromEmail = c.email.from_email
    f.fromName = c.email.from_name
  }
  if (c.webhook) f.format = c.webhook.format
  return f
}

export function parseRecipients(text: string): string[] {
  return text.split(/[\s,;]+/).map(s => s.trim()).filter(Boolean)
}

/** Client-side checks; keys are JSON Pointers like the API's field errors. */
export function validateChannelForm(f: ChannelFormState, editing: boolean): Record<string, string> {
  const errors: Record<string, string> = {}
  if (!f.name.trim()) errors['/name'] = 'A name is required.'
  if (f.type === 'email') {
    const to = parseRecipients(f.emailTo)
    if (to.length === 0) errors['/email/to'] = 'Add at least one recipient.'
    else if (to.length > 20) errors['/email/to'] = 'At most 20 recipients.'
    else if (to.some(a => !/^[^@\s]+@[^@\s]+$/.test(a))) errors['/email/to'] = 'Use plain addresses such as ops@example.com.'
    if (!f.usePlatformRelay) {
      if (!f.host.trim()) errors['/email/host'] = 'Enter the SMTP server.'
      if (!f.fromEmail.trim() && !editing) errors['/email/from_email'] = 'A sender address is required with your own SMTP server.'
      if (f.port.trim() !== '' && !(Number.isInteger(Number(f.port)) && Number(f.port) >= 0 && Number(f.port) <= 65535)) {
        errors['/email/port'] = 'Enter a port from 0 to 65535.'
      }
    }
  } else if (!editing && !f.webhookUrl.trim()) {
    errors['/webhook/url'] = 'A webhook URL is required.'
  }
  return errors
}

/** The POST body for a new channel. */
export function channelCreateBody(f: ChannelFormState): AlertChannelCreate {
  const body: AlertChannelCreate = {
    name: f.name.trim(),
    type: f.type,
    enabled: f.enabled,
    min_severity: f.minSeverity,
  }
  if (f.ruleId !== ANY) body.rule_id = f.ruleId
  if (f.clientGroupId !== ANY) body.client_group_id = f.clientGroupId
  if (f.type === 'email') {
    const email: NonNullable<AlertChannelCreate['email']> = { to: parseRecipients(f.emailTo) }
    if (f.fromEmail.trim()) email.from_email = f.fromEmail.trim()
    if (f.fromName.trim()) email.from_name = f.fromName.trim()
    if (!f.usePlatformRelay) {
      email.host = f.host.trim()
      if (f.port.trim() !== '') email.port = Number(f.port)
      email.encryption = f.encryption
      if (f.username.trim()) email.username = f.username.trim()
      if (f.password) email.password = f.password
    }
    body.email = email
  } else {
    const webhook: NonNullable<AlertChannelCreate['webhook']> = { url: f.webhookUrl.trim(), format: f.format }
    if (f.signingSecret) webhook.signing_secret = f.signingSecret
    body.webhook = webhook
  }
  return body
}

/**
 * The PATCH body: only what differs from `c`. Secrets are sent only when
 * the user typed a new one (replace) or chose to remove it (empty string);
 * otherwise they stay as stored. Empty when nothing changed.
 */
export function channelUpdateBody(c: AlertChannel, f: ChannelFormState): AlertChannelUpdate {
  const body: AlertChannelUpdate = {}
  if (f.name.trim() !== c.name) body.name = f.name.trim()
  if (f.minSeverity !== c.min_severity) body.min_severity = f.minSeverity
  if (f.ruleId !== (c.rule_id ?? ANY)) body.rule_id = f.ruleId === ANY ? '' : f.ruleId
  if (f.clientGroupId !== (c.client_group_id ?? ANY)) body.client_group_id = f.clientGroupId === ANY ? '' : f.clientGroupId

  if (c.type === 'email' && c.email) {
    const e = c.email
    const email: NonNullable<AlertChannelUpdate['email']> = {}
    const to = parseRecipients(f.emailTo)
    if (to.join(',') !== e.to.join(',')) email.to = to
    if (f.fromEmail.trim() !== e.from_email) email.from_email = f.fromEmail.trim()
    if (f.fromName.trim() !== e.from_name) email.from_name = f.fromName.trim()
    if (f.usePlatformRelay) {
      if (!e.uses_platform_relay) email.host = ''
    } else {
      if (f.host.trim() !== e.host) email.host = f.host.trim()
      const port = f.port.trim() === '' ? 0 : Number(f.port)
      if (port !== e.port) email.port = port
      if (f.encryption !== (e.encryption || 'starttls')) email.encryption = f.encryption
      if (f.username.trim() !== e.username) email.username = f.username.trim()
      if (f.clearPassword) email.password = ''
      else if (f.password) email.password = f.password
    }
    if (Object.keys(email).length) body.email = email
  }
  if (c.type === 'webhook' && c.webhook) {
    const webhook: NonNullable<AlertChannelUpdate['webhook']> = {}
    if (f.webhookUrl.trim()) webhook.url = f.webhookUrl.trim()
    if (f.format !== c.webhook.format) webhook.format = f.format
    if (f.clearSigningSecret) webhook.signing_secret = ''
    else if (f.signingSecret) webhook.signing_secret = f.signingSecret
    if (Object.keys(webhook).length) body.webhook = webhook
  }
  return body
}

/** One line describing where a channel delivers, without any secret. */
export function channelDestination(c: Pick<AlertChannel, 'type' | 'email' | 'webhook'>): string {
  if (c.type === 'email' && c.email) {
    const to = c.email.to.length > 2 ? `${c.email.to.slice(0, 2).join(', ')} and ${c.email.to.length - 2} more` : c.email.to.join(', ')
    return `${to} via ${c.email.uses_platform_relay ? 'the platform relay' : c.email.host}`
  }
  if (c.type === 'webhook' && c.webhook) return c.webhook.url
  return '-'
}

export function channelTypeLabel(c: Pick<AlertChannel, 'type' | 'webhook'>): string {
  if (c.type === 'email') return 'Email'
  const f = c.webhook?.format
  return f === 'slack' ? 'Webhook (Slack)' : f === 'teams' ? 'Webhook (Teams)' : 'Webhook'
}
