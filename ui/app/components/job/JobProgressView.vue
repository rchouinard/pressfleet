<script setup lang="ts">
// The presentation of one job: state, timing, steps with their logs, the
// final error and the roll-back outcome. It takes the job as a prop and has
// no Nuxt dependencies, so it is unit tested; <JobProgress> adds polling.
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import type { ApiError } from '../../utils/api-error'
import {
  formatDuration,
  isTerminal,
  jobStateView,
  jobTypeLabel,
  rollbackSummary,
  stepLabel,
  stepStateView,
  type Job,
  type JobStep,
  waitsFor,
} from '../../utils/job'
import JobStateBadge from './JobStateBadge.vue'
import JobStepLog from './JobStepLog.vue'

const props = defineProps<{
  job: Job
  /** The latest failure to load the job (stream or poll), if any. */
  pollError?: ApiError
  /** `pollError` is being retried. */
  retrying?: boolean
}>()

const emit = defineEmits<{ refresh: [] }>()

const live = computed(() => !isTerminal(props.job.state))
const stateView = computed(() => jobStateView(props.job))
const rollback = computed(() => rollbackSummary(props.job))

// Durations of running things tick; stopped jobs don't need the timer.
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  ticker = setInterval(() => {
    if (live.value) now.value = Date.now()
  }, 1000)
})
onBeforeUnmount(() => clearInterval(ticker))

const waiting = computed(() => waitsFor(props.job))

const duration = computed(() => formatDuration(props.job.started_at ?? props.job.created_at, props.job.finished_at, now.value))

function stepDuration(s: JobStep): string | undefined {
  return formatDuration(s.started_at, s.finished_at, now.value)
}

// Steps that are running or went wrong open by default; the user's own
// toggles win from then on.
const overrides = reactive<Record<string, boolean>>({})
function isOpen(s: JobStep): boolean {
  return overrides[s.name] ?? (s.state === 'running' || s.state === 'failed' || s.state === 'rollback_failed')
}
function onToggle(s: JobStep, event: Event) {
  overrides[s.name] = (event.target as HTMLDetailsElement).open
}

function when(iso: string | null | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

const showError = computed(() => !!props.job.error && (props.job.state !== 'succeeded'))
const errorHeading = computed(() => (props.job.state === 'queued' ? 'Last attempt failed; it will be retried' : 'The job failed'))
const hasResult = computed(() => Object.keys(props.job.result ?? {}).length > 0)
</script>

<template>
  <section class="space-y-5" :aria-label="`Progress of job ${job.id}`">
    <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
      <h2 class="text-lg font-semibold">{{ jobTypeLabel(job.type) }}</h2>
      <!-- Announces state changes to screen readers without the whole page being live. -->
      <div role="status" aria-live="polite">
        <JobStateBadge :view="stateView" />
      </div>
      <p v-if="live && job.attempts > 1" class="text-sm text-muted">Attempt {{ job.attempts }} of {{ job.max_attempts }}</p>
    </div>

    <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-2 lg:grid-cols-3">
      <div>
        <dt class="text-muted">Job ID</dt>
        <dd class="break-all font-mono text-xs">{{ job.id }}</dd>
      </div>
      <div>
        <dt class="text-muted">Target</dt>
        <dd>
          <!-- The wrapper fills the slot with a router link when the target's page exists. -->
          <slot name="target" :target="job.target">
            <span class="break-all">{{ job.target.type }} <span class="font-mono text-xs">{{ job.target.id }}</span></span>
          </slot>
        </dd>
      </div>
      <div v-if="waiting" data-testid="waits-for">
        <dt class="text-muted">{{ waiting.label }}</dt>
        <dd>
          <!-- The wrapper fills the slot with a router link. -->
          <slot name="waits-for" :job-id="waiting.jobId">
            <span class="break-all font-mono text-xs">{{ waiting.jobId }}</span>
          </slot>
        </dd>
      </div>
      <div>
        <dt class="text-muted">Duration</dt>
        <dd>{{ duration ?? '-' }}<span v-if="live && duration" class="text-muted"> (so far)</span></dd>
      </div>
      <div>
        <dt class="text-muted">Created</dt>
        <dd><time :datetime="job.created_at">{{ when(job.created_at) }}</time></dd>
      </div>
      <div>
        <dt class="text-muted">Started</dt>
        <dd><time v-if="job.started_at" :datetime="job.started_at">{{ when(job.started_at) }}</time><template v-else>-</template></dd>
      </div>
      <div>
        <dt class="text-muted">Finished</dt>
        <dd><time v-if="job.finished_at" :datetime="job.finished_at">{{ when(job.finished_at) }}</time><template v-else>-</template></dd>
      </div>
    </dl>

    <div v-if="pollError" role="status" class="flex flex-wrap items-center gap-3 rounded-md border border-default bg-elevated p-3 text-sm">
      <UIcon name="i-lucide-wifi-off" class="size-4 shrink-0" aria-hidden="true" />
      <span class="min-w-0 flex-1">
        {{ retrying ? 'Could not refresh this job; trying again. Showing the last known state.' : `Could not refresh this job: ${pollError.title}.` }}
        <span v-if="pollError.requestId" class="text-xs text-muted">Request ID: {{ pollError.requestId }}</span>
      </span>
      <UButton size="xs" color="neutral" variant="outline" @click="emit('refresh')">Try now</UButton>
    </div>

    <div
      v-if="showError"
      role="alert"
      class="rounded-md border border-red-300 bg-red-50 p-4 text-sm text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
    >
      <p class="font-medium">{{ errorHeading }}</p>
      <p class="mt-1 whitespace-pre-wrap break-words font-mono text-xs">{{ job.error }}</p>
    </div>

    <div
      v-if="rollback.attempted && rollback.text"
      class="rounded-md border p-4 text-sm"
      :class="job.state === 'rollback_failed'
        ? 'border-red-300 bg-red-50 text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100'
        : 'border-default bg-elevated'"
      :role="job.state === 'rollback_failed' ? 'alert' : 'status'"
      data-testid="rollback"
    >
      <p class="flex items-center gap-2 font-medium">
        <UIcon :name="job.state === 'rollback_failed' ? 'i-lucide-triangle-alert' : 'i-lucide-undo-2'" class="size-4" aria-hidden="true" />
        Roll-back
      </p>
      <p class="mt-1">{{ rollback.text }}</p>
    </div>

    <div>
      <h3 class="mb-2 font-medium">Steps</h3>
      <p v-if="job.steps.length === 0" class="text-sm text-muted">
        {{ live ? 'The steps appear once a worker starts the job.' : 'This job recorded no steps.' }}
      </p>
      <ol v-else class="divide-y divide-default rounded-md border border-default">
        <li v-for="(step, i) in job.steps" :key="step.name">
          <details :open="isOpen(step)" @toggle="onToggle(step, $event)">
            <summary class="flex min-h-11 cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
              <span class="w-5 shrink-0 text-right text-muted" aria-hidden="true">{{ i + 1 }}.</span>
              <span class="min-w-0 flex-1 font-medium">{{ stepLabel(step.name) }}</span>
              <span v-if="step.attempts > 1" class="text-xs text-muted">{{ step.attempts }} attempts</span>
              <span v-if="stepDuration(step)" class="text-xs text-muted">{{ stepDuration(step) }}</span>
              <JobStateBadge :view="stepStateView(step.state)" />
            </summary>
            <div class="space-y-2 px-3 pb-3 pl-10">
              <p class="text-xs text-muted">
                <template v-if="step.started_at">Started <time :datetime="step.started_at">{{ when(step.started_at) }}</time></template>
                <template v-else>Not started</template>
                <template v-if="step.finished_at">, finished <time :datetime="step.finished_at">{{ when(step.finished_at) }}</time></template>
              </p>
              <JobStepLog :entries="step.log ?? []" :follow="step.state === 'running'" :label="`Log of step ${stepLabel(step.name)}`" />
            </div>
          </details>
        </li>
      </ol>
    </div>

    <details v-if="hasResult" class="rounded-md border border-default">
      <summary class="min-h-11 cursor-pointer px-3 py-2 text-sm font-medium">Result</summary>
      <!-- Secrets are never in a result: they appear as { secret_ref } only. -->
      <pre class="overflow-auto px-3 pb-3 font-mono text-xs">{{ JSON.stringify(job.result, null, 2) }}</pre>
    </details>
  </section>
</template>
