// Reauthenticating on demand (#154). Sensitive actions from a session whose
// last check is older than two_factor.reauthentication_max_age (10 minutes
// by default, which the e2e platform keeps) are refused with 403 and this
// problem type. The worker-scoped adminApi session outlives that, so the
// session clients login() returns reauthenticate with the password and send
// the request once more. Browser pages sign in fresh for each test; the
// dashboard's own dialog is #158.

/** The problem type of the refusal (docs/configuration.md#reauthenticating-for-sensitive-actions). */
export const REAUTHENTICATION_REQUIRED_PROBLEM_TYPE
  = 'https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#reauthenticating-for-sensitive-actions'

/** Whether a response refuses its request until the session reauthenticates. */
async function needsReauthentication(response: Response): Promise<boolean> {
  if (response.status !== 403) return false
  if (!response.headers.get('Content-Type')?.startsWith('application/problem+json')) return false
  const problem = await response.clone().json().catch(() => undefined) as { type?: string } | undefined
  return problem?.type === REAUTHENTICATION_REQUIRED_PROBLEM_TYPE
}

/**
 * Sends request; if the control plane asks for reauthentication, calls
 * reauthenticate and sends a copy of the request, body included, once more.
 */
export async function fetchReauthenticating(
  request: Request,
  send: (request: Request) => Promise<Response>,
  reauthenticate: () => Promise<void>,
): Promise<Response> {
  // Cloned before sending: sending consumes the body.
  const retry = request.clone()
  const response = await send(request)
  if (!(await needsReauthentication(response))) return response
  await reauthenticate()
  return send(retry)
}
