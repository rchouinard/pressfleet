import type { AlertRule } from '../app/utils/alert'
import type { AlertChannel } from '../app/utils/alert-channel'

export function rule(overrides: Partial<AlertRule> = {}): AlertRule {
  return {
    id: '01J0000000000000000000RULE',
    name: 'Disk nearly full',
    description: 'Fires when a filesystem passes 90%.',
    scope: 'platform',
    client_group_id: null,
    kind: 'threshold',
    expr: 'pressfleet_host_filesystem_used_ratio',
    operator: 'gt',
    threshold: 0.9,
    for_seconds: 600,
    severity: 'warning',
    enabled: true,
    labels: {},
    annotations: {},
    builtin: true,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

export function emailChannel(overrides: Partial<AlertChannel> = {}): AlertChannel {
  return {
    id: '01J000000000000000000CHAN1',
    name: 'Ops email',
    type: 'email',
    enabled: true,
    min_severity: 'info',
    rule_id: null,
    client_group_id: null,
    email: { to: ['ops@example.com'], uses_platform_relay: true, host: '', port: 0, encryption: '', username: '', has_password: false, from_email: '', from_name: '' },
    webhook: null,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

export function webhookChannel(overrides: Partial<AlertChannel> = {}): AlertChannel {
  return emailChannel({
    id: '01J000000000000000000CHAN2',
    name: 'Slack',
    type: 'webhook',
    email: null,
    webhook: { format: 'slack', url: 'https://hooks.slack.com/…', has_signing_secret: true },
    ...overrides,
  })
}
