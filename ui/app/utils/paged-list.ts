import { ref, shallowRef } from 'vue'
import { toApiError, type ApiError } from './api-error'

/** One page of a cursor-paginated list endpoint. */
export interface Page<T> {
  items: readonly T[]
  next_cursor: string | null
}

/**
 * State for a cursor-paginated list: load the first page, then append later
 * pages on demand ("Load more"), never all of them eagerly. `fetchPage`
 * wraps one `api.GET` call and returns its `data`. A failed first page sets
 * `error` (render it in place); a failed later page throws out of
 * `loadMore()` so the caller can toast it and keep what is already shown.
 *
 * Plain Vue refs and no Nuxt runtime, so it is unit-tested with a fake
 * `fetchPage`.
 */
export function createPagedList<T>(fetchPage: (cursor: string | undefined) => Promise<Page<T> | undefined>) {
  const items = shallowRef<T[]>([])
  const cursor = ref<string | null>(null)
  const loading = ref(false)
  const loadingMore = ref(false)
  const error = ref<ApiError>()
  // A reload that starts while an older one is in flight wins.
  let generation = 0

  /** (Re)loads the first page, replacing what is shown. */
  async function load() {
    const mine = ++generation
    loading.value = true
    try {
      const page = await fetchPage(undefined)
      if (mine !== generation) return
      items.value = [...(page?.items ?? [])]
      cursor.value = page?.next_cursor ?? null
      error.value = undefined
    } catch (e) {
      if (mine !== generation) return
      items.value = []
      cursor.value = null
      error.value = toApiError(e)
    } finally {
      if (mine === generation) loading.value = false
    }
  }

  /** Appends the next page. Throws on failure; the list is left as it was. */
  async function loadMore() {
    if (!cursor.value || loadingMore.value || loading.value) return
    const mine = generation
    loadingMore.value = true
    try {
      const page = await fetchPage(cursor.value)
      if (mine !== generation) return
      items.value = [...items.value, ...(page?.items ?? [])]
      cursor.value = page?.next_cursor ?? null
    } finally {
      loadingMore.value = false
    }
  }

  return { items, cursor, loading, loadingMore, error, load, loadMore }
}
