<script setup lang="ts">
// "Export site" (design doc §5.7): the site's wp-content and database in the
// two files a site import takes, here or on another host. The export runs as
// a job; the files wait on the server until they expire. An export already
// running is picked up on load.
import { canExportSite, isSiteExportJob } from '../../utils/exports'
import { isTerminal } from '../../utils/job'
import type { Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()

const api = useApi()
const notify = useNotify()
const { can } = useCan()

const jobId = ref<string>()
const { data: running } = await useAsyncData(() => `site-export-running-${props.site.id}`, async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', {
      params: { query: { target_type: 'site', target_id: props.site.id, limit: 25 } },
    })
    return (data?.items ?? []).find(j => isSiteExportJob(j) && !isTerminal(j.state))?.id ?? null
  } catch {
    return null
  }
})
watch(running, (v) => {
  if (v && !jobId.value) jobId.value = v
}, { immediate: true })

const starting = ref(false)
async function start() {
  starting.value = true
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/export', { params: { path: { site_id: props.site.id } } })
    if (data) {
      notify.jobStarted(data.job)
      jobId.value = data.job.id
    }
  } catch (e) {
    notify.error(e)
  } finally {
    starting.value = false
  }
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="site-export-heading">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h2 id="site-export-heading" class="text-lg font-semibold">Export</h2>
      <UButton
        v-if="can('site.export')"
        size="sm"
        icon="i-lucide-package"
        :loading="starting"
        :disabled="!canExportSite(site) || !!jobId"
        @click="start()"
      >Export site</UButton>
    </div>
    <p class="text-sm text-muted">
      Exports the site's <code>wp-content</code> and database as the two files a site import takes, to move the site or hand it
      over. The platform's cache drop-ins are left out. The site keeps serving meanwhile; the files are deleted when they expire.
      To export an earlier version, use a backup on the <NuxtLink :to="`/sites/${site.id}/backups`" class="text-primary underline">Backups</NuxtLink> page.
    </p>
    <ExportPanel
      v-model:job-id="jobId"
      :site-id="site.id"
      :kinds="['site', 'snapshot']"
      empty-text="No exports to download."
    />
  </section>
</template>
