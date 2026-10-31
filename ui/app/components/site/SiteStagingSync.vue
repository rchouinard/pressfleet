<script setup lang="ts">
// The staging page's "Refresh from production" and "Push to production"
// buttons and their dialogs (#208), in the banner's `actions` slot. The
// production site is read here (the dialogs need its slug, state and
// versions, and a fresh read each time one opens, since something else may
// be working on it). Buttons the role can't use are hidden (IfCan); ones that
// can't work right now stay and say why, as SiteStagingCard does.
//
// A refresh changes the staging site, so the page stays and follows the
// `site.refresh` job. A push changes the production site: its job is on that
// site, so the page goes there, where the "Job progress" section picks the
// job up and shows the pre-push backup when it ends.
import { toApiError, type ApiError } from '../../utils/api-error'
import { backupChoices, type BackupChoice } from '../../utils/staging'
import {
  pushBlockedReason, pushPayload, refreshBlockedReason, refreshPayload,
  type ProductionLookup, type RefreshFormValues, type SyncScope,
} from '../../utils/staging-sync'
import { productionIdOf, type Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()
const emit = defineEmits<{ changed: [] }>()

const api = useApi()
const notify = useNotify()
const { can } = useCan()

const productionId = computed(() => productionIdOf(props.site))

const production = ref<Site>()
const productionLoading = ref(false)
const productionError = ref<string>()
async function loadProduction() {
  const id = productionId.value
  if (!id) return
  productionLoading.value = true
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: id } } })
    production.value = data
    productionError.value = undefined
  } catch (e) {
    production.value = undefined
    productionError.value = toApiError(e).title
  } finally {
    productionLoading.value = false
  }
}
// Again whenever the staging site changes (a job ended), so the reasons stay current.
watch([productionId, () => props.site.status, () => props.site.updated_at], () => void loadProduction(), { immediate: true })

const lookup = computed<ProductionLookup>(() => ({ site: production.value, loading: productionLoading.value && !production.value, error: productionError.value }))
const refreshBlocked = computed(() => refreshBlockedReason(props.site))
const pushBlocked = computed(() => pushBlockedReason(props.site, lookup.value))

// Refresh: the production backups to copy from are read when the dialog opens.
const refreshOpen = ref(false)
const backups = ref<BackupChoice[]>([])
const backupsLoading = ref(false)
const backupsError = ref<ApiError>()
async function openRefresh() {
  backups.value = []
  backupsError.value = undefined
  backupsLoading.value = true
  refreshOpen.value = true
  const id = productionId.value
  try {
    // The production site's state decides whether "a fresh copy" is offered.
    const [, backupList] = await Promise.all([
      loadProduction(),
      id ? api.GET('/api/v1/sites/{site_id}/backups', { params: { path: { site_id: id }, query: { limit: 50 } } }) : undefined,
    ])
    backups.value = backupChoices(backupList?.data?.items ?? [])
  } catch (e) {
    backupsError.value = toApiError(e)
  } finally {
    backupsLoading.value = false
  }
}

async function refresh(values: RefreshFormValues) {
  const { data } = await api.POST('/api/v1/sites/{site_id}/refresh', {
    params: { path: { site_id: props.site.id } },
    body: refreshPayload(values),
  })
  if (!data) return
  notify.jobStarted(data.job, 'Refresh from production started')
  emit('changed')
}

// Push: read production fresh before the dialog opens.
const pushOpen = ref(false)
const pushPreparing = ref(false)
async function openPush() {
  pushPreparing.value = true
  try {
    await loadProduction()
    if (!pushBlockedReason(props.site, lookup.value)) pushOpen.value = true
  } finally {
    pushPreparing.value = false
  }
}

async function push(scope: SyncScope) {
  const { data } = await api.POST('/api/v1/sites/{site_id}/push', {
    params: { path: { site_id: props.site.id } },
    body: pushPayload(scope),
  })
  if (!data) return
  notify.jobStarted(data.job, 'Push to production started')
  // The response carries the production site, and the job is on it.
  await navigateTo(`/sites/${data.site.id}`)
}
</script>

<template>
  <div class="flex flex-col gap-2 sm:items-end" data-testid="staging-sync">
    <div class="flex flex-wrap gap-2">
      <IfCan action="site.staging.refresh">
        <UButton icon="i-lucide-refresh-cw" color="neutral" variant="outline" :disabled="!!refreshBlocked" data-testid="refresh-open" @click="openRefresh()">Refresh from production</UButton>
      </IfCan>
      <IfCan action="site.staging.push">
        <UButton icon="i-lucide-upload" color="error" variant="outline" :disabled="!!pushBlocked" :loading="pushPreparing" data-testid="push-open" @click="openPush()">Push to production</UButton>
      </IfCan>
    </div>
    <p v-if="refreshBlocked && can('site.staging.refresh')" class="max-w-md text-sm text-muted sm:text-right" role="status" data-testid="refresh-blocked">{{ refreshBlocked }}</p>
    <p v-if="pushBlocked && can('site.staging.push')" class="max-w-md text-sm text-muted sm:text-right" role="status" data-testid="push-blocked">{{ pushBlocked }}</p>

    <SiteRefreshDialog
      v-model:open="refreshOpen"
      :staging-name="site.name"
      :production="production"
      :backups="backups"
      :backups-loading="backupsLoading"
      :backups-error="backupsError"
      :submit="refresh"
    />
    <SitePushDialog
      v-if="production"
      v-model:open="pushOpen"
      :staging="site"
      :production="production"
      :submit="push"
    />
  </div>
</template>
