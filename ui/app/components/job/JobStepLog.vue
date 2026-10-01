<script setup lang="ts">
// One step's log: monospace, whitespace preserved, rendered strictly as text
// (log lines come from site containers and are untrusted; there is no v-html
// anywhere in the job components). While the step runs the view follows the
// newest line, unless the user scrolled up to read, in which case it stays
// where they are until they scroll back to the bottom.
import { nextTick, ref, watch } from 'vue'
import type { JobLogEntry } from '../../utils/job'

const props = defineProps<{
  entries: readonly JobLogEntry[]
  /** Follow new lines (the step is running). */
  follow: boolean
  label: string
}>()

const el = ref<HTMLElement>()
// True while the view is at (or near) the bottom; flipped by the user's scrolling.
const pinned = ref(true)

function onScroll() {
  const e = el.value
  if (!e) return
  pinned.value = e.scrollHeight - e.scrollTop - e.clientHeight < 12
}

function scrollToEnd() {
  const e = el.value
  if (e) e.scrollTop = e.scrollHeight
}

watch(
  () => props.entries.length,
  async () => {
    if (!props.follow || !pinned.value) return
    await nextTick()
    scrollToEnd()
  },
  { flush: 'post' },
)

function time(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleTimeString([], { hour12: false })
}

const levelClass: Record<string, string> = {
  info: 'text-muted',
  warn: 'text-warning',
  error: 'text-error',
}
</script>

<template>
  <div
    v-if="entries.length"
    ref="el"
    role="log"
    tabindex="0"
    :aria-label="label"
    aria-live="off"
    class="max-h-72 overflow-auto rounded-md border border-default bg-elevated/50 p-2 font-mono text-xs leading-5 focus-visible:outline-2 focus-visible:outline-primary"
    @scroll.passive="onScroll"
  >
    <div v-for="(entry, i) in entries" :key="i" class="flex gap-2">
      <time :datetime="entry.time" class="shrink-0 text-dimmed">{{ time(entry.time) }}</time>
      <span class="w-10 shrink-0 font-semibold uppercase" :class="levelClass[entry.level]">{{ entry.level }}</span>
      <span class="min-w-0 whitespace-pre-wrap break-words">{{ entry.message }}</span>
    </div>
  </div>
  <p v-else class="text-xs text-muted">No log output.</p>
</template>
