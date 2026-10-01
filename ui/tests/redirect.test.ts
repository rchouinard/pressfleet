import { describe, expect, it } from 'vitest'
import { loginLocation, safeRedirect } from '../app/utils/redirect'

describe('safeRedirect', () => {
  it.each([
    ['/', '/'],
    ['/jobs', '/jobs'],
    ['/jobs/abc?x=1&y=2#top', '/jobs/abc?x=1&y=2#top'],
    ['/sites/a%20b', '/sites/a%20b'],
  ])('keeps the same-origin path %s', (input, want) => {
    expect(safeRedirect(input)).toBe(want)
  })

  it.each([
    ['nothing', undefined],
    ['empty', ''],
    ['an array (repeated query param)', ['/jobs', '/sites']],
    ['a number', 5],
    ['an absolute URL', 'https://evil.example/path'],
    ['a protocol-relative URL', '//evil.example/path'],
    ['a backslash host', '/\\evil.example'],
    ['a slash-backslash host', '/\\/evil.example'],
    ['a tab-smuggled protocol-relative URL', '/\t/evil.example'],
    ['a newline', '/jobs\n/x'],
    ['a javascript: URL', 'javascript:alert(1)'],
    ['a data: URL', 'data:text/html,hi'],
    ['a relative path', 'jobs'],
    ['the login page', '/login'],
    ['the login page with a query', '/login?redirect=/jobs'],
  ])('falls back to the dashboard for %s', (_name, input) => {
    expect(safeRedirect(input)).toBe('/')
  })
})

describe('loginLocation', () => {
  it('carries the return path', () => {
    expect(loginLocation('/jobs/1?x=y')).toEqual({ path: '/login', query: { redirect: '/jobs/1?x=y' } })
  })

  it('omits the dashboard and unsafe targets, and can flag an expired session', () => {
    expect(loginLocation('/')).toEqual({ path: '/login', query: {} })
    expect(loginLocation('//evil.example')).toEqual({ path: '/login', query: {} })
    expect(loginLocation('/jobs', { expired: true })).toEqual({ path: '/login', query: { redirect: '/jobs', expired: '1' } })
  })
})
