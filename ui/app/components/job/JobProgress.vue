<script setup lang="ts">
// Live progress of any job: <JobProgress :job-id="id" />. Polls through
// useJob and renders with JobProgressView. A failed load (404 for an unknown
// job, 403) shows in place with ProblemAlert.
import type { Job } from '../../utils/job'
import JobProgressView from './JobProgressView.vue'

const props = defineProps<{ jobId: string }>()
const emit = defineEmits<{
  /** Every time the job's data changes; lets a parent react to the end. */
  update: [job: Job]
}>()

const { job, error, retrying, status, refresh, restart } = useJob(() => props.jobId)
const router = useRouter()

watch(job, (j) => {
  if (j) emit('update', j)
})

// Link the target only when its page exists (sites arrive with #84).
const targetHref = computed(() => {
  const t = job.value?.target
  if (!t || t.type === 'platform') return undefined
  const path = t.type === 'site' ? `/sites/${t.id}` : `/client-groups/${t.id}`
  return router.resolve(path).matched.length > 0 ? path : undefined
})
</script>

<template>
  <div>
    <JobProgressView
      v-if="job"
      :job="job"
      :poll-error="error"
      :retrying="retrying"
      @refresh="status === 'failed' ? restart() : refresh()"
    >
      <template v-if="targetHref" #target="{ target }">
        <NuxtLink :to="targetHref" class="break-all text-primary underline">{{ target.type }} <span class="font-mono text-xs">{{ target.id }}</span></NuxtLink>
      </template>
    </JobProgressView>
    <template v-else-if="error">
      <ProblemAlert :error="error" />
      <UButton class="mt-3" color="neutral" variant="outline" icon="i-lucide-refresh-cw" @click="restart()">Try again</UButton>
    </template>
    <p v-else class="flex items-center gap-2 text-sm text-muted" role="status">
      <UIcon name="i-lucide-loader-circle" class="size-4 motion-safe:animate-spin" aria-hidden="true" />
      Loading job...
    </p>
  </div>
</template>
