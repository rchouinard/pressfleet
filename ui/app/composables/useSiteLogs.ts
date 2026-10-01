import type { MaybeRefOrGetter } from 'vue'
import { toApiError, type ApiError } from '../utils/api-error'
import { LogBuffer } from '../utils/log-buffer'
import { createLogStream, type LogStream, type LogStreamState } from '../utils/log-stream'
import {
  defaultFilters,
  filterQuery,
  isHistory,
  rangeBounds,
  rangeProblem,
  streamUrl,
  type LogFilters,
  type SiteLogEntry,
} from '../utils/logs'

/** A line as the viewer keeps it: the entry plus a key that is unique for the buffer's lifetime. */
export type LogLine = SiteLogEntry & { key: number }

/** Most lines held in memory; the oldest are dropped past this while following. */
export const MAX_LINES = 5000
const TAIL = 200
const PAGE = 200
/** Stream entries are collected and shown in batches so a busy site doesn't re-render per line. */
const FLUSH_MS = 100
const FILTER_DEBOUNCE_MS = 350

/**
 * State and actions of the site log viewer: either the live stream (a tail
 * of the newest lines, then new ones as they are written) or a history
 * range paged backwards with the API's cursor. Either way the lines sit in
 * one bounded buffer, oldest first, and "load older" extends it upwards.
 *
 * Pausing closes the stream and resumes from the last line seen
 * (`Last-Event-ID`), so nothing is lost and nothing is buffered while the
 * user reads.
 */
export function useSiteLogs(siteId: MaybeRefOrGetter<string>) {
  const api = useApi()
  const filters = reactive<LogFilters>(defaultFilters())

  const buffer = new LogBuffer<LogLine>(MAX_LINES)
  const lines = shallowRef<readonly LogLine[]>([])
  const dropped = ref(0)
  const stream = ref<LogStreamState>({ phase: 'idle', failures: 0 })
  const paused = ref(false)
  const loading = ref(false)
  const loadingOlder = ref(false)
  const error = ref<ApiError>()
  const olderError = ref<ApiError>()
  const malformed = ref(0)
  /** The next older page, once one has been read; null when nothing older is known. */
  const cursor = ref<string | null>(null)
  /** Whether older lines may exist; true until a read says otherwise. */
  const hasOlder = ref(true)

  const live = computed(() => !isHistory(filters))
  const rangeError = computed(() => rangeProblem(filters))

  let nextKey = 1
  let pending: LogLine[] = []
  let flushTimer: ReturnType<typeof setTimeout> | undefined
  let current: LogStream | undefined
  // Bumped on every reload so a slow response for old filters is ignored.
  let generation = 0
  // The window a history read used, so older pages stay inside it as time passes.
  let bounds: { since?: string, until?: string } = {}

  function publish() {
    lines.value = buffer.items
    dropped.value = buffer.dropped
  }

  function flush() {
    if (flushTimer !== undefined) clearTimeout(flushTimer)
    flushTimer = undefined
    if (pending.length === 0) return
    const batch = pending
    pending = []
    buffer.append(batch)
    publish()
  }

  function enqueue(entry: SiteLogEntry) {
    pending.push({ ...entry, key: nextKey++ })
    flushTimer ??= setTimeout(flush, FLUSH_MS)
  }

  function teardown() {
    current?.stop()
    current = undefined
    if (flushTimer !== undefined) clearTimeout(flushTimer)
    flushTimer = undefined
    pending = []
  }

  function reset() {
    generation++
    teardown()
    buffer.clear()
    publish()
    error.value = undefined
    olderError.value = undefined
    malformed.value = 0
    cursor.value = null
    hasOlder.value = true
    bounds = {}
    paused.value = false
    stream.value = { phase: 'idle', failures: 0 }
  }

  async function checkSession(e: ApiError) {
    // The stream is read with fetch, outside the API client, so a 401 there
    // is confirmed with a cheap authenticated call whose 401 handling
    // (redirect to /login) is the client's.
    if (e.status !== 401) return
    try {
      await api.GET('/api/v1/auth/me')
    } catch {
      // The client has already handled it.
    }
  }

  function startStream() {
    const id = toValue(siteId)
    const snapshot = { ...filters, sources: [...filters.sources] }
    current = createLogStream({
      url: () => streamUrl(id, snapshot, TAIL),
      onEntry: enqueue,
      onMalformed: () => {
        malformed.value++
      },
      onState: (s) => {
        stream.value = s
        if (s.phase === 'failed' && s.error) void checkSession(s.error)
      },
    })
    current.start()
  }

  async function loadHistory(gen: number) {
    loading.value = true
    try {
      bounds = rangeBounds(filters, new Date())
      const { since, until } = bounds
      const { data } = await api.GET('/api/v1/sites/{site_id}/logs', {
        params: { path: { site_id: toValue(siteId) }, query: { ...filterQuery(filters), since, until, limit: PAGE } },
      })
      if (gen !== generation) return
      buffer.append((data?.items ?? []).map(e => ({ ...e, key: nextKey++ })))
      cursor.value = data?.next_cursor ?? null
      hasOlder.value = cursor.value !== null
      publish()
    } catch (e) {
      if (gen !== generation) return
      error.value = toApiError(e)
    } finally {
      if (gen === generation) loading.value = false
    }
  }

  /** (Re)starts from the current filters: the stream's tail or the newest page of a range. */
  function reload() {
    reset()
    if (rangeError.value) return
    const gen = generation
    if (live.value) {
      startStream()
    } else {
      void loadHistory(gen)
    }
  }

  /** Reads the next older page above the first line. */
  async function loadOlder() {
    if (loadingOlder.value || !hasOlder.value || buffer.free <= 0) return
    const gen = generation
    loadingOlder.value = true
    olderError.value = undefined
    try {
      const query = { ...filterQuery(filters), limit: Math.min(PAGE, Math.max(1, buffer.free)) }
      let extra: { since?: string, until?: string, cursor?: string } = {}
      if (cursor.value) {
        // A cursor continues below the page it came from but never past `since`.
        extra = { cursor: cursor.value, since: bounds.since }
      } else if (live.value) {
        // After the stream's tail there is no cursor yet: page below the oldest line shown.
        const first = buffer.items[0]
        if (!first) return
        extra = { until: first.time }
      } else {
        return
      }
      const { data } = await api.GET('/api/v1/sites/{site_id}/logs', {
        params: { path: { site_id: toValue(siteId) }, query: { ...query, ...extra } },
      })
      if (gen !== generation) return
      const items = data?.items ?? []
      const added = buffer.prepend(items.map(e => ({ ...e, key: nextKey++ })))
      cursor.value = data?.next_cursor ?? null
      hasOlder.value = cursor.value !== null
      // A page that didn't fit leaves lines unread: say there is more instead of skipping them.
      if (added < items.length) hasOlder.value = false
      publish()
    } catch (e) {
      if (gen === generation) olderError.value = toApiError(e)
    } finally {
      if (gen === generation) loadingOlder.value = false
    }
  }

  function pause() {
    if (!live.value || paused.value) return
    paused.value = true
    flush()
    current?.stop()
  }

  function resume() {
    if (!paused.value) return
    paused.value = false
    current?.start()
  }

  function clear() {
    pending = []
    buffer.clear()
    publish()
    // Cleared lines are not coming back from "load older": only newer ones follow.
    hasOlder.value = false
    cursor.value = null
  }

  /** Retries after the stream gave up, from the tail. */
  function retry() {
    reload()
  }

  // Filters apply after a pause so typing in the search box doesn't reconnect per key.
  let debounce: ReturnType<typeof setTimeout> | undefined
  function onFilters() {
    if (debounce !== undefined) clearTimeout(debounce)
    debounce = setTimeout(reload, FILTER_DEBOUNCE_MS)
  }

  function wake() {
    current?.wake()
  }
  function onVisibility() {
    if (!document.hidden) wake()
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('online', wake)
    watch(() => toValue(siteId), reload)
    watch(() => [filters.sources.join(','), filters.level, filters.q, filters.range, filters.customSince, filters.customUntil], onFilters)
    reload()
  })
  onBeforeUnmount(() => {
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('online', wake)
    if (debounce !== undefined) clearTimeout(debounce)
    generation++
    teardown()
  })

  return {
    filters,
    lines,
    dropped,
    stream,
    paused,
    loading,
    loadingOlder,
    error,
    olderError,
    malformed,
    hasOlder,
    live,
    rangeError,
    reload,
    loadOlder,
    pause,
    resume,
    clear,
    retry,
    /** Flushes lines the stream has delivered but the view hasn't shown yet (before a download). */
    flush,
  }
}
