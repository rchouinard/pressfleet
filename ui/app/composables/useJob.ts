import type { MaybeRefOrGetter } from 'vue'
import type { ApiError } from '../utils/api-error'
import { createJobPoller, type PollerStatus } from '../utils/job-poller'
import { createJobStream, type JobSource } from '../utils/job-stream'
import type { Job } from '../utils/job'

/**
 * Live state of one job. Follows the job's event stream
 * (`GET /api/v1/jobs/{id}/events`, see `utils/job-stream.ts`) and, if the
 * stream can't be used (a proxy that buffers it, say), falls back to
 * polling `GET /api/v1/jobs/{id}` (`utils/job-poller.ts`). Either stops by
 * itself at a terminal state, when the component unmounts, or when the id
 * changes; either rests while the tab is hidden.
 *
 * `error` holds the latest failure; while it is being retried (`retrying`
 * is true) the last good `job` stays on screen.
 */
export function useJob(jobId: MaybeRefOrGetter<string>) {
  const api = useApi()
  const job = ref<Job>()
  const error = ref<ApiError>()
  const status = ref<PollerStatus>('idle')
  const retrying = ref(false)
  let source: JobSource | undefined

  const handlers = {
    onJob: (j: Job) => {
      job.value = j
      error.value = undefined
      retrying.value = false
    },
    onError: (e: ApiError, info: { fatal: boolean }) => {
      error.value = e
      retrying.value = !info.fatal
    },
    onStatus: (s: PollerStatus) => {
      status.value = s
    },
    isHidden: () => document.hidden,
  }

  function poll(id: string): JobSource {
    return createJobPoller({
      ...handlers,
      fetchJob: async (signal) => {
        const { data } = await api.GET('/api/v1/jobs/{job_id}', { params: { path: { job_id: id } }, signal })
        if (!data) throw new Error('The job response was empty.')
        return data
      },
    })
  }

  function begin(id: string) {
    source?.stop()
    job.value = undefined
    error.value = undefined
    retrying.value = false
    const stream = createJobStream({
      ...handlers,
      url: `/api/v1/jobs/${encodeURIComponent(id)}/events`,
      onFallback: () => {
        if (source !== stream) return
        source = poll(id)
        source.start()
      },
    })
    source = stream
    source.start()
  }

  function onVisibility() {
    if (document.hidden) source?.pause?.()
    else source?.wake()
  }

  function onOnline() {
    // A good moment to retry without waiting out the backoff.
    source?.wake()
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('online', onOnline)
    watch(() => toValue(jobId), begin, { immediate: true })
  })
  onBeforeUnmount(() => {
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('online', onOnline)
    source?.stop()
  })

  return {
    job,
    error,
    status,
    retrying,
    /** Checks again now (after an error, or on request). */
    refresh: () => source?.wake(),
    /** Starts following again after a fatal error. */
    restart: () => begin(toValue(jobId)),
  }
}
