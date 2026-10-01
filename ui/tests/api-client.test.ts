import { describe, expect, it, vi } from 'vitest'
import { createApiClient } from '../app/utils/api-client'
import { ApiError, toApiError } from '../app/utils/api-error'

const base = 'http://pressfleet.test'

function problem(status: number, body: Record<string, unknown>, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/problem+json', ...headers },
  })
}

describe('createApiClient', () => {
  it('returns data for 2xx responses and sends same-origin credentials', async () => {
    const fetch = vi.fn(async (_req: Request) => Response.json({ status: 'ok' }))
    const api = createApiClient({ baseUrl: base, fetch })

    const { data } = await api.GET('/healthz')

    expect(data).toEqual({ status: 'ok' })
    const req = fetch.mock.calls[0]![0]
    expect(req.url).toBe(`${base}/healthz`)
    expect(req.credentials).toBe('same-origin')
  })

  it('throws an ApiError built from Problem Details', async () => {
    const api = createApiClient({
      baseUrl: base,
      fetch: async () =>
        problem(
          422,
          {
            type: 'about:blank',
            title: 'Unprocessable Content',
            status: 422,
            detail: 'The request failed validation.',
            instance: '/api/v1/sites',
            request_id: 'req-1',
            errors: [{ field: '/php_version', detail: 'unsupported' }],
          },
        ),
    })

    const err = await api.GET('/api/v1/auth/me').catch((e: unknown) => e)

    expect(err).toBeInstanceOf(ApiError)
    const e = err as ApiError
    expect(e.status).toBe(422)
    expect(e.title).toBe('Unprocessable Content')
    expect(e.detail).toBe('The request failed validation.')
    expect(e.message).toBe('The request failed validation.')
    expect(e.requestId).toBe('req-1')
    expect(e.fieldError('/php_version')).toBe('unsupported')
    expect(e.fieldError('/nope')).toBeUndefined()
  })

  it('falls back to the X-Request-ID header and the status for non-problem bodies', async () => {
    const api = createApiClient({
      baseUrl: base,
      fetch: async () => new Response('<html>bad gateway</html>', { status: 502, headers: { 'Content-Type': 'text/html', 'X-Request-ID': 'edge-7' } }),
    })

    const e = (await api.GET('/api/v1/auth/me').catch((x: unknown) => x)) as ApiError

    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(502)
    expect(e.title).toBe('Bad Gateway')
    expect(e.type).toBe('about:blank')
    expect(e.requestId).toBe('edge-7')
    expect(e.errors).toEqual([])
  })

  it('survives a malformed JSON error body', async () => {
    const api = createApiClient({
      baseUrl: base,
      fetch: async () => new Response('{nope', { status: 500, headers: { 'Content-Type': 'application/json' } }),
    })

    const e = (await api.GET('/api/v1/auth/me').catch((x: unknown) => x)) as ApiError

    expect(e.status).toBe(500)
    expect(e.title).toBe('Internal Server Error')
  })

  it('reports network failures as ApiError with status 0', async () => {
    const cause = new TypeError('Failed to fetch')
    const api = createApiClient({
      baseUrl: base,
      fetch: async () => {
        throw cause
      },
    })

    const e = (await api.GET('/healthz').catch((x: unknown) => x)) as ApiError

    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(0)
    expect(e.type).toBe('urn:pressfleet:ui:network')
    expect(e.cause).toBe(cause)
  })

  it('calls onUnauthorized for 401 only, and still throws', async () => {
    const onUnauthorized = vi.fn()
    let status = 401
    const api = createApiClient({
      baseUrl: base,
      onUnauthorized,
      fetch: async () => problem(status, { type: 'about:blank', title: 'Unauthorized', status }),
    })

    await expect(api.GET('/api/v1/auth/me')).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
    expect(onUnauthorized.mock.calls[0]![0].status).toBe(401)

    status = 403
    await expect(api.GET('/api/v1/auth/me')).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('sends the CSRF token on unsafe methods only', async () => {
    const seen: Array<[string, string | null]> = []
    const api = createApiClient({
      baseUrl: base,
      getCsrfToken: () => 'csrf-abc',
      fetch: async (req) => {
        seen.push([req.method, req.headers.get('X-CSRF-Token')])
        return new Response(null, { status: 204 })
      },
    })

    await api.GET('/api/v1/auth/me')
    await api.POST('/api/v1/auth/logout')

    expect(seen).toEqual([
      ['GET', null],
      ['POST', 'csrf-abc'],
    ])
  })
})

describe('toApiError', () => {
  it('passes ApiError through and wraps anything else', () => {
    const original = new ApiError({ type: 'about:blank', title: 'Not Found', status: 404 })
    expect(toApiError(original)).toBe(original)

    const wrapped = toApiError(new Error('boom'))
    expect(wrapped.status).toBe(0)
    expect(wrapped.detail).toBe('boom')
    expect(toApiError('str').detail).toBe('str')
  })
})
