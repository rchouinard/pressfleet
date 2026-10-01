<script setup lang="ts">
// Proves the wiring end to end: generated types -> useApi -> same-origin
// GET /healthz served by the control plane that embeds this app.
const api = useApi()

// Failures are returned as data so the page can render them; see "Data
// fetching" in ui/README.md.
const { data: health, status, refresh } = useAsyncData('health', async () => {
  try {
    await api.GET('/healthz')
    return { error: null }
  } catch (e) {
    return { error: toApiError(e) }
  }
})
</script>

<template>
  <div class="space-y-6">
    <header class="space-y-2">
      <h1 class="text-3xl font-semibold tracking-tight">PressFleet</h1>
      <p class="text-muted">Self-hosted WordPress application platform.</p>
    </header>

    <UCard>
      <template #header>
        <h2 class="font-medium">Control plane</h2>
      </template>

      <div aria-live="polite" class="flex flex-wrap items-center gap-3">
        <template v-if="!health">
          <UIcon name="i-lucide-loader-circle" class="size-5 animate-spin" aria-hidden="true" />
          <span>Checking the control plane...</span>
        </template>
        <template v-else-if="!health.error">
          <UBadge color="success" variant="subtle" icon="i-lucide-check">Healthy</UBadge>
          <span class="text-muted">The API answered <code>GET /healthz</code>.</span>
        </template>
        <template v-else>
          <UBadge color="error" variant="subtle" icon="i-lucide-triangle-alert">Unavailable</UBadge>
        </template>
      </div>

      <ProblemAlert v-if="health?.error" :error="health.error" class="mt-4" />

      <template #footer>
        <UButton
          variant="outline"
          color="neutral"
          icon="i-lucide-refresh-cw"
          :loading="status === 'pending'"
          @click="refresh()"
        >
          Check again
        </UButton>
      </template>
    </UCard>
  </div>
</template>
