import { toApiError, type ApiError } from '../utils/api-error'
import type { PhpVersionCatalog } from '../utils/php'

/**
 * The PHP version catalog from `GET /api/v1/catalog/php-versions`: the
 * versions with their support status and end-of-life dates, the platform
 * default for new sites, and the failure if it couldn't be loaded. A failure
 * is kept as data (not thrown) so the page still renders; callers decide how
 * to degrade. `retry()` loads it again.
 */
export function usePhpCatalog() {
  const api = useApi()
  const { data, refresh } = useAsyncData('php-versions', async (): Promise<{ catalog?: PhpVersionCatalog, error?: ApiError }> => {
    try {
      const { data } = await api.GET('/api/v1/catalog/php-versions')
      return { catalog: data }
    } catch (e) {
      return { error: toApiError(e) }
    }
  })
  return {
    versions: computed(() => data.value?.catalog?.items ?? []),
    defaultVersion: computed(() => data.value?.catalog?.default),
    error: computed(() => data.value?.error),
    retry: () => refresh(),
  }
}

/**
 * Only the versions, for showing statuses. A failure leaves the list empty
 * rather than breaking the page: statuses then show as unknown, and the
 * selector offers only the site's current version (the server still
 * validates).
 */
export function usePhpVersions() {
  return usePhpCatalog().versions
}
