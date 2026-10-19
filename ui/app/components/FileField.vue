<script setup lang="ts">
// A labelled native file picker (one file) with its error. Native, because it
// is accessible and keyboard-friendly by default and supports very large
// files without reading them: the upload slices the File as it goes.
import { ref } from 'vue'
import { formatBytes } from '../utils/backup'
import FileFieldInput from './FileFieldInput.vue'

defineProps<{
  label: string
  accept?: string
  hint?: string
  error?: string
  disabled?: boolean
  file?: File
}>()
const emit = defineEmits<{ 'update:file': [file: File | undefined] }>()

const input = ref<InstanceType<typeof FileFieldInput>>()

/** Clears the picker (after a successful submit or a reset). */
function clear() {
  input.value?.clear()
  emit('update:file', undefined)
}
defineExpose({ clear })
</script>

<template>
  <UFormField :label="label" :description="hint" :error="error" required>
    <FileFieldInput ref="input" :accept="accept" :disabled="disabled" @change="emit('update:file', $event)" />
    <p v-if="file" class="mt-1 text-xs text-muted">{{ file.name }} ({{ formatBytes(file.size) }})</p>
  </UFormField>
</template>
