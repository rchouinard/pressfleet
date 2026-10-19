<script setup lang="ts">
// FileField's native file input. Inside a UFormField, useFormField gives it
// the field's input ID, which the field's <label for> points at, and the
// aria-describedby of its hint and error, as Nuxt UI's own inputs get. A
// native input placed in the field directly gets neither, so it would have
// no accessible name.
import { ref } from 'vue'
import { useFormField } from '@nuxt/ui/composables/useFormField'

defineProps<{ accept?: string, disabled?: boolean }>()
const emit = defineEmits<{ change: [file: File | undefined] }>()

const { id, ariaAttrs } = useFormField()
const input = ref<HTMLInputElement>()

function onChange(event: Event) {
  emit('change', (event.target as HTMLInputElement).files?.[0])
}

/** Clears the picker. */
function clear() {
  if (input.value) input.value.value = ''
}
defineExpose({ clear })
</script>

<template>
  <input
    :id="id"
    ref="input"
    type="file"
    :accept="accept"
    :disabled="disabled"
    v-bind="ariaAttrs"
    class="block w-full rounded-md border border-default bg-default text-sm text-default file:mr-3 file:cursor-pointer file:border-0 file:bg-elevated file:px-3 file:py-2 file:text-sm file:font-medium focus-visible:ring-2 focus-visible:ring-primary"
    @change="onChange"
  >
</template>
