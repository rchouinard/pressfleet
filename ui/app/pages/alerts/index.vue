<script setup lang="ts">
// Alert history: every incident, newest first, filterable by state, rule,
// client group, site and start date. `?site_id=` pre-filters to one site (a
// site page can link here). Most metrics have no data until the collectors
// land, so "no alerts" is the common state and says so.
import { ANY } from '../../utils/alert-channel'
import { dayAfter, dayStart, type Alert, type AlertState } from '../../utils/alert'

useHead({ title: 'Alerts' })
useBreadcrumbs([{ label: 'Alerts' }])

const api = useApi()
const route = useRoute()
const router = useRouter()
const { ruleItems, groupItems } = useAlertLookups()

const stateItems = [
  { label: 'Firing', value: 'firing' },
  { label: 'Resolved', value: 'resolved' },
  { label: 'Pending (not yet firing)', value: 'pending' },
  { label: 'Any state', value: ANY },
]
const state = ref<string>('firing')
const ruleId = ref<string>(ANY)
const groupId = ref<string>(ANY)
const from = ref('')
const to = ref('')
const siteId = computed(() => (typeof route.query.site_id === 'string' && route.query.site_id ? route.query.site_id : undefined))

const query = computed(() => ({
  limit: 25,
  state: state.value === ANY ? undefined : (state.value as AlertState),
  rule_id: ruleId.value === ANY ? undefined : ruleId.value,
  client_group_id: groupId.value === ANY ? undefined : groupId.value,
  site_id: siteId.value,
  since: dayStart(from.value),
  until: to.value ? dayAfter(to.value) : undefined,
}))

const list = useCursorList<Alert>(
  'alerts-list',
  cursor => api.GET('/api/v1/alerts', { params: { query: { ...query.value, cursor } } }),
  [query],
)

const filtered = computed(() => state.value !== 'firing' || ruleId.value !== ANY || groupId.value !== ANY || !!from.value || !!to.value || !!siteId.value)
function reset() {
  state.value = 'firing'
  ruleId.value = ANY
  groupId.value = ANY
  from.value = ''
  to.value = ''
  if (siteId.value) void router.replace({ query: { ...route.query, site_id: undefined } })
}
</script>

<template>
  <div class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">Alerts</h1>
    <AlertTabs />

    <form class="flex flex-wrap items-end gap-3" aria-label="Filter alerts" @submit.prevent>
      <UFormField label="State" name="state">
        <USelect v-model="state" :items="stateItems" class="w-52" />
      </UFormField>
      <UFormField label="Rule" name="rule">
        <USelect v-model="ruleId" :items="[{ label: 'Any rule', value: ANY }, ...ruleItems]" class="w-52" />
      </UFormField>
      <UFormField label="Client group" name="group">
        <USelect v-model="groupId" :items="[{ label: 'Any client group', value: ANY }, ...groupItems]" class="w-52" />
      </UFormField>
      <UFormField label="Started from" name="from">
        <UInput v-model="from" type="date" class="w-40" />
      </UFormField>
      <UFormField label="Started until" name="to">
        <UInput v-model="to" type="date" class="w-40" />
      </UFormField>
      <UButton v-if="filtered" color="neutral" variant="ghost" icon="i-lucide-x" @click="reset">Reset filters</UButton>
      <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="list.pending.value" @click="list.refresh()">Refresh</UButton>
    </form>
    <p v-if="siteId" class="text-sm text-muted">
      Showing alerts for site <span class="font-mono">{{ siteId.slice(0, 8) }}</span>.
    </p>

    <ProblemAlert v-if="list.loadError.value" :error="list.loadError.value" />

    <template v-else>
      <p v-if="list.pending.value && list.items.value.length === 0" class="text-sm text-muted" role="status">Loading alerts...</p>
      <p v-else-if="list.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        <template v-if="filtered">No alerts match these filters.</template>
        <template v-else>Nothing is firing. Alerts appear here when a rule's condition holds, once the metrics it watches are being collected.</template>
      </p>
      <AlertTable v-else :alerts="list.items.value" caption="Alerts, newest first" />

      <div v-if="list.cursor.value" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="list.loadingMore.value" @click="list.loadMore()">Load more</UButton>
      </div>
    </template>
  </div>
</template>
