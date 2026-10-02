<script setup lang="ts">
// A read-only value with a copy button: SFTP details, a database session's
// credentials, the tunnel command. The value lives only in the props; the
// caller decides how long that is (secrets are dropped when their dialog
// closes). Copying needs a secure context; otherwise the text is selected so
// Ctrl+C works.
import { ref } from 'vue'

const props = defineProps<{ label: string, value: string, mono?: boolean }>()

const copied = ref(false)
const failed = ref(false)
const field = ref<HTMLInputElement>()

async function copy() {
  failed.value = false
  try {
    await navigator.clipboard.writeText(props.value)
    copied.value = true
  } catch {
    copied.value = false
    failed.value = true
    field.value?.select()
  }
}
</script>

<template>
  <div class="space-y-1">
    <label class="block text-sm text-muted">
      {{ label }}
      <span class="mt-1 flex gap-2">
        <input
          ref="field"
          :value="value"
          readonly
          spellcheck="false"
          autocomplete="off"
          class="min-w-0 flex-1 rounded-md border border-default bg-default px-3 py-2 text-sm text-default outline-none focus-visible:ring-2 focus-visible:ring-primary"
          :class="{ 'font-mono': mono !== false }"
          @focus="field?.select()"
        >
        <UButton color="neutral" variant="outline" icon="i-lucide-copy" :aria-label="`Copy ${label}`" data-testid="copy" @click="copy" />
      </span>
    </label>
    <p class="min-h-4 text-xs" role="status" aria-live="polite">
      <template v-if="copied">Copied.</template>
      <template v-else-if="failed">Couldn't copy automatically. It is selected: press Ctrl+C (Cmd+C on a Mac).</template>
    </p>
  </div>
</template>
