<script setup lang="ts">
// Deploy a theme or plugin package to a site (design doc §5.8): pick what it
// is and what to call it, upload the zip or tar.gz with progress, then follow
// the deployment job. The job swaps the package in atomically, checks the
// site's health and rolls back by itself if the check fails.
import { toApiError, type ApiError } from '../../../utils/api-error'
import { formatBytes } from '../../../utils/backup'
import {
  DEPLOY_NAME_MAX, canActivate, canDeployToSite, deployBlockedReason, deployKindOptions, deployPayload, deployResult,
  validateDeploy, type DeployFormValues,
} from '../../../utils/deploy'
import { isTerminal, type Job } from '../../../utils/job'
import { activeJob } from '../../../utils/site'
import { packageExtensions, validateUploadFile } from '../../../utils/upload'

definePageMeta({ requires: 'site.code.deploy' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const id = computed(() => String(route.params.id))

// Same key and shape as the other site pages, so they share a cache entry.
const { data: loaded, refresh: refreshSite } = await useAsyncData(
  () => `site-${id.value}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: id.value } } })
      return { site: data ?? null, error: null as ApiError | null }
    } catch (e) {
      return { site: null, error: toApiError(e) }
    }
  },
  { watch: [id] },
)
const site = computed(() => loaded.value?.site ?? undefined)
const loadError = computed(() => loaded.value?.error ?? undefined)
const name = computed(() => site.value?.name ?? id.value.slice(0, 8))

useHead({ title: computed(() => `Deploy code: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Sites', to: '/sites' }, { label: name.value, to: `/sites/${id.value}` }, { label: 'Deploy code' }])

const values = reactive<DeployFormValues>({ kind: 'theme', name: '', activate: false })
const pkg = ref<File>()
const local = ref<Partial<Record<'name' | 'package', string>>>({})
const failure = ref<ApiError>()
const submitting = ref(false)
const upload = useUpload()

const kindItems = deployKindOptions.map(o => ({ value: o.value, label: o.label, description: o.description }))
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))
function err(field: 'name' | 'package') {
  return local.value[field] ?? (field === 'name' ? failure.value?.fieldError('/name') : undefined)
}

// The job in flight: picked up from the site's jobs on load, or set here.
const jobId = ref<string>()
const finished = ref<Job>()
const { data: pending } = await useAsyncData(() => `site-deploy-jobs-${id.value}`, async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', {
      params: { query: { target_type: 'site', target_id: id.value, limit: 10 } },
    })
    return activeJob(data?.items ?? [])?.id ?? null
  } catch {
    return null
  }
}, { watch: [id] })
watch(pending, (v) => {
  if (v && !jobId.value) jobId.value = v
}, { immediate: true })

const handled = new Set<string>()
async function onJobUpdate(job: Job) {
  if (!isTerminal(job.state) || handled.has(job.id)) return
  handled.add(job.id)
  finished.value = job
  await refreshSite()
}
const busyWithJob = computed(() => jobId.value !== undefined && finished.value?.id !== jobId.value)
const result = computed(() => (finished.value?.type === 'site.deploy' && finished.value.state === 'succeeded' ? deployResult(finished.value.result) : undefined))

const blocked = computed(() => (site.value ? deployBlockedReason(site.value) : undefined))
const canSubmit = computed(() => !!site.value && canDeployToSite(site.value) && !busyWithJob.value)

async function submit() {
  failure.value = undefined
  local.value = validateDeploy(values)
  const f = validateUploadFile(pkg.value, packageExtensions, 'the package')
  if (f) local.value.package = f
  if (Object.keys(local.value).length > 0 || !pkg.value) return

  submitting.value = true
  try {
    const done = await upload.start(pkg.value, { purpose: 'site_deploy', site_id: id.value })
    if (!done) return // cancelled
    const { data } = await api.POST('/api/v1/sites/{site_id}/deploy', {
      params: { path: { site_id: id.value } },
      body: deployPayload(values, done.id),
    })
    if (data) {
      notify.jobStarted(data.job)
      finished.value = undefined
      jobId.value = data.job.id
    }
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <header class="space-y-1">
      <h1 class="break-words text-2xl font-semibold tracking-tight">Deploy code: {{ name }}</h1>
      <p class="text-sm text-muted">
        <NuxtLink :to="`/sites/${id}`" class="text-primary underline">Back to the site</NuxtLink>
      </p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="site">
      <section v-if="jobId" class="space-y-3" aria-labelledby="deploy-job-heading">
        <div class="flex items-center justify-between gap-2">
          <h2 id="deploy-job-heading" class="text-lg font-semibold">Deployment</h2>
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide job progress" :disabled="busyWithJob" @click="jobId = undefined; finished = undefined" />
        </div>
        <JobProgress :job-id="jobId" @update="onJobUpdate" />
        <dl v-if="result" class="grid gap-x-6 gap-y-2 rounded-md border border-default bg-elevated p-3 text-sm sm:grid-cols-2">
          <div><dt class="text-muted">Deployed to</dt><dd class="break-all font-mono text-xs">{{ result.path || `${result.kind}/${result.name}` }}</dd></div>
          <div><dt class="text-muted">Files</dt><dd>{{ result.files ?? '-' }} ({{ formatBytes(result.bytes) }})</dd></div>
          <div><dt class="text-muted">Replaced a previous version</dt><dd>{{ result.replaced ? 'Yes' : 'No, first deployment' }}</dd></div>
          <div><dt class="text-muted">Activated</dt><dd>{{ result.activated ? 'Yes' : 'No' }}</dd></div>
        </dl>
      </section>

      <p v-if="blocked" class="rounded-md border border-default bg-elevated p-3 text-sm" role="status">{{ blocked }}</p>

      <form class="max-w-2xl space-y-5" novalidate @submit.prevent="submit">
        <ProblemAlert v-if="alertError" :error="alertError" />

        <UFormField name="kind">
          <URadioGroup v-model="values.kind" legend="What are you deploying?" :items="kindItems" :disabled="submitting" />
        </UFormField>

        <UFormField label="Directory name" name="name" required :error="err('name')" description="The theme or plugin's slug. An existing directory of this name is replaced.">
          <UInput v-model="values.name" :maxlength="DEPLOY_NAME_MAX" autocomplete="off" placeholder="acme-theme" class="w-full" :disabled="submitting" />
        </UFormField>

        <FileField
          v-model:file="pkg"
          label="Package"
          hint="A .zip or .tar.gz. If everything is in one top-level folder, as in theme and plugin zips, that folder is left out."
          :accept="packageExtensions.join(',')"
          :error="err('package')"
          :disabled="submitting"
        />

        <UCheckbox
          v-if="canActivate(values.kind)"
          v-model="values.activate"
          :label="`Activate the ${values.kind} after deploying`"
          :disabled="submitting"
        />

        <UploadProgressBar v-if="pkg && upload.progress.value" :name="pkg.name" :progress="upload.progress.value" />

        <p class="text-sm text-muted">
          The package is checked first, swapped in atomically, and the site's health is verified. If that fails, the previous version is put back.
        </p>

        <div class="flex flex-wrap gap-2">
          <UButton type="submit" icon="i-lucide-rocket" :loading="submitting" :disabled="!canSubmit">Upload and deploy</UButton>
          <UButton v-if="submitting" color="neutral" variant="outline" @click="upload.cancel()">Cancel upload</UButton>
        </div>
      </form>
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading site...</p>
  </div>
</template>
