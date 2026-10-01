import type { WatchSource } from 'vue'
import { toApiError, type ApiError } from '../utils/api-error'

interface Page<T> {
  items: T[]
  next_cursor: string | null
}

/**
 * A cursor-paginated list: the first page loads (and reloads when a watched
 * source changes) through `useAsyncData`; `loadMore()` appends the next one.
 * Pages are never loaded eagerly (ui/README "Data fetching").
 *
 *     const list = useCursorList('alerts', cursor => api.GET('/api/v1/alerts', { params: { query: { cursor } } }), [filters])
 *
 * `fetchPage` gets the cursor (undefined for the first page) and returns the
 * `{ data }` the API client resolves with.
 */
export function useCursorList<T>(
  key: string,
  fetchPage: (cursor: string | undefined) => Promise<{ data?: Page<T> }>,
  watchSources: WatchSource[] = [],
) {
  const notify = useNotify()
  const items = ref<T[]>([]) as Ref<T[]>
  const cursor = ref<string | null>(null)

  const { data: loaded, status, refresh } = useAsyncData(
    key,
    async () => {
      try {
        const { data } = await fetchPage(undefined)
        items.value = data?.items ?? []
        cursor.value = data?.next_cursor ?? null
        return { error: null as ApiError | null }
      } catch (e) {
        items.value = []
        cursor.value = null
        return { error: toApiError(e) }
      }
    },
    { watch: watchSources },
  )

  const loadError = computed(() => loaded.value?.error ?? undefined)
  const pending = computed(() => status.value === 'pending')

  const loadingMore = ref(false)
  async function loadMore() {
    if (!cursor.value || loadingMore.value) return
    loadingMore.value = true
    try {
      const { data } = await fetchPage(cursor.value)
      items.value.push(...(data?.items ?? []))
      cursor.value = data?.next_cursor ?? null
    } catch (e) {
      notify.error(e)
    } finally {
      loadingMore.value = false
    }
  }

  return { items, cursor, loadError, pending, loadingMore, loadMore, refresh }
}
