import { describe, expect, it } from 'vitest'
import {
  ANY, channelCreateBody, channelDestination, channelFormFrom, channelTypeLabel, channelUpdateBody, emptyChannelForm, parseRecipients,
  validateChannelForm,
} from '../app/utils/alert-channel'
import { emailChannel, webhookChannel } from './alert-fixtures'

describe('parseRecipients', () => {
  it('splits on commas, semicolons, spaces and new lines', () => {
    expect(parseRecipients('a@x.io, b@x.io;c@x.io\nd@x.io  ')).toEqual(['a@x.io', 'b@x.io', 'c@x.io', 'd@x.io'])
    expect(parseRecipients('')).toEqual([])
  })
})

describe('validateChannelForm', () => {
  it('requires a name and recipients for email', () => {
    expect(Object.keys(validateChannelForm(emptyChannelForm('email'), false)).sort()).toEqual(['/email/to', '/name'])
  })

  it('rejects malformed recipients', () => {
    const f = { ...emptyChannelForm('email'), name: 'x', emailTo: 'not-an-address' }
    expect(validateChannelForm(f, false)['/email/to']).toMatch(/plain addresses/)
  })

  it('needs host and sender for a channel with its own SMTP server', () => {
    const f = { ...emptyChannelForm('email'), name: 'x', emailTo: 'a@x.io', usePlatformRelay: false, port: '99999' }
    expect(Object.keys(validateChannelForm(f, false)).sort()).toEqual(['/email/from_email', '/email/host', '/email/port'])
  })

  it('requires the webhook URL on create but not on edit', () => {
    const f = { ...emptyChannelForm('webhook'), name: 'x' }
    expect(validateChannelForm(f, false)['/webhook/url']).toBeDefined()
    expect(validateChannelForm(f, true)).toEqual({})
  })
})

describe('channelCreateBody', () => {
  it('builds an email channel that uses the platform relay', () => {
    const f = { ...emptyChannelForm('email'), name: ' Ops ', emailTo: 'a@x.io b@x.io', minSeverity: 'warning' as const }
    expect(channelCreateBody(f)).toEqual({ name: 'Ops', type: 'email', enabled: true, min_severity: 'warning', email: { to: ['a@x.io', 'b@x.io'] } })
  })

  it('builds an email channel with its own server and a password', () => {
    const f = { ...emptyChannelForm('email'), name: 'x', emailTo: 'a@x.io', usePlatformRelay: false, host: 'smtp.x.io', port: '587', username: 'u', password: 'pw', fromEmail: 'alerts@x.io' }
    expect(channelCreateBody(f).email).toEqual({
      to: ['a@x.io'], host: 'smtp.x.io', port: 587, encryption: 'starttls', username: 'u', password: 'pw', from_email: 'alerts@x.io',
    })
  })

  it('builds a webhook with an optional signing secret and limits', () => {
    const f = { ...emptyChannelForm('webhook'), name: 'x', webhookUrl: ' https://h.example/x ', format: 'teams' as const, signingSecret: 's3', ruleId: 'R1', clientGroupId: 'G1' }
    expect(channelCreateBody(f)).toEqual({
      name: 'x', type: 'webhook', enabled: true, min_severity: 'info', rule_id: 'R1', client_group_id: 'G1',
      webhook: { url: 'https://h.example/x', format: 'teams', signing_secret: 's3' },
    })
  })

  it('leaves out the webhook signing secret when blank', () => {
    const f = { ...emptyChannelForm('webhook'), name: 'x', webhookUrl: 'https://h.example/x' }
    expect(channelCreateBody(f).webhook).toEqual({ url: 'https://h.example/x', format: 'generic' })
  })
})

describe('write-only secrets on edit', () => {
  it('never fills secrets into the edit form', () => {
    const f = channelFormFrom(webhookChannel())
    expect(f.webhookUrl).toBe('')
    expect(f.signingSecret).toBe('')
    expect(f.format).toBe('slack')
    const e = channelFormFrom(emailChannel({ email: { ...emailChannel().email!, uses_platform_relay: false, host: 'smtp.x.io', port: 587, encryption: 'tls', username: 'u', has_password: true } }))
    expect(e.password).toBe('')
    expect(e.host).toBe('smtp.x.io')
  })

  it('sends nothing when nothing changed', () => {
    for (const c of [emailChannel(), webhookChannel()]) expect(channelUpdateBody(c, channelFormFrom(c))).toEqual({})
  })

  it('keeps stored secrets unless a new one is typed or removal is chosen', () => {
    const c = webhookChannel()
    const f = channelFormFrom(c)
    f.name = 'Slack ops'
    expect(channelUpdateBody(c, f)).toEqual({ name: 'Slack ops' })

    f.webhookUrl = 'https://hooks.slack.com/services/NEW'
    f.signingSecret = 'rotated'
    expect(channelUpdateBody(c, f).webhook).toEqual({ url: 'https://hooks.slack.com/services/NEW', signing_secret: 'rotated' })

    const g = channelFormFrom(c)
    g.clearSigningSecret = true
    expect(channelUpdateBody(c, g)).toEqual({ webhook: { signing_secret: '' } })
  })

  it('removes the SMTP password with an empty string and replaces it with the typed one', () => {
    const c = emailChannel({ email: { ...emailChannel().email!, uses_platform_relay: false, host: 'smtp.x.io', port: 587, encryption: 'starttls', username: 'u', has_password: true, from_email: 'a@x.io' } })
    const f = channelFormFrom(c)
    f.clearPassword = true
    expect(channelUpdateBody(c, f)).toEqual({ email: { password: '' } })
    f.clearPassword = false
    f.password = 'new'
    expect(channelUpdateBody(c, f)).toEqual({ email: { password: 'new' } })
  })

  it('switches to the platform relay with an empty host, and clears limits with empty strings', () => {
    const c = emailChannel({ rule_id: 'R1', email: { ...emailChannel().email!, uses_platform_relay: false, host: 'smtp.x.io', from_email: 'a@x.io' } })
    const f = channelFormFrom(c)
    f.usePlatformRelay = true
    f.ruleId = ANY
    expect(channelUpdateBody(c, f)).toEqual({ rule_id: '', email: { host: '' } })
  })
})

describe('channel descriptions', () => {
  it('describes destinations without secrets', () => {
    expect(channelDestination(emailChannel())).toBe('ops@example.com via the platform relay')
    expect(channelDestination(emailChannel({ email: { ...emailChannel().email!, to: ['a@x.io', 'b@x.io', 'c@x.io', 'd@x.io'] } }))).toBe('a@x.io, b@x.io and 2 more via the platform relay')
    expect(channelDestination(webhookChannel())).toBe('https://hooks.slack.com/…')
  })

  it('labels the type and webhook format', () => {
    expect(channelTypeLabel(emailChannel())).toBe('Email')
    expect(channelTypeLabel(webhookChannel())).toBe('Webhook (Slack)')
    expect(channelTypeLabel(webhookChannel({ webhook: { format: 'generic', url: 'https://x/…', has_signing_secret: false } }))).toBe('Webhook')
  })
})
