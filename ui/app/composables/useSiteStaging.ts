import type { ComputedRef } from 'vue'
import { toApiError, type ApiError } from '../utils/api-error'
import { isStaging, type Site } from '../utils/site'

/**
 * A production site's staging sites (all pages: the delete dialog names
 * them). A page of results is enough to show; more than 100 is not expected.
 * Await the result, like `useAsyncData`.
 */
export function useSiteStaging(id: ComputedRef<string>, site: ComputedRef<Site | undefined>) {
  const api = useApi()
  return useAsyncData(
    () => `site-staging-${id.value}`,
    async () => {
      // The site loads after the route changes; until the loaded site is the
      // one in the route there is nothing to ask about (the watch below runs
      // this again when it arrives).
      if (!site.value || site.value.id !== id.value || isStaging(site.value)) return { items: [] as Site[], error: null as ApiError | null }
      try {
        const { data } = await api.GET('/api/v1/sites', { params: { query: { parent_site_id: id.value, limit: 100 } } })
        return { items: data?.items ?? [], error: null as ApiError | null }
      } catch (e) {
        return { items: [] as Site[], error: toApiError(e) }
      }
    },
    { watch: [id, () => site.value?.id] },
  )
}
