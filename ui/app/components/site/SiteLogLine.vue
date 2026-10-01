<script setup lang="ts">
// One runtime log line. The message is untrusted (it is whatever the site's
// containers wrote, including HTML and terminal escapes), so it is only ever
// interpolated as text, never with v-html. A very long line is cut at
// `maxRenderedChars` with a button to show the rest, so a single
// multi-megabyte line can't freeze the page. Plain markup, no Nuxt runtime,
// so it is unit-testable.
import { computed, ref } from 'vue'
import { truncateMessage, type SiteLogEntry } from '../../utils/logs'

const props = defineProps<{ entry: SiteLogEntry }>()
const expanded = ref(false)
const shown = computed(() => truncateMessage(props.entry.message, expanded.value))

const time = computed(() => {
  const d = new Date(props.entry.time)
  if (Number.isNaN(d.getTime())) return props.entry.time
  const ms = String(d.getMilliseconds()).padStart(3, '0')
  return `${d.toLocaleTimeString([], { hour12: false })}.${ms}`
})

const levelClass: Record<string, string> = {
  debug: 'text-dimmed',
  info: 'text-muted',
  warn: 'text-warning',
  error: 'text-error',
}
</script>

<template>
  <div class="flex flex-wrap gap-x-2 border-b border-default/40 py-0.5 sm:flex-nowrap" data-testid="log-line">
    <time :datetime="entry.time" :title="entry.time" class="shrink-0 text-dimmed">{{ time }}</time>
    <span class="w-11 shrink-0 font-semibold uppercase" :class="levelClass[entry.level]">{{ entry.level }}</span>
    <span class="w-14 shrink-0 text-dimmed">{{ entry.source }}</span>
    <span class="min-w-0 basis-full whitespace-pre-wrap [overflow-wrap:anywhere] sm:basis-auto sm:flex-1">{{ shown.text }}<template v-if="shown.hidden > 0">... <button
      type="button"
      class="rounded-sm text-primary underline underline-offset-2 focus-visible:outline-2 focus-visible:outline-primary"
      @click="expanded = true"
    >show {{ shown.hidden.toLocaleString() }} more characters</button></template></span>
  </div>
</template>
