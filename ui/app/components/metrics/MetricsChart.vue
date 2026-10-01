<script setup lang="ts">
// One metric as a time-series card: a uPlot canvas (client only), a table of
// latest/min/avg/max per series that doubles as the legend and as the
// accessible alternative to the canvas, and a hover read-out. A metric with
// no data shows an empty note instead of an empty axis.
import type uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import {
  chartSummary, formatTime, formatValue, seriesColor, seriesDashes, seriesStats, toChartData, yBounds,
  type ChartRange, type MetricDescriptor, type MetricResult,
} from '../../utils/metrics'

const props = defineProps<{
  descriptor: MetricDescriptor
  result: MetricResult
  range: ChartRange
  /** Client group names by ID, to label per-group series. */
  names?: Record<string, string>
}>()

const colorMode = useColorMode()
const dark = computed(() => colorMode.value === 'dark')

const data = computed(() => toChartData(props.result, props.descriptor.title, props.names))
const stats = computed(() => seriesStats(data.value))
const summary = computed(() => chartSummary(props.descriptor.title, props.descriptor.unit, stats.value))
const rangeSeconds = computed(() => props.range.end - props.range.start)

const host = ref<HTMLElement>()
const hoverIdx = ref<number | null>(null)
let plot: uPlot | undefined
let observer: ResizeObserver | undefined
let building = 0

const hover = computed(() => {
  const i = hoverIdx.value
  if (i === null || i >= data.value.times.length) return undefined
  return {
    time: new Date(data.value.times[i]! * 1000).toLocaleString(),
    values: data.value.series.map(s => ({ label: s.label, value: formatValue(s.values[i], props.descriptor.unit) })),
  }
})

function destroy() {
  plot?.destroy()
  plot = undefined
}

async function build() {
  const token = ++building
  destroy()
  const el = host.value
  if (!el || data.value.series.length === 0) return
  const { default: UPlot } = await import('uplot')
  // A newer build started (or the card unmounted) while the library loaded.
  if (token !== building || !host.value) return

  const text = getComputedStyle(el).color
  const grid = dark.value ? 'rgba(148, 163, 184, 0.2)' : 'rgba(100, 116, 139, 0.2)'
  const unit = props.descriptor.unit
  const bounds = yBounds(unit)
  const axis = { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } }

  plot = new UPlot({
    width: Math.max(el.clientWidth, 200),
    height: 180,
    legend: { show: false },
    cursor: { drag: { x: false, y: false }, points: { size: 6 } },
    scales: {
      x: { time: true, range: () => [props.range.start, props.range.end] },
      y: {
        range: (_u, min, max) => {
          if (bounds.min !== undefined && bounds.max !== undefined) return [bounds.min, bounds.max]
          const lo = min >= 0 ? 0 : min
          return [lo, max > lo ? max + (max - lo) * 0.08 : lo + 1]
        },
      },
    },
    axes: [
      { ...axis, values: (_u, vals) => vals.map(v => formatTime(v, rangeSeconds.value)), space: 90 },
      { ...axis, size: 70, values: (_u, vals) => vals.map(v => formatValue(v, unit)) },
    ],
    series: [
      {},
      ...data.value.series.map((s, i) => ({
        label: s.label,
        stroke: seriesColor(i, dark.value),
        width: 2,
        dash: seriesDashes[i % seriesDashes.length],
        spanGaps: false,
        points: { show: false },
      })),
    ],
    hooks: { setCursor: [(u) => { hoverIdx.value = u.cursor.idx ?? null }] },
  }, [data.value.times, ...data.value.series.map(s => s.values)] as uPlot.AlignedData, el)
}

onMounted(() => {
  void build()
  observer = new ResizeObserver(() => {
    if (plot && host.value) plot.setSize({ width: Math.max(host.value.clientWidth, 200), height: 180 })
  })
  if (host.value) observer.observe(host.value)
})
onBeforeUnmount(() => {
  building++
  observer?.disconnect()
  destroy()
})
watch([data, dark], () => { void build() })
</script>

<template>
  <section class="min-w-0 rounded-lg border border-default bg-default p-3 sm:p-4" :aria-label="descriptor.title">
    <header class="mb-2">
      <h3 class="text-sm font-semibold">{{ descriptor.title }}</h3>
      <p class="text-xs text-muted">{{ descriptor.description }}</p>
    </header>

    <p v-if="data.series.length === 0" class="py-6 text-center text-sm text-muted">
      No data in this range.
    </p>
    <template v-else>
      <div class="min-h-[2.5rem] text-xs text-muted" aria-hidden="true">
        <template v-if="hover">
          <span class="font-medium text-default">{{ hover.time }}</span>
          <span v-for="v in hover.values" :key="v.label" class="ml-3 whitespace-nowrap">{{ v.label }}: {{ v.value }}</span>
        </template>
        <template v-else>Hover over the chart to read values.</template>
      </div>
      <!-- The canvas is a picture to assistive technology; the table below carries the numbers. -->
      <div ref="host" class="w-full text-muted" role="img" :aria-label="summary" />

      <div class="mt-2 overflow-x-auto">
        <table class="w-full min-w-[20rem] text-left text-xs">
          <caption class="sr-only">{{ descriptor.title }}: latest, minimum, average and maximum per series</caption>
          <thead class="text-muted">
            <tr>
              <th scope="col" class="py-1 pr-2 font-medium">Series</th>
              <th scope="col" class="px-2 py-1 text-right font-medium">Latest</th>
              <th scope="col" class="px-2 py-1 text-right font-medium">Min</th>
              <th scope="col" class="px-2 py-1 text-right font-medium">Avg</th>
              <th scope="col" class="py-1 pl-2 text-right font-medium">Max</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(s, i) in stats" :key="s.label" class="border-t border-default">
              <th scope="row" class="py-1 pr-2 font-normal">
                <svg class="mr-1.5 inline-block align-middle" width="24" height="8" aria-hidden="true">
                  <line
                    x1="0" y1="4" x2="24" y2="4" stroke-width="2"
                    :stroke="seriesColor(i, dark)"
                    :stroke-dasharray="seriesDashes[i % seriesDashes.length]?.join(' ')"
                  />
                </svg>
                <span class="break-all">{{ s.label }}</span>
              </th>
              <td class="px-2 py-1 text-right tabular-nums">{{ formatValue(s.latest, descriptor.unit) }}</td>
              <td class="px-2 py-1 text-right tabular-nums">{{ formatValue(s.min, descriptor.unit) }}</td>
              <td class="px-2 py-1 text-right tabular-nums">{{ formatValue(s.avg, descriptor.unit) }}</td>
              <td class="py-1 pl-2 text-right tabular-nums">{{ formatValue(s.max, descriptor.unit) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>
