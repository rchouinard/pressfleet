// The e2e fixtures' reauthentication on demand (e2e/fixtures/reauth.ts),
// tested here because Playwright specs need a running platform.
import { describe, expect, it } from 'vitest'
import { fetchReauthenticating, REAUTHENTICATION_REQUIRED_PROBLEM_TYPE } from '../e2e/fixtures/reauth'

function problem(type: string): Response {
  return new Response(JSON.stringify({ type, title: 'Forbidden', status: 403 }), {
    status: 403,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

function request(): Request {
  return new Request('https://fleet.e2e.test/api/v1/users', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': 'csrf' },
    body: JSON.stringify({ email: 'a@e2e.test' }),
  })
}

describe('fetchReauthenticating', () => {
  it('reauthenticates once and replays the request, body included', async () => {
    const seen: { body: string, csrf: string | null }[] = []
    let reauths = 0
    const response = await fetchReauthenticating(request(), async (r) => {
      seen.push({ body: await r.text(), csrf: r.headers.get('X-CSRF-Token') })
      return seen.length === 1 ? problem(REAUTHENTICATION_REQUIRED_PROBLEM_TYPE) : new Response('{}', { status: 201 })
    }, async () => { reauths++ })
    expect(response.status).toBe(201)
    expect(reauths).toBe(1)
    expect(seen).toEqual([
      { body: '{"email":"a@e2e.test"}', csrf: 'csrf' },
      { body: '{"email":"a@e2e.test"}', csrf: 'csrf' },
    ])
  })

  it('passes other refusals through whole, without reauthenticating', async () => {
    let reauths = 0
    const response = await fetchReauthenticating(request(), async () => problem('about:blank'), async () => { reauths++ })
    expect(response.status).toBe(403)
    expect(((await response.json()) as { type: string }).type).toBe('about:blank')
    expect(reauths).toBe(0)
  })

  it('retries only once', async () => {
    let sent = 0
    const response = await fetchReauthenticating(request(), async () => {
      sent++
      return problem(REAUTHENTICATION_REQUIRED_PROBLEM_TYPE)
    }, async () => {})
    expect(response.status).toBe(403)
    expect(sent).toBe(2)
  })

  it('fails when reauthenticating fails', async () => {
    await expect(fetchReauthenticating(request(), async () => problem(REAUTHENTICATION_REQUIRED_PROBLEM_TYPE), async () => {
      throw new Error('wrong password')
    })).rejects.toThrow('wrong password')
  })
})
