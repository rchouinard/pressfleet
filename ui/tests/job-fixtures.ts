import type { Job, JobStep } from '../app/utils/job'

export function step(overrides: Partial<JobStep> = {}): JobStep {
  return {
    name: 'create_database',
    state: 'pending',
    attempts: 0,
    started_at: null,
    finished_at: null,
    log: [],
    ...overrides,
  }
}

export function job(overrides: Partial<Job> = {}): Job {
  return {
    id: '01J0000000000000000000JOB1',
    type: 'site.create',
    target: { type: 'site', id: '01J00000000000000000000SITE' },
    state: 'running',
    attempts: 1,
    max_attempts: 3,
    result: {},
    steps: [],
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:05Z',
    started_at: '2026-10-01T10:00:01Z',
    finished_at: null,
    ...overrides,
  }
}
