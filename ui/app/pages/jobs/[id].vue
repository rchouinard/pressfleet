<script setup lang="ts">
// Progress of one job with its steps and logs. Reachable for any job id, from
// the jobs list, a toast's "View progress", or a link.
const route = useRoute()
const id = computed(() => String(route.params.id))

useHead({ title: computed(() => `Job ${id.value.slice(0, 8)}`) })
useBreadcrumbs(() => [{ label: 'Jobs', to: '/jobs' }, { label: id.value.slice(0, 8) }])
</script>

<template>
  <div class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">Job</h1>
    <JobProgress :job-id="id" />
  </div>
</template>
