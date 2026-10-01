<script setup lang="ts">
// The metrics screen shared by the site page (scope "site", needs `siteId`)
// and the platform page (scope "platform"). It reads the catalog, queries one
// group at a time so a failing or slow group doesn't blank the rest, and
// draws a card per metric. Collectors (#68 to #74) feed the data; until they
// do, every metric comes back empty and the page says so.
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  defaultLimits, defaultRangeInput, formatSpan, formatValue, isEmpty, isTimestampUnit, metricsRangeProblem, rangeQuery,
  resolveRange, sectionsFor, toChartData,
  type ChartRange, type MetricCatalog, type MetricDescriptor, type MetricResult, type MetricsRange, type MetricsScope, type RangeInput,
} from '../../utils/metrics'

const props = defineProps<{ scope: MetricsScope, siteId?: string }>()

const api = useApi()
const { groupItems, groupNames } = useClientGroupLookup()

const { data: loaded, refresh: reloadCatalog, status: catalogStatus } = await useAsyncData(
  'metrics-catalog',
  async () => {
    try {
      const { data } = await api.GET('/api/v1/metrics/catalog')
      return { catalog: data as MetricCatalog | undefined, error: null as ApiError | null }
    } catch (e) {
      return { catalog: undefined, error: toApiError(e) }
    }
  },
)
const catalogError = computed(() => loaded.value?.error ?? undefined)
const limits = computed(() => loaded.value?.catalog?.limits ?? defaultLimits)
const sections = computed(() => (loaded.value?.catalog ? sectionsFor(loaded.value.catalog, props.scope) : []))

const input = ref<RangeInput>(defaultRangeInput())
const clientGroup = ref('all')
const clientGroupItems = computed(() => [{ label: 'All client groups', value: 'all' }, ...groupItems.value])

const problem = computed(() =>
  input.value.customStart || input.value.customEnd ? metricsRangeProblem(input.value, new Date(), limits.value) : undefined,
)

type SectionState =
  | { status: 'loading' }
  | { status: 'ok', data: MetricsRange }
  | { status: 'error', error: ApiError }

const range = ref<ChartRange>()
const states = ref<Record<string, SectionState>>({})
// Each load gets a number; answers from an older load (the range changed
// meanwhile) are dropped so they can't overwrite newer data.
let generation = 0

async function loadSection(group: string, r: ChartRange, gen: number) {
  let state: SectionState
  try {
    const query = { group: [group], ...rangeQuery(r) }
    const { data } = props.scope === 'site'
      ? await api.GET('/api/v1/sites/{site_id}/metrics', { params: { path: { site_id: props.siteId! }, query } })
      : await api.GET('/api/v1/metrics/platform', {
        params: { query: { ...query, client_group_id: clientGroup.value === 'all' ? undefined : clientGroup.value } },
      })
    state = { status: 'ok', data: data as MetricsRange }
  } catch (e) {
    state = { status: 'error', error: toApiError(e) }
  }
  if (gen === generation) states.value = { ...states.value, [group]: state }
}

async function load() {
  const r = resolveRange(input.value, new Date(), limits.value)
  if (!r) return
  const gen = ++generation
  range.value = r
  states.value = Object.fromEntries(sections.value.map(s => [s.group, { status: 'loading' } as SectionState]))
  await Promise.all(sections.value.map(s => loadSection(s.group, r, gen)))
}

// Query once the catalog is there (it names the groups), and again when the
// platform's client group filter changes.
watch(() => sections.value.length, (n) => { if (n > 0) void load() }, { immediate: true })
watch(clientGroup, () => { void load() })

const busy = computed(() => Object.values(states.value).some(s => s.status === 'loading'))
const everyGroupAnswered = computed(() =>
  sections.value.length > 0 && sections.value.every(s => states.value[s.group]?.status === 'ok'))

function errorOf(group: string) {
  const s = states.value[group]
  return s?.status === 'error' ? s.error : undefined
}

function resultsOf(group: string) {
  const s = states.value[group]
  return s?.status === 'ok' ? s.data.metrics : []
}
const hasData = computed(() => sections.value.some(s => resultsOf(s.group).some(r => !isEmpty(r))))
const noDataAtAll = computed(() => everyGroupAnswered.value && !hasData.value)

interface Row { d: MetricDescriptor, result: MetricResult }

function viewOf(group: string) {
  const results = new Map(resultsOf(group).map(r => [r.id, r]))
  const sec = sections.value.find(s => s.group === group)!
  const withData: Row[] = []
  const empty: string[] = []
  for (const d of sec.metrics) {
    const result = results.get(d.id)
    if (result && !isEmpty(result)) withData.push({ d, result })
    else empty.push(d.title)
  }
  return {
    group,
    charts: withData.filter(r => !isTimestampUnit(r.d.unit)),
    dates: withData.filter(r => isTimestampUnit(r.d.unit)),
    empty,
  }
}

/** The newest value of each series of a timestamp metric, as a date. */
function latestDates(d: { title: string }, result: Parameters<typeof toChartData>[0]) {
  return toChartData(result, d.title, groupNames.value).series.map(s => ({
    label: s.label,
    value: formatValue([...s.values].reverse().find(v => v !== null), 'timestamp'),
  }))
}

const rangeText = computed(() => {
  const r = range.value
  if (!r) return ''
  return `${new Date(r.start * 1000).toLocaleString()} to ${new Date(r.end * 1000).toLocaleString()}, one point per ${formatSpan(r.step)}`
})
</script>

<template>
  <div class="space-y-4">
    <div v-if="catalogError" class="space-y-2">
      <ProblemAlert :error="catalogError" />
      <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-refresh-cw" @click="reloadCatalog()">Retry</UButton>
    </div>
    <div v-else-if="catalogStatus === 'pending' && sections.length === 0" class="space-y-3" role="status" aria-live="polite">
      <p class="text-sm text-muted">Loading metrics...</p>
      <USkeleton class="h-48 w-full" />
    </div>
    <template v-else>
      <div class="space-y-3">
        <MetricsRangePicker v-model="input" :problem="problem" :loading="busy" @apply="load" @refresh="load" />
        <UFormField v-if="scope === 'platform'" label="Backup repositories of" name="client_group" class="max-w-xs">
          <USelect v-model="clientGroup" :items="clientGroupItems" class="w-full" />
        </UFormField>
        <p v-if="rangeText" class="text-xs text-muted">{{ rangeText }}</p>
      </div>

      <div
        v-if="noDataAtAll"
        role="status"
        class="rounded-md border border-default bg-elevated p-4 text-sm"
      >
        <p class="font-medium">No metric data in this range.</p>
        <p class="text-muted">
          Nothing has reported yet. Metrics appear once the collectors are running; try a longer range if they started recently.
        </p>
      </div>

      <section v-for="sec in sections" :key="sec.group" class="space-y-3" :aria-labelledby="`metrics-${sec.group}`">
        <h2 :id="`metrics-${sec.group}`" class="text-lg font-semibold tracking-tight">{{ sec.title }}</h2>

        <div v-if="!states[sec.group] || states[sec.group]!.status === 'loading'" class="grid gap-3 md:grid-cols-2" role="status" aria-live="polite">
          <span class="sr-only">Loading {{ sec.title }} metrics</span>
          <USkeleton v-for="m in sec.metrics.slice(0, 2)" :key="m.id" class="h-48 w-full" />
        </div>

        <div v-else-if="states[sec.group]!.status === 'error'" class="space-y-2">
          <ProblemAlert v-if="errorOf(sec.group)" :error="errorOf(sec.group)!" />
          <UButton size="sm" color="neutral" variant="outline" icon="i-lucide-refresh-cw" @click="range && loadSection(sec.group, range, generation)">
            Retry
          </UButton>
        </div>

        <template v-else>
          <template v-for="view in [viewOf(sec.group)]" :key="view.group">
            <div v-if="view.charts.length" class="grid gap-3 md:grid-cols-2">
              <MetricsChart
                v-for="c in view.charts"
                :key="c.d.id"
                :descriptor="c.d"
                :result="c.result"
                :range="range!"
                :names="groupNames"
              />
            </div>
            <dl v-if="view.dates.length" class="grid gap-3 md:grid-cols-2">
              <div v-for="c in view.dates" :key="c.d.id" class="rounded-lg border border-default p-3">
                <dt class="text-sm font-semibold">{{ c.d.title }}</dt>
                <dd v-for="l in latestDates(c.d, c.result)" :key="l.label" class="text-sm">
                  <span v-if="l.label !== c.d.title" class="text-muted">{{ l.label }}: </span>{{ l.value }}
                </dd>
              </div>
            </dl>
            <p v-if="view.empty.length && !noDataAtAll" class="text-sm text-muted">
              No data in this range: {{ view.empty.join(', ') }}.
            </p>
          </template>
        </template>
      </section>

      <div v-if="!sections.length" class="text-sm text-muted">The metrics catalog has no entries for this page.</div>
    </template>
  </div>
</template>
