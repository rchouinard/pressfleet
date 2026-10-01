<script setup lang="ts">
// One alert, its labels, and which channels it was delivered to (or failed
// to be). Reachable from the alert history and delivery tables.
import { toApiError } from '../../utils/api-error'
import { alertStateView, formatWhen, severityView, type AlertDelivery } from '../../utils/alert'

const route = useRoute()
const id = computed(() => String(route.params.id))
const api = useApi()

useHead({ title: computed(() => `Alert ${id.value.slice(0, 8)}`) })
useBreadcrumbs(() => [{ label: 'Alerts', to: '/alerts' }, { label: id.value.slice(0, 8) }])

const { data, status } = useAsyncData(
  () => `alert-${id.value}`,
  async () => {
    try {
      const { data: alert } = await api.GET('/api/v1/alerts/{alert_id}', { params: { path: { alert_id: id.value } } })
      return { alert: alert ?? null, error: null }
    } catch (e) {
      return { alert: null, error: toApiError(e) }
    }
  },
  { watch: [id] },
)
const alert = computed(() => data.value?.alert ?? null)

const deliveries = useCursorList<AlertDelivery>(
  `alert-deliveries-${id.value}`,
  cursor => api.GET('/api/v1/alert-deliveries', { params: { query: { alert_id: id.value, limit: 25, cursor } } }),
  [id],
)

const labels = computed(() => Object.entries(alert.value?.labels ?? {}).sort(([a], [b]) => a.localeCompare(b)))
</script>

<template>
  <div class="space-y-6">
    <ProblemAlert v-if="data?.error" :error="data.error" />
    <p v-else-if="status === 'pending' && !alert" class="text-sm text-muted" role="status">Loading alert...</p>

    <template v-else-if="alert">
      <header class="space-y-2">
        <h1 class="text-2xl font-semibold tracking-tight">{{ alert.summary || alert.rule_name }}</h1>
        <div class="flex flex-wrap items-center gap-2">
          <AlertBadge :view="alertStateView(alert.state)" />
          <AlertBadge :view="severityView(alert.severity)" />
          <span class="text-sm text-muted">Rule: {{ alert.rule_name }}</span>
        </div>
        <p v-if="alert.description" class="max-w-prose text-sm">{{ alert.description }}</p>
      </header>

      <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
        <div><dt class="text-muted">Started</dt><dd><time :datetime="alert.started_at">{{ formatWhen(alert.started_at) }}</time></dd></div>
        <div><dt class="text-muted">Fired</dt><dd>{{ formatWhen(alert.fired_at) }}</dd></div>
        <div><dt class="text-muted">Resolved</dt><dd>{{ formatWhen(alert.resolved_at) }}</dd></div>
        <div><dt class="text-muted">Latest value</dt><dd>{{ alert.value }}</dd></div>
        <div>
          <dt class="text-muted">Site</dt>
          <dd>
            <NuxtLink v-if="alert.site_id" :to="`/sites/${alert.site_id}`" class="rounded font-mono text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">{{ alert.site_id }}</NuxtLink>
            <template v-else>Platform alert</template>
          </dd>
        </div>
      </dl>

      <section v-if="labels.length" aria-labelledby="labels-heading" class="space-y-2">
        <h2 id="labels-heading" class="font-medium">Labels</h2>
        <ul class="flex flex-wrap gap-2">
          <li v-for="[k, v] in labels" :key="k" class="rounded-md border border-default px-2 py-0.5 font-mono text-xs">{{ k }}={{ v }}</li>
        </ul>
      </section>

      <section aria-labelledby="deliveries-heading" class="space-y-3">
        <h2 id="deliveries-heading" class="font-medium">Notifications</h2>
        <ProblemAlert v-if="deliveries.loadError.value" :error="deliveries.loadError.value" />
        <template v-else>
          <p v-if="deliveries.pending.value && deliveries.items.value.length === 0" class="text-sm text-muted" role="status">Loading notifications...</p>
          <p v-else-if="deliveries.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
            No notifications were sent for this alert. Either it is still pending, or no enabled channel wanted it.
          </p>
          <AlertDeliveryTable v-else :deliveries="deliveries.items.value" caption="Notifications for this alert" />
          <div v-if="deliveries.cursor.value" class="flex justify-center">
            <UButton color="neutral" variant="outline" :loading="deliveries.loadingMore.value" @click="deliveries.loadMore()">Load more</UButton>
          </div>
        </template>
      </section>
    </template>
  </div>
</template>
