/** Where the login page sends users who have no (valid) return path. */
export const DEFAULT_REDIRECT = '/'

const ORIGIN = 'http://pressfleet.invalid'

/**
 * Validates a post-login return path taken from the URL. Only same-origin
 * absolute paths survive; anything else (other origins, `//host`,
 * `/\host`, `javascript:`, control characters, the login page itself) falls
 * back to the dashboard, so `?redirect=` can't be used as an open redirect.
 */
export function safeRedirect(target: unknown): string {
  if (typeof target !== 'string' || target === '') return DEFAULT_REDIRECT
  // Must be a plain absolute path. Browsers treat "\" like "/", and control
  // characters (tab, newline) are stripped from URLs, so "/<tab>/host"
  // would become "//host".
  if (!target.startsWith('/') || target.startsWith('//')) return DEFAULT_REDIRECT
  // eslint-disable-next-line no-control-regex
  if (/[\\\u0000-\u001f\u007f]/.test(target)) return DEFAULT_REDIRECT

  let url: URL
  try {
    url = new URL(target, ORIGIN)
  } catch {
    return DEFAULT_REDIRECT
  }
  if (url.origin !== ORIGIN) return DEFAULT_REDIRECT
  if (url.pathname === '/login' || url.pathname.startsWith('/login/')) return DEFAULT_REDIRECT
  return url.pathname + url.search + url.hash
}

/** The login location that returns the user to `fullPath` afterwards. */
export function loginLocation(fullPath: string, options: { expired?: boolean } = {}) {
  const query: Record<string, string> = {}
  const redirect = safeRedirect(fullPath)
  if (redirect !== DEFAULT_REDIRECT) query.redirect = redirect
  if (options.expired) query.expired = '1'
  return { path: '/login', query }
}
