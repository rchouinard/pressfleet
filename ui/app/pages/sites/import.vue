<script setup lang="ts">
// Import a site (design doc §5.7): pick a client group, a wp-content archive
// and a SQL dump; both files are uploaded in chunks with progress, then the
// import job is started and followed here. The archive and dump are checked
// by the job's first step, before any site exists, so a bad file fails the
// job cleanly. A completed upload is kept if the import request itself
// fails, so fixing a field and retrying doesn't send the files again.
import { toApiError, type ApiError } from '../../utils/api-error'
import { isTerminal, type Job } from '../../utils/job'
import {
  IMPORT_NAME_MAX, importPayload, importedSiteId, validateImport,
  type ImportFormValues,
} from '../../utils/site-import'
import { archiveExtensions, dumpExtensions, validateUploadFile, type Upload } from '../../utils/upload'

useHead({ title: 'Import a site' })
useBreadcrumbs([{ label: 'Sites', to: '/sites' }, { label: 'Import' }])
definePageMeta({ requires: 'site.import' })

const api = useApi()
const notify = useNotify()
const { groupItems } = useClientGroupLookup()

const values = reactive<ImportFormValues>({ client_group_id: '', name: '', php_version: '' })
const archive = ref<File>()
const dump = ref<File>()
const local = ref<Partial<Record<keyof ImportFormValues | 'archive' | 'dump', string>>>({})
const failure = ref<ApiError>()
const submitting = ref(false)

const archiveUpload = useUpload()
const dumpUpload = useUpload()
// Completed uploads for the files currently chosen; dropped when the file or
// the client group changes (an upload is tied to its group).
const doneArchive = ref<Upload>()
const doneDump = ref<Upload>()
watch([archive, () => values.client_group_id], () => {
  doneArchive.value = undefined
})
watch([dump, () => values.client_group_id], () => {
  doneDump.value = undefined
})

function err(field: keyof ImportFormValues | 'archive' | 'dump') {
  return local.value[field] ?? (field === 'archive' || field === 'dump' ? undefined : failure.value?.fieldError(`/${field}`))
}
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

// After the request: the job being followed.
const jobId = ref<string>()
const siteId = ref<string>()
const finished = ref<Job>()
const started = computed(() => jobId.value !== undefined)
const importedId = computed(() => (finished.value?.state === 'succeeded' ? importedSiteId(finished.value.result) ?? siteId.value : undefined))

function onJobUpdate(job: Job) {
  if (isTerminal(job.state)) finished.value = job
}

async function submit() {
  failure.value = undefined
  local.value = validateImport(values)
  const a = validateUploadFile(archive.value, archiveExtensions, 'the wp-content archive')
  const d = validateUploadFile(dump.value, dumpExtensions, 'the database dump')
  if (a) local.value.archive = a
  if (d) local.value.dump = d
  if (Object.keys(local.value).length > 0 || !archive.value || !dump.value) return

  submitting.value = true
  try {
    const base = { purpose: 'site_import' as const, client_group_id: values.client_group_id }
    if (!doneArchive.value) {
      doneArchive.value = await archiveUpload.start(archive.value, base)
      if (!doneArchive.value) return // cancelled
    }
    if (!doneDump.value) {
      doneDump.value = await dumpUpload.start(dump.value, base)
      if (!doneDump.value) return
    }
    const { data } = await api.POST('/api/v1/sites/import', {
      body: importPayload(values, doneArchive.value.id, doneDump.value.id),
    })
    if (!data) return
    siteId.value = data.site_id
    jobId.value = data.job.id
    notify.jobStarted(data.job)
  } catch (e) {
    failure.value = toApiError(e)
    // An upload that failed was removed by the uploader; a stale one (the
    // server refused it) can't be reused either.
    if (failure.value.status === 409 || failure.value.status === 422) {
      doneArchive.value = undefined
      doneDump.value = undefined
    }
  } finally {
    submitting.value = false
  }
}

function cancel() {
  archiveUpload.cancel()
  dumpUpload.cancel()
}

function startOver() {
  jobId.value = undefined
  siteId.value = undefined
  finished.value = undefined
  doneArchive.value = undefined
  doneDump.value = undefined
  archive.value = undefined
  dump.value = undefined
}
</script>

<template>
  <div class="space-y-6">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Import a site</h1>
      <p class="text-sm text-muted">
        Bring an existing WordPress site onto the platform from a copy of its <code>wp-content</code> and a database dump. The files are checked before any site is created.
      </p>
    </header>

    <form v-if="!started" class="max-w-2xl space-y-5" novalidate @submit.prevent="submit">
      <ProblemAlert v-if="alertError" :error="alertError" />

      <UFormField label="Client group" name="client_group_id" required :error="err('client_group_id')">
        <USelect v-model="values.client_group_id" :items="groupItems" placeholder="Choose a client group" class="w-full" :disabled="submitting" />
      </UFormField>

      <FileField
        v-model:file="archive"
        label="wp-content archive"
        hint="A .tar.gz, .tar.bz2, .tar or .zip of the site's wp-content directory, or of a whole WordPress install."
        :accept="archiveExtensions.join(',')"
        :error="err('archive')"
        :disabled="submitting"
      />
      <FileField
        v-model:file="dump"
        label="Database dump"
        hint="A .sql or .sql.gz file from wp db export, mysqldump or phpMyAdmin. Multisite networks aren't supported."
        :accept="dumpExtensions.join(',')"
        :error="err('dump')"
        :disabled="submitting"
      />

      <UFormField label="Name" name="name" hint="Optional" description="Defaults to the imported site's title." :error="err('name')">
        <UInput v-model="values.name" :maxlength="IMPORT_NAME_MAX" autocomplete="off" class="w-full" :disabled="submitting" />
      </UFormField>
      <UFormField label="PHP version" name="php_version" hint="Optional" description="For example 8.4. The platform default is used if blank." :error="err('php_version')">
        <UInput v-model="values.php_version" autocomplete="off" class="w-full" :disabled="submitting" />
      </UFormField>

      <div v-if="archiveUpload.progress.value || dumpUpload.progress.value || doneArchive || doneDump" class="space-y-3 rounded-md border border-default p-3" aria-live="polite">
        <UploadProgressBar v-if="archive && archiveUpload.progress.value" :name="archive.name" :progress="archiveUpload.progress.value" />
        <p v-else-if="doneArchive && archive" class="text-sm">Archive uploaded: {{ archive.name }}</p>
        <UploadProgressBar v-if="dump && dumpUpload.progress.value" :name="dump.name" :progress="dumpUpload.progress.value" />
        <p v-else-if="doneDump && dump" class="text-sm">Dump uploaded: {{ dump.name }}</p>
      </div>

      <p class="text-sm text-muted">
        Keep this page open while the files upload. The site keeps its own WordPress users, so no admin password is generated, and its address is set to the preview domain.
      </p>

      <div class="flex flex-wrap gap-2">
        <UButton type="submit" icon="i-lucide-upload" :loading="submitting">Upload and import</UButton>
        <UButton v-if="submitting" color="neutral" variant="outline" @click="cancel()">Cancel upload</UButton>
        <UButton v-else color="neutral" variant="ghost" to="/sites">Back to sites</UButton>
      </div>
    </form>

    <section v-else-if="jobId" class="space-y-4" aria-labelledby="import-job-heading">
      <h2 id="import-job-heading" class="sr-only">Import progress</h2>
      <JobProgress :job-id="jobId" @update="onJobUpdate" />
      <div v-if="importedId" class="flex flex-wrap items-center gap-3 rounded-md border border-default bg-elevated p-3 text-sm" role="status">
        <UIcon name="i-lucide-circle-check" class="size-4" aria-hidden="true" />
        <span>The site was imported. It runs on its preview domain until you take it live.</span>
        <UButton size="sm" :to="`/sites/${importedId}`">Open the site</UButton>
      </div>
      <div v-else-if="finished" class="flex flex-wrap items-center gap-3">
        <p class="text-sm text-muted">No site was kept. Fix the problem above and try again.</p>
        <UButton size="sm" color="neutral" variant="outline" @click="startOver()">Import again</UButton>
      </div>
      <p v-else-if="siteId" class="text-sm text-muted">
        You can leave this page; the job keeps running and is listed under <NuxtLink to="/jobs" class="text-primary underline">Jobs</NuxtLink>.
      </p>
    </section>
  </div>
</template>
