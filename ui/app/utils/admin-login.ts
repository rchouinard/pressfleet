import type { components } from '../api/schema.gen'
import type { ApiError } from './api-error'
import type { Site } from './site'

export type AdminLogin = components['schemas']['AdminLogin']
export type SiteAdministrator = components['schemas']['SiteAdministrator']

/**
 * The problem type of the `409` that means "choose a user" (design doc §5.9):
 * the site has no usable default administrator. Other `409`s (the site isn't
 * running, or is in maintenance) are `about:blank`.
 */
export const CHOOSE_USER_PROBLEM_TYPE
  = 'https://github.com/rchouinard/pressfleet/blob/main/docs/pressfleet_architecture_and_implementation_document.md#59-dashboard-login-to-wordpress'

/** Whether a failed login asks the user to pick an administrator. */
export function isChooseUserError(e: ApiError): boolean {
  return e.status === 409 && e.type === CHOOSE_USER_PROBLEM_TYPE
}

/**
 * Why the login can't be used right now, or undefined when it can. The API
 * answers `409` in these states, so the button is disabled with the reason.
 */
export function adminLoginBlockedReason(site: Pick<Site, 'status' | 'maintenance'>): string | undefined {
  if (site.status !== 'running') return 'Start the site to log in to WordPress.'
  if (site.maintenance) return 'Turn off maintenance mode to log in to WordPress.'
  return undefined
}

/**
 * A note for when the login lands on the preview domain behind its password:
 * the browser asks for the preview credentials before WordPress sees the
 * login form. Undefined when it won't.
 */
export function previewCredentialsNote(site: Pick<Site, 'primary_domain' | 'preview_protection'>): string | undefined {
  if (site.primary_domain !== null || !site.preview_protection.enabled) return undefined
  return 'This site is not live and its preview is password protected, so your browser will ask for the preview credentials first.'
}

/** The label of an administrator in a list: login, then name and email when they differ. */
export function administratorLabel(a: Pick<SiteAdministrator, 'login' | 'display_name' | 'email'>): string {
  const extra = [a.display_name, a.email].filter(v => v !== '' && v !== a.login)
  return extra.length > 0 ? `${a.login} (${extra.join(', ')})` : a.login
}

/** The minimal window surface the login needs, so tests can fake it. */
export interface LoginTab {
  closed: boolean
  name: string
  close(): void
  opener: unknown
  document: Pick<Document, 'title' | 'body'>
}

let tabCounter = 0

/**
 * Opens the blank tab the login will land in. It must run synchronously in
 * the click handler: browsers block `window.open` after an `await`. Returns
 * null when a popup blocker refuses. (No `noopener` here, which would hide
 * the window from us; `opener` is cleared instead, since the tab ends up on
 * the site's origin.)
 */
export function openLoginTab(win: Pick<Window, 'open'> = window): LoginTab | null {
  const tab = win.open('', `pressfleet-wp-login-${++tabCounter}`, '') as LoginTab | null
  if (!tab) return null
  try {
    tab.opener = null
    tab.document.title = 'Logging in to WordPress'
    tab.document.body.textContent = 'Logging in to WordPress...'
  } catch {
    // Writing a placeholder is a nicety.
  }
  return tab
}

/**
 * Submits `token` to `url` as a hidden POST form targeted at `tab`, so the
 * token travels in a request body: never in a URL, history or a log. The
 * form is removed right after it is submitted.
 */
export function submitLoginForm(doc: Document, tab: Pick<LoginTab, 'name'>, login: Pick<AdminLogin, 'url' | 'token'>): void {
  const form = doc.createElement('form')
  form.method = 'POST'
  form.action = login.url
  form.target = tab.name
  form.hidden = true
  const input = doc.createElement('input')
  input.type = 'hidden'
  input.name = 'token'
  input.value = login.token
  form.append(input)
  doc.body.append(form)
  try {
    form.submit()
  } finally {
    form.remove()
  }
}

/** Closes a tab that was opened for a login that didn't happen. */
export function closeLoginTab(tab: LoginTab | null | undefined): void {
  try {
    if (tab && !tab.closed) tab.close()
  } catch {
    // Already gone.
  }
}
