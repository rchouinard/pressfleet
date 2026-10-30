<script setup lang="ts">
// Jobs as a table: type (a link to the progress page), target, state,
// created, duration. Used by the jobs list and the dashboard. The table
// scrolls sideways inside its own region on narrow screens.
import { formatDuration, jobStateView, jobTypeLabel, waitsFor, type Job } from '../../utils/job'

defineProps<{ jobs: readonly Job[], caption: string }>()

function created(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[34rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Job</th>
          <th scope="col" class="px-3 py-2 font-medium">Target</th>
          <th scope="col" class="px-3 py-2 font-medium">State</th>
          <th scope="col" class="px-3 py-2 font-medium">Created</th>
          <th scope="col" class="px-3 py-2 font-medium">Duration</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="job in jobs" :key="job.id">
          <th scope="row" class="px-3 py-2 font-medium">
            <NuxtLink :to="`/jobs/${job.id}`" class="rounded text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">
              {{ jobTypeLabel(job.type) }}
            </NuxtLink>
          </th>
          <td class="px-3 py-2">
            {{ job.target.type }}
            <span class="font-mono text-xs text-muted">{{ job.target.id.slice(0, 8) }}</span>
          </td>
          <td class="px-3 py-2">
            <JobStateBadge :view="jobStateView(job)" />
            <span v-if="waitsFor(job)" class="mt-1 block text-xs text-muted" data-testid="waits-for">
              {{ waitsFor(job)!.label }}
              <NuxtLink :to="`/jobs/${waitsFor(job)!.jobId}`" class="font-mono text-primary underline-offset-2 hover:underline">{{ waitsFor(job)!.jobId.slice(0, 8) }}</NuxtLink>
            </span>
          </td>
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="job.created_at">{{ created(job.created_at) }}</time></td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatDuration(job.started_at, job.finished_at) ?? '-' }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
