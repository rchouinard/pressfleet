// The API as the specs use it for setup and for checking outcomes: the
// dashboard's own typed client (app/utils/api-client.ts) pointed at the
// control plane's published port, with a session from logging in. Calls
// resolve with { data } and throw ApiError otherwise, as in the app.
import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { createApiClient, type ApiClient } from '../../app/utils/api-client'
import { toApiError, type ApiError } from '../../app/utils/api-error'
import type { components } from '../../app/api/schema.gen'

export type Role = components['schemas']['Role']
export type User = components['schemas']['User']
export type Job = components['schemas']['Job']

/** Name of the session cookie (control-plane/internal/api/authn.go). */
export const SESSION_COOKIE = 'pressfleet_session'

/** An API client with the session it uses. */
export interface Api {
  client: ApiClient
  /** The session cookie's value, for signing a browser in (session.ts). */
  sessionToken?: string
  csrfToken?: string
}

/** A user the specs created, with the password they sign in with. */
export interface TestUser {
  id: string
  email: string
  password: string
  role: Role
}

/** An API client that sends the given headers on every request. */
function client(apiURL: string, headers: () => Record<string, string>, getCsrfToken?: () => string | undefined): ApiClient {
  return createApiClient({
    baseUrl: apiURL,
    getCsrfToken,
    fetch: (request) => {
      for (const [k, v] of Object.entries(headers())) request.headers.set(k, v)
      return fetch(request)
    },
  })
}

/** Logs in through the API and returns a client that uses the session. */
export async function login(apiURL: string, email: string, password: string): Promise<Api> {
  const api: Api = {
    client: client(apiURL, (): Record<string, string> => (api.sessionToken ? { Cookie: `${SESSION_COOKIE}=${api.sessionToken}` } : {}), () => api.csrfToken),
  }
  const res = await fetch(new URL('/api/v1/auth/login', apiURL), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!res.ok) throw new Error(`login as ${email}: status ${res.status}: ${await res.text()}`)
  const cookie = res.headers.getSetCookie().find(c => c.startsWith(`${SESSION_COOKIE}=`))
  api.sessionToken = cookie?.slice(SESSION_COOKIE.length + 1).split(';')[0]
  api.csrfToken = ((await res.json()) as components['schemas']['CurrentUser']).csrf_token ?? undefined
  if (!api.sessionToken || !api.csrfToken) throw new Error(`login as ${email}: no session cookie or CSRF token`)
  return api
}

/** A client that authenticates with an API token instead of a session. */
export function withToken(apiURL: string, token: string): ApiClient {
  return client(apiURL, () => ({ Authorization: `Bearer ${token}` }))
}

/**
 * Runs an API call that should fail and returns its ApiError, e.g. to check
 * that the server refuses what the UI hides from a role.
 */
export async function apiError(call: Promise<unknown>): Promise<ApiError> {
  try {
    await call
  } catch (e) {
    return toApiError(e)
  }
  throw new Error('the API call succeeded')
}

/** A unique name for a record, so parallel specs never collide. */
export function unique(prefix: string): string {
  return `${prefix} ${randomUUID().slice(0, 8)}`
}

/** A unique email address for a test user. */
export function uniqueEmail(prefix: string): string {
  return `${prefix}-${randomUUID().slice(0, 8)}@e2e.test`
}

/** A password long enough for the server's policy. */
export function randomPassword(): string {
  return randomBytes(18).toString('base64url')
}

/** Creates a user with the role through the API, as an admin. */
export async function createUser(admin: Api, role: Role, prefix: string = role): Promise<TestUser> {
  const password = randomPassword()
  const { data } = await admin.client.POST('/api/v1/users', { body: { email: uniqueEmail(prefix), password, role } })
  return { id: data!.id, email: data!.email, password, role }
}

/** Creates a client group through the API and returns it. */
export async function createClientGroup(api: Api, name = unique('E2E Group')) {
  const { data } = await api.client.POST('/api/v1/client-groups', { body: { name } })
  return data!
}

const terminal = new Set<Job['state']>(['succeeded', 'failed', 'rollback_failed'])

/** Polls a job through the API until it ends, and returns it. */
export async function waitForJob(api: Api, jobId: string, timeoutMs = 150_000): Promise<Job> {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const { data } = await api.client.GET('/api/v1/jobs/{job_id}', { params: { path: { job_id: jobId } } })
    if (terminal.has(data!.state)) return data!
    if (Date.now() > deadline) throw new Error(`job ${jobId} still ${data!.state} after ${timeoutMs} ms`)
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
}

/**
 * Uploads a small file in one chunk through the resumable-upload API
 * (POST /uploads, PATCH, complete) and returns the upload's ID. The
 * dashboard's own uploader (utils/upload.ts) is covered by its unit tests
 * and the import and deploy specs.
 */
export async function uploadFile(
  api: Api,
  apiURL: string,
  content: Uint8Array<ArrayBuffer>,
  filename: string,
  request: Pick<components['schemas']['UploadCreate'], 'purpose' | 'client_group_id' | 'site_id'>,
): Promise<string> {
  const sha256 = createHash('sha256').update(content).digest('hex')
  const { data: upload } = await api.client.POST('/api/v1/uploads', { body: { ...request, filename, size_bytes: content.byteLength, sha256 } })
  const res = await fetch(new URL(`/api/v1/uploads/${upload!.id}?offset=0`, apiURL), {
    method: 'PATCH',
    headers: {
      'Content-Type': 'application/octet-stream',
      'Cookie': `${SESSION_COOKIE}=${api.sessionToken}`,
      'X-CSRF-Token': api.csrfToken!,
    },
    body: new Blob([content]),
  })
  if (!res.ok) throw new Error(`upload chunk: status ${res.status}: ${await res.text()}`)
  await api.client.POST('/api/v1/uploads/{upload_id}/complete', { params: { path: { upload_id: upload!.id } }, body: { sha256 } })
  return upload!.id
}

/**
 * Starts a site import that fails at its validate step (the archive and
 * dump are random bytes) and creates no site, and returns its job.
 */
export async function startFailingImport(api: Api, apiURL: string, clientGroupId: string): Promise<Job> {
  const request = { purpose: 'site_import', client_group_id: clientGroupId } as const
  const archive = await uploadFile(api, apiURL, new Uint8Array(randomBytes(4096)), 'wp-content.tar.gz', request)
  const dump = await uploadFile(api, apiURL, new Uint8Array(randomBytes(4096)), 'database.sql', request)
  const { data } = await api.client.POST('/api/v1/sites/import', {
    body: { client_group_id: clientGroupId, archive_upload_id: archive, dump_upload_id: dump, name: unique('E2E Broken Import') },
  })
  return data!.job
}
