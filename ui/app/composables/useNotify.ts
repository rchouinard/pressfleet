import type { Job } from '../utils/job'
import { jobTypeLabel } from '../utils/job'
import { toApiError } from '../utils/api-error'
import { notificationFromError } from '../utils/notify'

/**
 * Toasts for the whole app (the toast area is rendered by `<UApp>`).
 *
 *     const notify = useNotify()
 *     try { ... } catch (e) { notify.error(e) }       // ApiError with request ID
 *     notify.success('Cache purged')
 *     notify.jobStarted(data.job)                      // after a 202: "View progress"
 *
 * Errors that belong next to a form (422 field errors) should also be shown
 * there; use a toast for failures that have no better place.
 */
export function useNotify() {
  const toast = useToast()

  return {
    /** Shows any thrown value as an error toast, with the request ID for ApiErrors. */
    error(e: unknown) {
      const n = notificationFromError(toApiError(e))
      toast.add({
        title: n.title,
        description: n.description,
        icon: n.icon,
        color: 'error',
        duration: 10000,
        // Multi-line description (detail, then request ID).
        ui: { description: 'whitespace-pre-line' },
      })
    },
    success(title: string, description?: string) {
      toast.add({ title, description, icon: 'i-lucide-circle-check', color: 'success' })
    },
    info(title: string, description?: string) {
      toast.add({ title, description, icon: 'i-lucide-info', color: 'info' })
    },
    /**
     * The pattern for an operation that returned `202 Accepted` with a job:
     * say it started and offer the progress page.
     */
    jobStarted(job: Pick<Job, 'id' | 'type'>, title?: string) {
      toast.add({
        title: title ?? `${jobTypeLabel(job.type)} started`,
        description: 'It runs in the background.',
        icon: 'i-lucide-list-checks',
        color: 'info',
        duration: 10000,
        actions: [{ label: 'View progress', color: 'neutral', variant: 'outline', onClick: () => void navigateTo(`/jobs/${job.id}`) }],
      })
    },
  }
}
