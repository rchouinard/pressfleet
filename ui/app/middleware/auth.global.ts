import { isNetworkError } from '../utils/notify'
import { loginLocation, safeRedirect } from '../utils/redirect'
import { minimumRole, type Action } from '../utils/permissions'
import { roleAtLeast } from '../utils/roles'
import { enrollmentRedirect } from '../utils/two-factor'

/**
 * Guards every route. The session is loaded once per page load (GET
 * /api/v1/auth/me), then:
 *
 * - signed-out users go to /login with the page they wanted as `redirect`
 *   (validated by `safeRedirect`; see utils/redirect.ts),
 * - signed-in users on /login go on to their `redirect` (or the dashboard),
 * - a session that must enroll a second factor (`enrollment_required`)
 *   sees only the setup page until it has one; the server refuses
 *   everything else anyway (see enrollmentRedirect),
 * - a page that sets `definePageMeta({ requires: 'user.view' })` is a 403
 *   for roles below that action's minimum (hiding the nav entry is not
 *   enough; people follow links),
 * - if the control plane can't be reached, the error page says so with a
 *   retry instead of a misleading login form.
 *
 * Public pages opt out with `definePageMeta({ public: true })`.
 */
export default defineNuxtRouteMiddleware(async (to) => {
  // The breadcrumb trail belongs to the page being left.
  useState('breadcrumbs', () => []).value = []

  const session = useSession()
  if (!session.loaded.value) {
    try {
      await session.refresh()
    } catch (e) {
      const error = toApiError(e)
      return abortNavigation(createError({
        status: isNetworkError(error) ? 503 : error.status || 500,
        statusText: isNetworkError(error) ? 'Control plane unreachable' : error.title,
        message: error.detail ?? error.title,
        fatal: true,
      }))
    }
  }

  const user = session.user.value
  if (to.meta.public === true) {
    if (user && to.path === '/login') return navigateTo(safeRedirect(to.query.redirect), { replace: true })
    return
  }
  if (!user) return navigateTo(loginLocation(to.fullPath), { replace: true })

  const enroll = enrollmentRedirect(user, to)
  if (enroll) return navigateTo(enroll, { replace: true })

  const requires = to.meta.requires as Action | undefined
  if (requires && !roleAtLeast(user.role, minimumRole(requires))) {
    return abortNavigation(createError({
      status: 403,
      statusText: 'Forbidden',
      message: 'Your role does not allow you to open this page.',
    }))
  }
})
