<script setup lang="ts">
// Renders an ApiError for the user: what went wrong, per-field validation
// messages, and the request ID to quote when reporting it. Plain markup (no
// Nuxt runtime) so it is unit-testable.
import type { ApiError } from '../utils/api-error'

defineProps<{ error: ApiError }>()
</script>

<template>
  <div
    role="alert"
    class="rounded-md border border-red-300 bg-red-50 p-4 text-sm text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
  >
    <p class="font-medium">{{ error.title }}</p>
    <p v-if="error.detail">{{ error.detail }}</p>
    <ul v-if="error.errors.length" class="mt-2 list-disc pl-5">
      <li v-for="e in error.errors" :key="e.field">
        <code>{{ e.field }}</code>: {{ e.detail }}
      </li>
    </ul>
    <p v-if="error.requestId" class="mt-2 text-xs opacity-75">Request ID: {{ error.requestId }}</p>
  </div>
</template>
