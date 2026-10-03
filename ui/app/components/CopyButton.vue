<script setup lang="ts">
// An icon button that copies a value, for values shown inline (an ID in a
// table row) where a CopyField would be too big. The icon turns into a
// check, and screen readers hear "Copied", once it worked. Copying needs a
// secure context; without one the button says so in its tooltip.
import { ref } from 'vue'

const props = defineProps<{ value: string, label: string }>()

const state = ref<'idle' | 'copied' | 'failed'>('idle')

async function copy() {
  try {
    await navigator.clipboard.writeText(props.value)
    state.value = 'copied'
  } catch {
    state.value = 'failed'
  }
}
</script>

<template>
  <span class="inline-flex items-center">
    <UButton
      color="neutral"
      variant="ghost"
      size="xs"
      :icon="state === 'copied' ? 'i-lucide-check' : 'i-lucide-copy'"
      :aria-label="`Copy ${label}`"
      :title="state === 'failed' ? `Couldn't copy; select the ${label} and copy it by hand` : `Copy ${label}`"
      data-testid="copy"
      @click="copy"
    />
    <span class="sr-only" role="status" aria-live="polite">{{ state === 'copied' ? 'Copied.' : state === 'failed' ? "Couldn't copy." : '' }}</span>
  </span>
</template>
