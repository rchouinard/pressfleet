<script setup lang="ts">
// A newly created token, shown once. The value lives only in this
// component's props: it is never put in storage, the URL or a log, and the
// caller drops it when the dialog closes. Copying uses the clipboard API
// (secure contexts); the field is also selectable for the manual route, and
// on plain HTTP, where `navigator.clipboard` is missing, that is the fallback.
import { ref } from 'vue'

const props = defineProps<{ token: string }>()

const copied = ref(false)
const failed = ref(false)
const field = ref<HTMLInputElement>()

async function copy() {
  failed.value = false
  try {
    await navigator.clipboard.writeText(props.token)
    copied.value = true
  } catch {
    // No clipboard access: select the text so Ctrl+C works.
    copied.value = false
    failed.value = true
    field.value?.select()
  }
}
</script>

<template>
  <div class="space-y-3">
    <div class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" role="note">
      <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <p>Copy this token now. It is shown only once and can't be retrieved later; if you lose it, revoke it and create a new one.</p>
    </div>
    <div class="flex flex-col gap-2 sm:flex-row">
      <input
        ref="field"
        :value="token"
        readonly
        spellcheck="false"
        autocomplete="off"
        aria-label="API token"
        class="min-w-0 flex-1 rounded-md border border-default bg-default px-3 py-2 font-mono text-sm outline-none focus-visible:ring-2 focus-visible:ring-primary"
        @focus="field?.select()"
      >
      <UButton icon="i-lucide-copy" data-testid="copy" @click="copy">Copy token</UButton>
    </div>
    <p class="min-h-5 text-sm" role="status" aria-live="polite">
      <template v-if="copied">Copied to the clipboard.</template>
      <template v-else-if="failed">Couldn't copy automatically. The token is selected: press Ctrl+C (Cmd+C on a Mac).</template>
    </p>
  </div>
</template>
