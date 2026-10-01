<script setup lang="ts">
// The app-level error page: unknown routes (404), pages the role may not open
// (403), a control plane that is unreachable or failing at load (5xx), and
// bugs. Defining it keeps Nuxt's built-in error pages out of the bundle; those
// inject an inline script that the Content-Security-Policy would block.
//
// Signed-in users keep the app shell around the message, so they can
// navigate away. A failed load of data inside a page is rendered in place by
// the page itself (ProblemAlert), not here.
import type { NuxtError } from '#app'

const props = defineProps<{ error: NuxtError }>()

const route = useRoute()
const user = useSessionUser()

const status = computed(() => Number(props.error.status) || 500)
const view = computed(() => {
  switch (true) {
    case status.value === 404:
      return { icon: 'i-lucide-map-pinned', title: 'Page not found', text: 'There is nothing at this address.' }
    case status.value === 403:
      return { icon: 'i-lucide-lock', title: 'You do not have access', text: props.error.message || 'Your role does not allow you to open this page. Ask an administrator if you need access.' }
    case status.value === 503 || status.value === 502 || status.value === 504:
      return { icon: 'i-lucide-wifi-off', title: 'Control plane unavailable', text: 'The dashboard could not reach the control plane, or it is starting up. Try again in a moment.' }
    case status.value >= 500:
      return { icon: 'i-lucide-server-crash', title: 'The server hit a problem', text: props.error.message || 'The control plane reported an internal error.' }
    default:
      return { icon: 'i-lucide-bug', title: 'Something went wrong', text: 'An unexpected error occurred in the dashboard.' }
  }
})
const retryable = computed(() => status.value >= 500 || status.value === 0)

useHead({ title: computed(() => view.value.title) })

function retry() {
  void clearError({ redirect: route.fullPath })
}
</script>

<template>
  <UApp>
    <NuxtLayout :name="user ? 'default' : 'auth'">
      <div class="flex max-w-xl flex-col items-start gap-4" role="alert">
        <p class="flex items-center gap-2 text-sm font-medium text-muted">
          <UIcon :name="view.icon" class="size-5" aria-hidden="true" />
          Error {{ status }}
        </p>
        <h1 class="text-3xl font-semibold tracking-tight">{{ view.title }}</h1>
        <p class="text-muted">{{ view.text }}</p>
        <div class="flex flex-wrap gap-2">
          <UButton v-if="retryable" icon="i-lucide-refresh-cw" @click="retry()">Try again</UButton>
          <UButton :variant="retryable ? 'outline' : 'solid'" color="neutral" icon="i-lucide-arrow-left" @click="clearError({ redirect: '/' })">
            Back to the dashboard
          </UButton>
        </div>
      </div>
    </NuxtLayout>
  </UApp>
</template>
