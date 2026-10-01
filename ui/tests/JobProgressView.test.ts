import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import JobProgressView from '../app/components/job/JobProgressView.vue'
import { ApiError } from '../app/utils/api-error'
import { job, step } from './job-fixtures'

// Nuxt UI components are not registered outside Nuxt; stand in for the few used.
const stubs = {
  UIcon: { props: ['name'], template: '<i :data-icon="name" />' },
  UBadge: { template: '<span data-badge><slot /></span>' },
  UButton: { template: '<button><slot /></button>' },
}
const mountView = (props: Record<string, unknown>) => mount(JobProgressView, { props: props as never, global: { stubs } })

afterEach(() => {
  vi.useRealTimers()
})

describe('JobProgressView', () => {
  it('shows a running job: state, ordered steps with their state, and open running step with its log', () => {
    const wrapper = mountView({
      job: job({
        steps: [
          step({ name: 'create_database', state: 'succeeded', attempts: 1, started_at: '2026-10-01T10:00:01Z', finished_at: '2026-10-01T10:00:03Z', log: [{ time: '2026-10-01T10:00:02Z', level: 'info', message: 'database created' }] }),
          step({ name: 'create_volume', state: 'running', attempts: 1, started_at: '2026-10-01T10:00:03Z', log: [{ time: '2026-10-01T10:00:04Z', level: 'info', message: 'creating volume' }] }),
          step({ name: 'start_containers', state: 'pending' }),
        ],
      }),
    })

    expect(wrapper.find('[role=status] [data-badge]').text()).toContain('Running')
    const items = wrapper.findAll('ol > li')
    expect(items.map(li => li.find('summary').text())).toEqual([
      expect.stringContaining('Create database'),
      expect.stringContaining('Create volume'),
      expect.stringContaining('Start containers'),
    ])
    expect(items[0]!.find('summary').text()).toContain('Done')
    expect(items[0]!.find('summary').text()).toContain('2 s')
    expect(items[1]!.find('summary').text()).toContain('Running')
    expect(items[2]!.find('summary').text()).toContain('Pending')

    // The running step is open; the finished one is collapsed until opened.
    expect(items[0]!.find('details').attributes('open')).toBeUndefined()
    expect(items[1]!.find('details').attributes('open')).toBeDefined()
    expect(items[1]!.find('[role=log]').text()).toContain('creating volume')
    expect(wrapper.find('[role=alert]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=rollback]').exists()).toBe(false)
  })

  it('shows a failed job with its error and the roll-back outcome', () => {
    const wrapper = mountView({
      job: job({
        state: 'failed',
        error: 'create_volume: no space left on device',
        finished_at: '2026-10-01T10:00:20Z',
        steps: [
          step({ name: 'create_database', state: 'rolled_back', attempts: 1 }),
          step({ name: 'create_volume', state: 'failed', attempts: 3, log: [{ time: '2026-10-01T10:00:09Z', level: 'error', message: 'no space left on device' }] }),
        ],
      }),
    })

    expect(wrapper.text()).toContain('Failed, rolled back')
    const alert = wrapper.find('[role=alert]')
    expect(alert.text()).toContain('The job failed')
    expect(alert.text()).toContain('create_volume: no space left on device')
    expect(wrapper.find('[data-testid=rollback]').text()).toContain('rolled back (1 step undone)')
    const items = wrapper.findAll('ol > li')
    expect(items[0]!.text()).toContain('Rolled back')
    expect(items[1]!.text()).toContain('3 attempts')
    // A failed step opens by default so the reason is in view.
    expect(items[1]!.find('details').attributes('open')).toBeDefined()
    expect(items[1]!.find('[role=log]').text()).toContain('no space left on device')
  })

  it('flags an incomplete roll-back as an alert', () => {
    const wrapper = mountView({
      job: job({
        state: 'rollback_failed',
        error: 'cleanup failed',
        steps: [step({ name: 'create_database', state: 'rollback_failed' })],
      }),
    })
    const rollback = wrapper.find('[data-testid=rollback]')
    expect(rollback.attributes('role')).toBe('alert')
    expect(rollback.text()).toContain('left behind')
    expect(wrapper.text()).toContain('Rollback failed')
  })

  it('shows a succeeded job without error or roll-back, with the result', () => {
    const wrapper = mountView({
      job: job({ state: 'succeeded', finished_at: '2026-10-01T10:00:09Z', result: { site_id: 's1', admin_password: { secret_ref: 'x' } }, steps: [step({ state: 'succeeded' })] }),
    })
    expect(wrapper.text()).toContain('Succeeded')
    expect(wrapper.find('[role=alert]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=rollback]').exists()).toBe(false)
    expect(wrapper.text()).toContain('secret_ref')
  })

  it('explains a queued job that is waiting to retry', () => {
    const wrapper = mountView({ job: job({ state: 'queued', attempts: 2, error: 'transient', started_at: null }) })
    expect(wrapper.text()).toContain('Waiting to retry')
    expect(wrapper.find('[role=alert]').text()).toContain('it will be retried')
  })

  it('renders log lines as text only, keeping whitespace', () => {
    const message = '<img src=x onerror=alert(1)>\n  indented <b>bold</b>'
    const wrapper = mountView({
      job: job({ steps: [step({ state: 'running', log: [{ time: '2026-10-01T10:00:00Z', level: 'warn', message }] })] }),
    })
    const log = wrapper.find('[role=log]')
    expect(log.find('img').exists()).toBe(false)
    expect(log.find('b').exists()).toBe(false)
    expect(log.text()).toContain('<img src=x onerror=alert(1)>')
    expect(log.html()).toContain('&lt;img')
    expect(log.find('.whitespace-pre-wrap').text()).toBe(message)
  })

  it('shows empty states for steps without log and jobs without steps', () => {
    const wrapper = mountView({ job: job({ state: 'queued', started_at: null, steps: [] }) })
    expect(wrapper.text()).toContain('The steps appear once a worker starts the job.')
    const done = mountView({ job: job({ state: 'succeeded', steps: [step({ state: 'succeeded', log: [] })] }) })
    done.find('details').element.setAttribute('open', '')
    expect(done.text()).toContain('No log output.')
  })

  it('tells the user a refresh failed and keeps the last state', async () => {
    const wrapper = mountView({
      job: job(),
      pollError: new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Network error', status: 0 }),
      retrying: true,
    })
    expect(wrapper.text()).toContain('trying again')
    expect(wrapper.text()).toContain('Running')
    await wrapper.find('button').trigger('click')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
  })

  it('counts the duration of a running job up', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-01T10:00:11Z'))
    const wrapper = mountView({ job: job({ started_at: '2026-10-01T10:00:01Z' }) })
    expect(wrapper.text()).toContain('10 s')
    await vi.advanceTimersByTimeAsync(5000)
    expect(wrapper.text()).toContain('15 s')
    wrapper.unmount()
  })

  it('keeps a step the user collapsed collapsed across updates', async () => {
    const running = job({ steps: [step({ state: 'running', log: [{ time: '2026-10-01T10:00:00Z', level: 'info', message: 'a' }] })] })
    const wrapper = mountView({ job: running })
    const details = wrapper.find('details')
    expect(details.attributes('open')).toBeDefined()
    ;(details.element as HTMLDetailsElement).open = false
    await details.trigger('toggle')
    await wrapper.setProps({ job: { ...running, updated_at: '2026-10-01T10:00:09Z' } })
    expect(wrapper.find('details').attributes('open')).toBeUndefined()
  })
})
