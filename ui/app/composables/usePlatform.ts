/**
 * The platform's connection details and build from `GET /api/v1/platform`,
 * shared by every caller. A failed lookup is `null`: what depends on it
 * (the SFTP card, the version) is hidden rather than breaking the page.
 */
export function usePlatform() {
  const api = useApi()
  return useAsyncData('platform', async () => {
    try {
      const { data } = await api.GET('/api/v1/platform')
      return data ?? null
    } catch {
      return null
    }
  })
}
