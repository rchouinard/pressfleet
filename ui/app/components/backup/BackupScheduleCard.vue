<script setup lang="ts">
// A site's backup schedule: a summary, and for those who may change it
// (`canEdit`) a form. The page supplies `save(values)`, which does the PUT;
// 422s show next to the fields, anything else in a ProblemAlert.
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  describeSchedule, hourOptions, intervalOptions, scheduleToForm, scheduleUnchanged, validateSchedule,
  type BackupSchedule, type ScheduleFormValues,
} from '../../utils/backup'

const props = defineProps<{
  schedule: BackupSchedule
  canEdit: boolean
  save: (values: ScheduleFormValues) => Promise<void>
}>()

const editing = ref(false)
const values = ref<ScheduleFormValues>(scheduleToForm(props.schedule))
const busy = ref(false)
const error = ref<ApiError>()
const clientErrors = ref<ReturnType<typeof validateSchedule>>({})

function edit() {
  values.value = scheduleToForm(props.schedule)
  error.value = undefined
  clientErrors.value = {}
  editing.value = true
}

async function submit() {
  clientErrors.value = validateSchedule(values.value)
  if (Object.keys(clientErrors.value).length > 0) return
  busy.value = true
  error.value = undefined
  try {
    await props.save(values.value)
    editing.value = false
  } catch (e) {
    error.value = toApiError(e)
  } finally {
    busy.value = false
  }
}

const fieldError = (pointer: string, own?: string) => own ?? error.value?.fieldError(pointer)
const unchanged = computed(() => scheduleUnchanged(props.schedule, values.value))

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="backup-schedule-heading">
    <div class="flex flex-wrap items-start justify-between gap-2">
      <h2 id="backup-schedule-heading" class="text-lg font-semibold">Schedule</h2>
      <UButton v-if="canEdit && !editing" color="neutral" variant="outline" size="sm" icon="i-lucide-pencil" @click="edit()">Edit schedule</UButton>
    </div>

    <template v-if="!editing">
      <p class="text-sm">{{ describeSchedule(schedule) }}</p>
      <p v-if="schedule.next_run_at" class="text-sm text-muted">Next backup: <time :datetime="schedule.next_run_at">{{ when(schedule.next_run_at) }}</time></p>
    </template>

    <form v-else class="space-y-3" novalidate @submit.prevent="submit()">
      <UCheckbox v-model="values.enabled" label="Back this site up on a schedule" :disabled="busy" />
      <UFormField label="How often" :error="fieldError('/interval_hours', clientErrors.interval_hours)">
        <USelect v-model="values.interval_hours" :items="intervalOptions" :disabled="busy || !values.enabled" class="w-full sm:w-64" />
      </UFormField>
      <UFormField label="Start hour" description="UTC. With an interval under a day, one backup starts in this hour." :error="fieldError('/hour', clientErrors.hour)">
        <USelect v-model="values.hour" :items="hourOptions" :disabled="busy || !values.enabled" class="w-full sm:w-64" />
      </UFormField>
      <ProblemAlert v-if="error" :error="error" />
      <div class="flex flex-wrap gap-2">
        <UButton type="submit" :loading="busy" :disabled="unchanged">Save schedule</UButton>
        <UButton color="neutral" variant="outline" :disabled="busy" @click="editing = false">Cancel</UButton>
      </div>
    </form>
  </section>
</template>
