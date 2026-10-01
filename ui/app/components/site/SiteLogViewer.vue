<script setup lang="ts">
// The runtime log viewer for one site: live tail or a history range, with
// source, level, text and time filters. See useSiteLogs for the data flow.
//
// Accessibility: the log box is `role="log"` with aria-live off, because
// announcing every line would drown a screen reader. The coarse status
// ("Live", "Paused", "Reconnecting") is the only live region, and it changes
// rarely. Auto-scroll follows new lines only while the view is at the
// bottom; scrolling up stops it, and "Jump to latest" restores it.
import { formatDownload, downloadName, logLevels, logSources, rangeOptions, type LogLevel, type LogSource, type RangeKind } from '../../utils/logs'

const props = defineProps<{
  siteId: string
  /** Used in the download's file name. */
  siteSlug?: string
}>()

const viewer = useSiteLogs(() => props.siteId)
const { filters, lines, stream, paused, live } = viewer

// A select can't hold "no value", so "any level" is a sentinel.
const ANY = 'any'
const levelItems = [{ label: 'Any level', value: ANY }, ...logLevels.map(l => ({ label: l.label, value: l.value }))]
const levelModel = computed({
  get: () => filters.level ?? ANY,
  set: (v: string) => {
    filters.level = v === ANY ? undefined : (v as LogLevel)
  },
})
const rangeModel = computed({
  get: () => filters.range,
  set: (v: RangeKind) => {
    filters.range = v
  },
})
function toggleSource(source: LogSource, on: boolean | 'indeterminate') {
  const set = new Set(filters.sources)
  if (on === true) set.add(source)
  else set.delete(source)
  filters.sources = logSources.map(s => s.value).filter(s => set.has(s))
}

// --- scrolling -------------------------------------------------------------

const box = ref<HTMLElement>()
// True while the view is at (or near) the bottom; flipped by the user's scrolling.
const pinned = ref(true)

function onScroll() {
  const e = box.value
  if (!e) return
  pinned.value = e.scrollHeight - e.scrollTop - e.clientHeight < 24
}
function scrollToEnd() {
  const e = box.value
  if (e) e.scrollTop = e.scrollHeight
  pinned.value = true
}

watch(
  () => lines.value,
  async () => {
    if (!pinned.value || paused.value) return
    await nextTick()
    scrollToEnd()
  },
  { flush: 'post' },
)

// Older lines go above the first one; keep what the user is reading where it is.
async function loadOlder() {
  const e = box.value
  const before = e?.scrollHeight ?? 0
  const top = e?.scrollTop ?? 0
  const wasPinned = pinned.value
  await viewer.loadOlder()
  await nextTick()
  if (e && !wasPinned) e.scrollTop = top + (e.scrollHeight - before)
}

function jumpToLatest() {
  scrollToEnd()
}

// --- status ----------------------------------------------------------------

const status = computed(() => {
  if (viewer.rangeError.value) return { label: 'Check the time range', icon: 'i-lucide-triangle-alert', color: 'warning' as const }
  if (!live.value) return { label: 'History', icon: 'i-lucide-history', color: 'neutral' as const }
  if (paused.value) return { label: 'Paused', icon: 'i-lucide-pause', color: 'neutral' as const }
  switch (stream.value.phase) {
    case 'live': return { label: 'Live', icon: 'i-lucide-radio', color: 'success' as const }
    case 'waiting': return { label: 'Reconnecting', icon: 'i-lucide-refresh-cw', color: 'warning' as const }
    case 'failed': return { label: 'Disconnected', icon: 'i-lucide-unplug', color: 'error' as const }
    default: return { label: 'Connecting', icon: 'i-lucide-loader-circle', color: 'neutral' as const }
  }
})

// A countdown for the reconnect, ticking only while one is pending.
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
watch(
  () => stream.value.phase === 'waiting',
  (waiting) => {
    if (ticker) clearInterval(ticker)
    ticker = undefined
    if (waiting) {
      now.value = Date.now()
      ticker = setInterval(() => {
        now.value = Date.now()
      }, 1000)
    }
  },
)
onBeforeUnmount(() => {
  if (ticker) clearInterval(ticker)
})
const retryIn = computed(() => Math.max(0, Math.ceil(((stream.value.retryAt ?? 0) - now.value) / 1000)))

// --- download --------------------------------------------------------------

function download() {
  viewer.flush()
  const blob = new Blob([formatDownload(lines.value)], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = downloadName(props.siteSlug ?? props.siteId, new Date())
  document.body.append(a)
  a.click()
  a.remove()
  // Revoked on the next turn so the browser has started the download.
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

const countLabel = computed(() => {
  const n = lines.value.length
  const base = `${n.toLocaleString()} ${n === 1 ? 'line' : 'lines'}`
  return viewer.dropped.value > 0 ? `${base} (${viewer.dropped.value.toLocaleString()} older dropped)` : base
})
const empty = computed(() => lines.value.length === 0 && !viewer.loading.value && !viewer.error.value && !viewer.rangeError.value)
</script>

<template>
  <section class="space-y-3" aria-label="Site logs">
    <form class="space-y-3 rounded-lg border border-default p-3" @submit.prevent="viewer.reload()">
      <div class="flex flex-wrap items-end gap-3">
        <UFormField label="Search" name="q" class="min-w-48 flex-1" help="Matches the whole line, ignoring case.">
          <UInput v-model="filters.q" type="search" maxlength="200" icon="i-lucide-search" placeholder="Search logs" class="w-full" />
        </UFormField>
        <UFormField label="Level" name="level">
          <USelect v-model="levelModel" :items="levelItems" class="w-48" />
        </UFormField>
        <UFormField label="Time range" name="range">
          <USelect v-model="rangeModel" :items="[...rangeOptions]" class="w-48" />
        </UFormField>
      </div>

      <div v-if="filters.range === 'custom'" class="flex flex-wrap items-start gap-3">
        <UFormField label="From" name="since">
          <UInput v-model="filters.customSince" type="datetime-local" />
        </UFormField>
        <UFormField label="Until" name="until" :error="viewer.rangeError.value">
          <UInput v-model="filters.customUntil" type="datetime-local" />
        </UFormField>
      </div>

      <fieldset class="flex flex-wrap items-center gap-x-4 gap-y-2">
        <legend class="mb-1 text-sm font-medium">Sources <span class="font-normal text-muted">(none selected shows all)</span></legend>
        <UCheckbox
          v-for="s in logSources"
          :key="s.value"
          :model-value="filters.sources.includes(s.value)"
          :label="s.label"
          :title="s.hint"
          @update:model-value="toggleSource(s.value, $event)"
        />
      </fieldset>
    </form>

    <div class="flex flex-wrap items-center justify-between gap-2">
      <div class="flex flex-wrap items-center gap-3 text-sm">
        <UBadge :color="status.color" variant="subtle" :icon="status.icon" data-testid="log-status">{{ status.label }}</UBadge>
        <span class="text-muted">{{ countLabel }}</span>
        <span v-if="stream.phase === 'waiting' && live" class="text-muted">
          Connection lost; retrying in {{ retryIn }}s (attempt {{ stream.failures }}).
        </span>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <UButton v-if="live && !paused" color="neutral" variant="outline" size="sm" icon="i-lucide-pause" :disabled="stream.phase === 'failed'" @click="viewer.pause()">Pause</UButton>
        <UButton v-if="live && paused" color="primary" variant="outline" size="sm" icon="i-lucide-play" @click="viewer.resume()">Resume</UButton>
        <UButton v-if="!live" color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="viewer.loading.value" @click="viewer.reload()">Refresh</UButton>
        <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-trash-2" :disabled="lines.length === 0" @click="viewer.clear()">Clear</UButton>
        <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-download" :disabled="lines.length === 0" @click="download()">Download</UButton>
      </div>
    </div>

    <!-- The one live region: the coarse status, not the lines. -->
    <p class="sr-only" role="status" aria-live="polite">{{ status.label }}</p>

    <ProblemAlert v-if="viewer.error.value" :error="viewer.error.value" />
    <div v-if="stream.phase === 'failed' && stream.error" class="space-y-2">
      <ProblemAlert :error="stream.error" />
      <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" @click="viewer.retry()">Reconnect</UButton>
    </div>

    <div class="relative">
      <div
        ref="box"
        role="log"
        tabindex="0"
        aria-live="off"
        aria-label="Runtime log lines, oldest first"
        class="h-[60vh] min-h-64 overflow-auto rounded-md border border-default bg-elevated/50 p-2 font-mono text-xs leading-5 focus-visible:outline-2 focus-visible:outline-primary"
        @scroll.passive="onScroll"
      >
        <div v-if="viewer.hasOlder.value && lines.length > 0" class="pb-2 text-center">
          <UButton color="neutral" variant="outline" size="xs" icon="i-lucide-chevrons-up" :loading="viewer.loadingOlder.value" @click="loadOlder()">Load older lines</UButton>
          <p v-if="viewer.olderError.value" class="mt-1 text-error" role="alert">{{ viewer.olderError.value.title }}: {{ viewer.olderError.value.detail ?? 'try again' }}</p>
        </div>
        <p v-else-if="lines.length > 0 && !viewer.hasOlder.value" class="pb-2 text-center text-dimmed">No older lines.</p>

        <SiteLogLine v-for="line in lines" :key="line.key" :entry="line" />

        <p v-if="viewer.loading.value && lines.length === 0" class="p-4 text-center text-muted">Loading logs...</p>
        <p v-else-if="empty" class="p-4 text-center text-muted">
          {{ live ? 'No log lines yet. New lines appear here as the site writes them.' : 'No log lines match this range and these filters.' }}
        </p>
      </div>
      <UButton
        v-if="live && !pinned && lines.length > 0 && !paused"
        class="absolute right-4 bottom-4 shadow-md"
        size="sm"
        icon="i-lucide-arrow-down-to-line"
        @click="jumpToLatest()"
      >
        Jump to latest
      </UButton>
    </div>
    <p v-if="viewer.malformed.value > 0" class="text-xs text-muted">{{ viewer.malformed.value }} unreadable stream {{ viewer.malformed.value === 1 ? 'event was' : 'events were' }} skipped.</p>
  </section>
</template>
