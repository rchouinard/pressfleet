import type { components } from '../api/schema.gen'

export type Problem = components['schemas']['Problem']
export type FieldError = components['schemas']['FieldError']

/** Content type of RFC 9457 Problem Details documents. */
export const PROBLEM_CONTENT_TYPE = 'application/problem+json'

/**
 * An error response from the API (RFC 9457 Problem Details), or a request
 * that never got one: `status` is 0 and `type` is `urn:pressfleet:ui:network`
 * when the server could not be reached.
 */
export class ApiError extends Error {
  readonly status: number
  readonly type: string
  readonly title: string
  readonly detail: string | undefined
  readonly instance: string | undefined
  /** The request ID to quote when reporting an issue. */
  readonly requestId: string | undefined
  /** Per-field validation errors (`422` only). */
  readonly errors: readonly FieldError[]
  /** Seconds to wait before retrying, from a `429` or `503` (`Retry-After`). */
  readonly retryAfter: number | undefined
  /**
   * The whole problem document, extension members included, such as the
   * `methods` of a login that needs a second factor.
   */
  readonly members: Readonly<Record<string, unknown>>

  constructor(problem: Problem, options?: ErrorOptions) {
    super(problem.detail || problem.title, options)
    this.name = 'ApiError'
    this.status = problem.status
    this.type = problem.type
    this.title = problem.title
    this.detail = problem.detail
    this.instance = problem.instance
    this.requestId = problem.request_id
    this.errors = problem.errors ?? []
    this.retryAfter = problem.retry_after && problem.retry_after > 0 ? problem.retry_after : undefined
    this.members = { ...problem }
  }

  /** The validation message for a JSON Pointer field such as `/php_version`. */
  fieldError(field: string): string | undefined {
    return this.errors.find(e => e.field === field)?.detail
  }
}

/**
 * Parses a `Retry-After` header: a number of seconds or an HTTP date.
 * Returns whole seconds (at least 1), or undefined if the value is missing or
 * unusable.
 */
export function parseRetryAfter(value: string | null | undefined, now: Date = new Date()): number | undefined {
  const v = value?.trim()
  if (!v) return undefined
  if (/^\d+$/.test(v)) return Number(v) > 0 ? Number(v) : undefined
  const at = Date.parse(v)
  if (Number.isNaN(at)) return undefined
  const seconds = Math.ceil((at - now.getTime()) / 1000)
  return seconds > 0 ? seconds : undefined
}

/** Narrows an unknown thrown value to ApiError, wrapping anything else. */
export function toApiError(e: unknown): ApiError {
  if (e instanceof ApiError) return e
  return new ApiError(
    { type: 'urn:pressfleet:ui:unexpected', title: 'Unexpected error', status: 0, detail: e instanceof Error ? e.message : String(e) },
    { cause: e },
  )
}

const statusTitles: Record<number, string> = {
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  409: 'Conflict',
  422: 'Unprocessable Content',
  429: 'Too Many Requests',
  500: 'Internal Server Error',
  502: 'Bad Gateway',
  503: 'Service Unavailable',
  504: 'Gateway Timeout',
}

/**
 * Builds an ApiError from a non-2xx response. The control plane always sends
 * Problem Details, but a proxy in front of it (or a crash) may send anything,
 * so a body that isn't a problem document still produces a usable error.
 */
export async function problemFromResponse(response: Response): Promise<ApiError> {
  const requestId = response.headers.get('X-Request-ID') ?? undefined
  const contentType = response.headers.get('Content-Type') ?? ''
  let parsed: unknown
  if (contentType.includes('json')) {
    try {
      parsed = await response.json()
    } catch {
      parsed = undefined
    }
  }
  // The body's retry_after and the header carry the same delay; a proxy that
  // answers for the control plane may send only the header.
  const retryAfter = parseRetryAfter(response.headers.get('Retry-After'))
  if (isProblem(parsed)) {
    return new ApiError({ ...parsed, request_id: parsed.request_id ?? requestId, retry_after: parsed.retry_after ?? retryAfter })
  }
  return new ApiError({
    type: 'about:blank',
    title: statusTitles[response.status] ?? (response.statusText || `HTTP ${response.status}`),
    status: response.status,
    request_id: requestId,
    retry_after: retryAfter,
  })
}

function isProblem(v: unknown): v is Problem {
  if (typeof v !== 'object' || v === null) return false
  const p = v as Record<string, unknown>
  return typeof p.title === 'string' && typeof p.status === 'number'
}
