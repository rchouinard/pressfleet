<script setup lang="ts">
// A confirmation for destructive actions, used by every "delete X" button.
// The caller owns the request: it listens for `confirm`, sets `busy` while
// the request runs, passes the failure as `error` (the dialog stays open and
// shows the server's reason, request ID included), and closes it on success.
import type { ApiError } from '../utils/api-error'

defineProps<{
  title: string
  description?: string
  confirmLabel?: string
  busy?: boolean
  /** The action can't go ahead; the slot says why. */
  confirmDisabled?: boolean
  error?: ApiError
}>()
const open = defineModel<boolean>('open', { required: true })
defineEmits<{ confirm: [] }>()
</script>

<template>
  <UModal v-model:open="open" :title="title" :description="description" :dismissible="!busy">
    <template #body>
      <slot />
      <ProblemAlert v-if="error" :error="error" :class="{ 'mt-3': $slots.default }" />
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton color="error" :loading="busy" :disabled="confirmDisabled" @click="$emit('confirm')">{{ confirmLabel ?? 'Delete' }}</UButton>
      </div>
    </template>
  </UModal>
</template>
