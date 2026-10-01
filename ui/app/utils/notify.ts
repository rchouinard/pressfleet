import type { ApiError } from './api-error'

/** What a toast needs to show an error; framework-free so it is testable. */
export interface ErrorNotification {
  title: string
  description?: string
  icon: string
  /** The request ID, also appended to the description for copying. */
  requestId?: string
}

/**
 * Turns an ApiError into toast content: a title that says what happened, the
 * server's detail, and the request ID operators quote when reporting an
 * issue. Auth, throttling and connectivity get plainer wording than the raw
 * Problem Details title.
 */
export function notificationFromError(error: ApiError): ErrorNotification {
  let title = error.title
  let detail = error.detail
  let icon = 'i-lucide-circle-alert'

  if (error.status === 0 && error.type === 'urn:pressfleet:ui:network') {
    title = 'Control plane unreachable'
    detail = 'Check your connection; the dashboard will keep trying.'
    icon = 'i-lucide-wifi-off'
  } else if (error.status === 0) {
    title = 'Something went wrong'
    detail = undefined
  } else if (error.status === 401) {
    title = 'Signed out'
    detail = 'Your session is no longer valid. Sign in again to continue.'
    icon = 'i-lucide-log-in'
  } else if (error.status === 403) {
    title = 'Not allowed'
    detail = error.detail || 'Your role does not allow this action.'
    icon = 'i-lucide-lock'
  } else if (error.status === 429) {
    title = 'Too many requests'
    icon = 'i-lucide-timer'
  } else if (error.status >= 500) {
    icon = 'i-lucide-server-crash'
  }

  const parts = [detail, error.requestId ? `Request ID: ${error.requestId}` : undefined].filter(Boolean)
  return {
    title,
    description: parts.length ? parts.join('\n') : undefined,
    icon,
    requestId: error.requestId,
  }
}

/** Whether an error means the control plane could not be reached at all. */
export function isNetworkError(error: ApiError): boolean {
  return error.status === 0 && error.type === 'urn:pressfleet:ui:network'
}
