import { describe, expect, it } from 'vitest'
import { computed } from 'vue'
import { createEndedJobs, followedJob, formatDuration, isTerminal, jobStateView, jobTypeLabel, rollbackSummary, startedByUser, stepLabel, stepStateView, type JobState } from '../app/utils/job'
import { job, step } from './job-fixtures'

describe('job states', () => {
  it('knows the terminal states from the spec', () => {
    const states: JobState[] = ['queued', 'running', 'rolling_back', 'succeeded', 'failed', 'rollback_failed']
    expect(states.filter(isTerminal)).toEqual(['succeeded', 'failed', 'rollback_failed'])
  })

  it('labels each state with text, an icon and a colour', () => {
    expect(jobStateView({ state: 'queued' }).label).toBe('Queued')
    expect(jobStateView({ state: 'queued', error: 'boom' }).label).toBe('Waiting to retry')
    expect(jobStateView({ state: 'running' }).label).toBe('Running')
    expect(jobStateView({ state: 'rolling_back' }).label).toBe('Rolling back')
    expect(jobStateView({ state: 'succeeded' }).color).toBe('success')
    expect(jobStateView({ state: 'failed' }).label).toBe('Failed, rolled back')
    expect(jobStateView({ state: 'rollback_failed' }).label).toBe('Failed, rollback incomplete')
    for (const s of ['pending', 'running', 'succeeded', 'failed', 'rolled_back', 'rollback_failed'] as const) {
      expect(stepStateView(s).icon).toMatch(/^i-lucide-/)
    }
  })

  it('shows states it does not know as they are', () => {
    expect(jobStateView({ state: 'paused' as JobState }).label).toBe('paused')
  })

  it('treats job types as opaque when unknown', () => {
    expect(jobTypeLabel('site.create')).toBe('Create site')
    expect(jobTypeLabel('backup.run')).toBe('backup.run')
    expect(stepLabel('create_database')).toBe('Create database')
  })
})

describe('formatDuration', () => {
  const t0 = '2026-10-01T10:00:00Z'
  it.each([
    ['2026-10-01T10:00:00.500Z', '500 ms'],
    ['2026-10-01T10:00:12Z', '12 s'],
    ['2026-10-01T10:03:04Z', '3 min 04 s'],
    ['2026-10-01T11:05:00Z', '1 h 05 min'],
  ])('formats %s', (end, want) => {
    expect(formatDuration(t0, end)).toBe(want)
  })

  it('counts an open end up to now and handles missing starts', () => {
    expect(formatDuration(t0, null, Date.parse('2026-10-01T10:00:07Z'))).toBe('7 s')
    expect(formatDuration(null, null)).toBeUndefined()
    expect(formatDuration('garbage', null)).toBeUndefined()
  })
})

describe('rollbackSummary', () => {
  it('reports nothing for a job that is going well', () => {
    const s = rollbackSummary(job({ state: 'running', steps: [step({ state: 'succeeded' })] }))
    expect(s.attempted).toBe(false)
    expect(s.text).toBe('')
  })

  it('says rolling back is in progress', () => {
    const s = rollbackSummary(job({ state: 'rolling_back', steps: [step({ state: 'rolled_back' })] }))
    expect(s.inProgress).toBe(true)
    expect(s.text).toContain('undoing')
  })

  it('counts the steps undone for a failed job', () => {
    const s = rollbackSummary(job({
      state: 'failed',
      steps: [step({ name: 'a', state: 'rolled_back' }), step({ name: 'b', state: 'rolled_back' }), step({ name: 'c', state: 'failed' })],
    }))
    expect(s.rolledBack).toEqual(['a', 'b'])
    expect(s.text).toContain('rolled back (2 steps undone)')
  })

  it('names the steps whose rollback failed', () => {
    const s = rollbackSummary(job({ state: 'rollback_failed', steps: [step({ name: 'create_database', state: 'rollback_failed' })] }))
    expect(s.failed).toEqual(['create_database'])
    expect(s.text).toContain('Create database')
    expect(s.text).toContain('left behind')
  })
})

describe('createEndedJobs', () => {
  it('records a job once, when it ends', () => {
    const ended = createEndedJobs()
    expect(ended.add({ id: 'j1', state: 'running' })).toBe(false)
    expect(ended.has('j1')).toBe(false)
    expect(ended.add({ id: 'j1', state: 'succeeded' })).toBe(true)
    expect(ended.add({ id: 'j1', state: 'succeeded' })).toBe(false)
    expect(ended.has('j1')).toBe(true)
  })

  it('updates what is computed from it', () => {
    // The backups page hid Restore and disabled "Back up now" until a
    // reload, because this didn't recompute when the backup ended.
    const ended = createEndedJobs()
    const busy = computed(() => !ended.has('j1'))
    expect(busy.value).toBe(true)
    ended.add({ id: 'j1', state: 'failed' })
    expect(busy.value).toBe(false)
  })
})

describe('followedJob', () => {
  const user = '01J0000000000000000000USER'
  // Recent jobs come newest first.
  const mine = (id: string, state: JobState) => job({ id, state, created_by: user })
  const platform = (id: string, state: JobState) => job({ id, state, created_by: null })

  it('tells jobs a person started from the platform\'s own', () => {
    expect(startedByUser(mine('a', 'running'))).toBe(true)
    expect(startedByUser(platform('a', 'running'))).toBe(false)
    expect(startedByUser(job())).toBe(false)
  })

  it('picks up the newest unfinished job a person started', () => {
    expect(followedJob([mine('c', 'succeeded'), mine('b', 'running'), mine('a', 'queued')])).toBe('b')
    expect(followedJob([mine('b', 'failed'), mine('a', 'rollback_failed')])).toBeUndefined()
    expect(followedJob([])).toBeUndefined()
  })

  it('never picks up a job the platform started', () => {
    expect(followedJob([platform('b', 'running'), mine('a', 'succeeded')])).toBeUndefined()
    expect(followedJob([platform('c', 'running'), mine('b', 'queued')])).toBe('b')
  })

  it('keeps its job until it has shown it end', () => {
    // A newer job a person started waits for the followed one to end.
    expect(followedJob([mine('b', 'queued'), mine('a', 'running')], { id: 'a', done: false })).toBe('a')
    // The list can say the job ended before the panel has shown it.
    expect(followedJob([mine('b', 'running'), mine('a', 'succeeded')], { id: 'a', done: false })).toBe('a')
  })

  it('keeps an ended job on screen while only the platform starts jobs', () => {
    // site.wp_cron starting as site.create ends.
    expect(followedJob([platform('b', 'running'), mine('a', 'succeeded')], { id: 'a', done: true })).toBe('a')
  })

  it('moves to a newer job a person started once the followed one is done', () => {
    expect(followedJob([mine('b', 'queued'), mine('a', 'succeeded')], { id: 'a', done: true })).toBe('b')
    // Dismissed while running counts as done.
    expect(followedJob([mine('b', 'queued'), mine('a', 'running')], { id: 'a', done: true })).toBe('b')
  })

  it('does not go back to an older job', () => {
    expect(followedJob([mine('b', 'succeeded'), mine('a', 'running')], { id: 'b', done: true })).toBe('b')
    // A job no longer in the list is older than all of it.
    expect(followedJob([mine('c', 'running')], { id: 'a', done: true })).toBe('c')
  })
})
