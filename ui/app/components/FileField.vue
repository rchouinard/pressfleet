<script setup lang="ts">
// A labelled native file picker (one file) with its error. Native, because it
// is accessible and keyboard-friendly by default and supports very large
// files without reading them: the upload slices the File as it goes.
import { ref } from 'vue'
import { formatBytes } from '../utils/backup'

defineProps<{
  label: string
  accept?: string
  hint?: string
  error?: string
  disabled?: boolean
  file?: File
}>()
const emit = defineEmits<{ 'update:file': [file: File | undefined] }>()

const input = ref<HTMLInputElement>()

function onChange(event: Event) {
  emit('update:file', (event.target as HTMLInputElement).files?.[0])
}

/** Clears the picker (after a successful submit or a reset). */
function clear() {
  if (input.value) input.value.value = ''
  emit('update:file', undefined)
}
defineExpose({ clear })
</script>

<template>
  <UFormField :label="label" :description="hint" :error="error" required>
    <input
      ref="input"
      type="file"
      :accept="accept"
      :disabled="disabled"
      class="block w-full rounded-md border border-default bg-default text-sm text-default file:mr-3 file:cursor-pointer file:border-0 file:bg-elevated file:px-3 file:py-2 file:text-sm file:font-medium focus-visible:ring-2 focus-visible:ring-primary"
      @change="onChange"
    >
    <p v-if="file" class="mt-1 text-xs text-muted">{{ file.name }} ({{ formatBytes(file.size) }})</p>
  </UFormField>
</template>
