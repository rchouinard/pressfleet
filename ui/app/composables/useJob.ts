import type { MaybeRefOrGetter } from 'vue'
import type { ApiError } from '../utils/api-error'
import { createJobPoller, type JobPoller, type PollerStatus } from '../utils/job-poller'
import type { Job } from '../utils/job'

/**
 * Live state of one job. Polls `GET /api/v1/jobs/{id}` (there is no job
 * stream) with the backoff and pausing described in `utils/job-poller.ts`
 * and stops by itself at a terminal state, when the component unmounts, or
 * when the id changes.
 *
 * `error` holds the latest failed poll; while the poller is still retrying
 * (`retrying` is true) the last good `job` stays on screen.
 */
export function useJob(jobId: MaybeRefOrGetter<string>) {
  const api = useApi()
  const job = ref<Job>()
  const error = ref<ApiError>()
  const status = ref<PollerStatus>('idle')
  const retrying = ref(false)
  let poller: JobPoller | undefined

  function begin(id: string) {
    poller?.stop()
    job.value = undefined
    error.value = undefined
    retrying.value = false
    poller = createJobPoller({
      fetchJob: async (signal) => {
        const { data } = await api.GET('/api/v1/jobs/{job_id}', { params: { path: { job_id: id } }, signal })
        if (!data) throw new Error('The job response was empty.')
        return data
      },
      onJob: (j) => {
        job.value = j
        error.value = undefined
        retrying.value = false
      },
      onError: (e, info) => {
        error.value = e
        retrying.value = !info.fatal
      },
      onStatus: (s) => {
        status.value = s
      },
      isHidden: () => document.hidden,
    })
    poller.start()
  }

  function onVisibility() {
    if (!document.hidden) poller?.wake()
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    // `online` is a good moment to retry without waiting out the backoff.
    window.addEventListener('online', onVisibility)
    watch(() => toValue(jobId), begin, { immediate: true })
  })
  onBeforeUnmount(() => {
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('online', onVisibility)
    poller?.stop()
  })

  return {
    job,
    error,
    status,
    retrying,
    /** Polls again now (after an error, or on request). */
    refresh: () => poller?.wake(),
    /** Starts polling again after a fatal error. */
    restart: () => begin(toValue(jobId)),
  }
}
