<script setup lang="ts">
// The site page's "Domains" section: wires SiteDomainsPanel to the go-live and
// aliases endpoints. Both answer 202 with a job; the page follows it (its job
// list picks it up after `changed`).
import type { SiteAliases, SiteGoLive } from '../../utils/domains'
import type { Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()
const emit = defineEmits<{ changed: [] }>()
const api = useApi()
const notify = useNotify()
const { can } = useCan()

async function goLive(body: SiteGoLive) {
  const { data } = await api.POST('/api/v1/sites/{site_id}/go-live', { params: { path: { site_id: props.site.id } }, body })
  if (data) notify.jobStarted(data.job)
  emit('changed')
}
async function setAliases(body: SiteAliases) {
  const { data } = await api.PUT('/api/v1/sites/{site_id}/aliases', { params: { path: { site_id: props.site.id } }, body })
  if (data) notify.jobStarted(data.job)
  emit('changed')
}
</script>

<template>
  <SiteDomainsPanel :site="site" :can-manage="can('site.domains.manage')" :go-live="goLive" :set-aliases="setAliases" />
</template>
