<script setup lang="ts">
// New recovery codes, shown once: the list, a copy button for all of them,
// and a download as a text file. The codes live only in the props; the
// caller drops them when its dialog closes. Copying needs a secure context;
// otherwise the list can still be selected or downloaded.
import { computed, ref } from 'vue'
import { recoveryCodesHref, recoveryCodesText } from '../../utils/two-factor-settings'

const props = defineProps<{ codes: readonly string[], email: string, host: string }>()

const copyState = ref<'idle' | 'copied' | 'failed'>('idle')
const text = computed(() => recoveryCodesText(props.codes, { email: props.email, host: props.host }))
const href = computed(() => recoveryCodesHref(text.value))
const filename = computed(() => `pressfleet-recovery-codes-${props.host}.txt`)

async function copy() {
  try {
    await navigator.clipboard.writeText(props.codes.join('\n'))
    copyState.value = 'copied'
  } catch {
    copyState.value = 'failed'
  }
}
</script>

<template>
  <div class="space-y-3">
    <p class="text-sm">
      Each code signs you in once if you lose your second factors. Save them somewhere safe, like a password manager. <strong>They won't be shown again.</strong>
    </p>
    <ol aria-label="Recovery codes" class="grid grid-cols-1 gap-x-6 gap-y-1 rounded-md border border-default bg-elevated p-3 font-mono text-sm sm:grid-cols-2" data-testid="recovery-codes">
      <li v-for="code in codes" :key="code" class="select-all">{{ code }}</li>
    </ol>
    <div class="flex flex-wrap gap-2">
      <UButton color="neutral" variant="outline" :icon="copyState === 'copied' ? 'i-lucide-check' : 'i-lucide-copy'" data-testid="copy-codes" @click="copy">
        Copy codes
      </UButton>
      <a
        :href="href"
        :download="filename"
        class="inline-flex items-center gap-1.5 rounded-md border border-default px-2.5 py-1.5 text-sm font-medium outline-none hover:bg-elevated focus-visible:ring-2 focus-visible:ring-primary"
        data-testid="download-codes"
      >
        <UIcon name="i-lucide-download" class="size-4" aria-hidden="true" />
        Download
      </a>
    </div>
    <p class="min-h-4 text-xs" role="status" aria-live="polite">
      <template v-if="copyState === 'copied'">Copied.</template>
      <template v-else-if="copyState === 'failed'">Couldn't copy automatically. Select the codes and copy them, or download them.</template>
    </p>
  </div>
</template>
