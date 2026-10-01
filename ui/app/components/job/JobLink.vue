<script setup lang="ts">
// The link to a job's progress page, for places that start a job (a 202
// response carries the job). Default text is "View progress"; for a toast
// use useNotify().jobStarted(job), which adds the same link as an action.
defineProps<{ jobId: string }>()
</script>

<template>
  <NuxtLink :to="`/jobs/${jobId}`" class="inline-flex items-center gap-1 text-primary underline underline-offset-2">
    <slot>View progress</slot>
    <UIcon name="i-lucide-arrow-right" class="size-3.5" aria-hidden="true" />
  </NuxtLink>
</template>
