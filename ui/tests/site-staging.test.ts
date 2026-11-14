import { afterEach, describe, expect, it, vi } from 'vitest'
import { computed, ref, watch, type WatchSource } from 'vue'
import { flushPromises } from '@vue/test-utils'
import { useSiteStaging } from '../app/composables/useSiteStaging'
import { toApiError } from '../app/utils/api-error'
import type { Site } from '../app/utils/site'
import { site, stagingSite } from './site-fixtures'

// A stand-in for Nuxt's useAsyncData that keeps what the page relies on: the
// handler runs at once and again whenever a `watch` source changes.
function fakeUseAsyncData<T>(_key: () => string, handler: () => Promise<T>, options: { watch?: WatchSource[] } = {}) {
  const data = ref<T>()
  const refresh = async () => {
    data.value = await handler()
  }
  watch(options.watch ?? [], () => void refresh())
  const first = refresh()
  return Object.assign(first.then(() => ({ data, refresh })), { data, refresh })
}

afterEach(() => vi.unstubAllGlobals())

describe('useSiteStaging', () => {
  function setup() {
    const get = vi.fn(async (_path: string, _init: unknown) => ({ data: { items: [stagingSite('st1', 'p1')] } }))
    vi.stubGlobal('useApi', () => ({ GET: get }))
    vi.stubGlobal('useAsyncData', fakeUseAsyncData)
    vi.stubGlobal('toApiError', toApiError)
    const id = ref('p1')
    const loaded = ref<Site | undefined>(stagingSite('old', 'zzz'))
    const staging = useSiteStaging(computed(() => id.value), computed(() => loaded.value))
    return { get, id, loaded, staging }
  }

  it('lists the staging sites once the production site has loaded, though the id did not change', async () => {
    const { get, loaded, staging } = setup()
    await staging
    // The route changed to a production site while the old (staging) site was
    // still the loaded one: nothing to fetch yet.
    expect(get).not.toHaveBeenCalled()
    expect(staging.data.value?.items).toEqual([])

    loaded.value = site({ id: 'p1' })
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/api/v1/sites', { params: { query: { parent_site_id: 'p1', limit: 100 } } })
    expect(staging.data.value?.items.map(s => s.id)).toEqual(['st1'])
  })

  it('does not fetch for a site other than the one in the route', async () => {
    const { get, loaded, id, staging } = setup()
    loaded.value = site({ id: 'p0' })
    await staging
    id.value = 'p1'
    await flushPromises()
    expect(get).not.toHaveBeenCalled()
    loaded.value = site({ id: 'p1' })
    await flushPromises()
    expect(get).toHaveBeenCalledTimes(1)
  })
})
