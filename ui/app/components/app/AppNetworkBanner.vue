<script setup lang="ts">
// Shown while the browser is offline or the control plane can't be reached
// (a request failed at the network level). It clears itself on the next
// request that gets any answer, or when the browser comes back online. The
// "Check now" button makes such a request.
const reachable = useApiReachable()
const api = useApi()
const online = ref(true)
const checking = ref(false)

function sync() {
  online.value = navigator.onLine
}
onMounted(() => {
  sync()
  window.addEventListener('online', sync)
  window.addEventListener('offline', sync)
})
onBeforeUnmount(() => {
  window.removeEventListener('online', sync)
  window.removeEventListener('offline', sync)
})

const visible = computed(() => !online.value || !reachable.value)

async function check() {
  checking.value = true
  try {
    await api.GET('/healthz')
  } catch {
    // The client already recorded that the control plane is unreachable.
  } finally {
    checking.value = false
  }
}
</script>

<template>
  <div
    v-if="visible"
    role="status"
    class="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-100"
  >
    <UIcon name="i-lucide-wifi-off" class="size-4 shrink-0" aria-hidden="true" />
    <span class="min-w-0 flex-1">
      {{ online ? 'The control plane is unreachable. What you see may be out of date; the dashboard keeps trying.' : 'You are offline. What you see may be out of date.' }}
    </span>
    <UButton size="xs" color="neutral" variant="outline" :loading="checking" @click="check()">Check now</UButton>
  </div>
</template>
