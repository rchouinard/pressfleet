import { describe, expect, it } from 'vitest'
import { ApiError } from '../app/utils/api-error'
import { isNetworkError, notificationFromError } from '../app/utils/notify'

function err(status: number, extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) {
  return new ApiError({ type: 'about:blank', title: 'Some Title', status, ...extra })
}

describe('notificationFromError', () => {
  it('shows the server title, detail and request ID', () => {
    const n = notificationFromError(err(409, { title: 'Conflict', detail: 'The site is busy.', request_id: 'req-1' }))
    expect(n.title).toBe('Conflict')
    expect(n.description).toBe('The site is busy.\nRequest ID: req-1')
    expect(n.requestId).toBe('req-1')
  })

  it('has no description when there is nothing to add', () => {
    expect(notificationFromError(err(404, { title: 'Not Found' })).description).toBeUndefined()
  })

  it('words 403 as a role problem, keeping the server detail', () => {
    const n = notificationFromError(err(403, { title: 'Forbidden', request_id: 'r' }))
    expect(n.title).toBe('Not allowed')
    expect(n.description).toContain('Your role does not allow this action.')
    expect(n.description).toContain('Request ID: r')
    expect(notificationFromError(err(403, { detail: 'Only admins may delete sites.' })).description).toContain('Only admins may delete sites.')
  })

  it('words 401, 429 and 5xx plainly', () => {
    expect(notificationFromError(err(401)).title).toBe('Signed out')
    expect(notificationFromError(err(429, { title: 'Too Many Requests' })).title).toBe('Too many requests')
    expect(notificationFromError(err(500, { title: 'Internal Server Error', request_id: 'x' })).description).toBe('Request ID: x')
  })

  it('explains an unreachable control plane', () => {
    const network = new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Network error', status: 0 })
    expect(isNetworkError(network)).toBe(true)
    expect(notificationFromError(network).title).toBe('Control plane unreachable')
  })

  it('does not leak the message of non-API errors', () => {
    const unexpected = new ApiError({ type: 'urn:pressfleet:ui:unexpected', title: 'Unexpected error', status: 0, detail: 'at Object.<anonymous> (stack)' })
    const n = notificationFromError(unexpected)
    expect(n.title).toBe('Something went wrong')
    expect(n.description).toBeUndefined()
  })
})
