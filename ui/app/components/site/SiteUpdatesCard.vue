<script setup lang="ts">
// Pending plugin and theme updates for one site (design doc §9 Q2), as of
// its latest update check, and "Check now" for after someone has updated
// it. Report only: WordPress applies the updates (auto-updates or wp-admin),
// so there is deliberately no update button here. The site page passes the
// site; a finished check changes its counts' checked_at, which reloads this.
import { toApiError, type ApiError } from '../../utils/api-error'
import { pendingUpdatesText, type PendingUpdate, type SiteUpdates } from '../../utils/site-updates'
import type { Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()
const emit = defineEmits<{ changed: [] }>()
const api = useApi()
const notify = useNotify()

const { data: loaded, refresh } = await useAsyncData(
  () => `site-updates-${props.site.id}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/sites/{site_id}/updates', { params: { path: { site_id: props.site.id } } })
      return { updates: data ?? null, error: null as ApiError | null }
    } catch (e) {
      return { updates: null, error: toApiError(e) }
    }
  },
)
// The site's counts change when a check finishes; reload the details then.
watch(() => [props.site.id, props.site.pending_updates?.checked_at], () => refresh())

const updates = computed<SiteUpdates | undefined>(() => loaded.value?.updates ?? undefined)
const loadError = computed(() => loaded.value?.error ?? undefined)
const groups = computed<{ title: string, items: PendingUpdate[] }[]>(() => {
  const u = updates.value
  if (!u) return []
  return [{ title: 'Plugins', items: u.plugins }, { title: 'Themes', items: u.themes }].filter(g => g.items.length > 0)
})

const checking = ref(false)
const failure = ref<ApiError>()
async function checkNow() {
  checking.value = true
  failure.value = undefined
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/updates/check', { params: { path: { site_id: props.site.id } } })
    if (data) notify.jobStarted(data.job, 'Update check started')
    // The page follows the job and reloads the site when it ends, which
    // reloads this card.
    emit('changed')
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    checking.value = false
  }
}

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="site-updates-heading">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h2 id="site-updates-heading" class="text-lg font-semibold">Plugin and theme updates</h2>
      <IfCan action="site.update">
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="outline"
          size="sm"
          :loading="checking"
          data-testid="updates-check"
          @click="checkNow()"
        >
          Check now
        </UButton>
      </IfCan>
    </div>
    <ProblemAlert v-if="loadError" :error="loadError" />
    <p v-else-if="!updates" class="text-sm text-muted" role="status">Loading updates...</p>
    <template v-else>
      <p v-if="!updates.checked_at" class="text-sm text-muted" data-testid="updates-unchecked">
        Not checked yet. Sites are checked twice a day while they run.
      </p>
      <template v-else>
        <p v-if="groups.length === 0" class="flex items-center gap-2 text-sm" data-testid="updates-none">
          <UIcon name="i-lucide-circle-check" class="size-4 text-success" aria-hidden="true" />
          Every plugin and theme is up to date.
        </p>
        <template v-else>
          <p class="flex items-start gap-2 text-sm" role="status" data-testid="updates-summary">
            <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0 text-warning" aria-hidden="true" />
            <span>
              {{ pendingUpdatesText({ plugins: updates.plugins.length, themes: updates.themes.length, checked_at: updates.checked_at }) }}
              {{ updates.plugins.length + updates.themes.length === 1 ? 'has an update' : 'have updates' }} available.
              WordPress applies them, with auto-updates or from wp-admin; the platform only reports them.
            </span>
          </p>
          <div v-for="g in groups" :key="g.title" class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="`${g.title} with updates`">
            <table class="w-full min-w-[28rem] text-left text-sm">
              <caption class="sr-only">{{ g.title }} with updates available</caption>
              <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th scope="col" class="px-3 py-2 font-medium">{{ g.title === 'Plugins' ? 'Plugin' : 'Theme' }}</th>
                  <th scope="col" class="px-3 py-2 font-medium">Installed</th>
                  <th scope="col" class="px-3 py-2 font-medium">Available</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-default">
                <tr v-for="u in g.items" :key="u.name" data-testid="updates-row">
                  <th scope="row" class="px-3 py-2 font-medium">
                    {{ u.title || u.name }}
                    <span v-if="u.title && u.title !== u.name" class="block text-xs font-normal text-muted">{{ u.name }}</span>
                  </th>
                  <td class="px-3 py-2">{{ u.version }}</td>
                  <td class="px-3 py-2">{{ u.update_version }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
        <p class="text-xs text-muted">Last checked {{ when(updates.checked_at) }}.</p>
      </template>
      <ProblemAlert v-if="failure" :error="failure" />
    </template>
  </section>
</template>
