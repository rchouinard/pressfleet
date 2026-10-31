<script setup lang="ts">
// WordPress core for one site (design doc §5.5): version and branch, whose
// policy applies, the release it moves to next, a major release waiting for
// approval, branch warnings, and the actions: hold the branch, approve the
// pending major release, follow the client group again, update now. Policy
// changes answer 200 with the new status; "Update now" answers 202 with a job.
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  canUpdateCoreNow, coreApprove, coreDecisionText, coreFollowGroup, coreHold, hasOwnCorePolicy,
  type CorePolicyUpdate, type SiteCore,
} from '../../utils/site-settings'
import type { Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()
const emit = defineEmits<{ changed: [] }>()
const api = useApi()
const notify = useNotify()
const { can } = useCan()

const { data: loaded, refresh } = await useAsyncData(
  () => `site-core-${props.site.id}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/sites/{site_id}/core', { params: { path: { site_id: props.site.id } } })
      return { core: data ?? null, error: null as ApiError | null }
    } catch (e) {
      return { core: null, error: toApiError(e) }
    }
  },
)
// A finished job changes the version, so reload with the site.
watch(() => [props.site.id, props.site.status, props.site.wp_core_version], () => refresh())

const override = ref<SiteCore>()
const core = computed<SiteCore | undefined>(() => override.value ?? loaded.value?.core ?? undefined)
watch(() => loaded.value?.core, () => { override.value = undefined })
const loadError = computed(() => loaded.value?.error ?? undefined)

const busy = ref(false)
const failure = ref<ApiError>()

async function setPolicy(body: CorePolicyUpdate) {
  busy.value = true
  failure.value = undefined
  try {
    const { data } = await api.PUT('/api/v1/sites/{site_id}/core/policy', { params: { path: { site_id: props.site.id } }, body })
    if (data) override.value = data
    notify.success('Core policy saved', 'The rollout will pick it up at its next pass.')
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    busy.value = false
  }
}

const updateOpen = ref(false)
const updateError = ref<ApiError>()
async function updateNow() {
  busy.value = true
  updateError.value = undefined
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/core/update', { params: { path: { site_id: props.site.id } } })
    updateOpen.value = false
    if (data) notify.jobStarted(data.job)
    emit('changed')
  } catch (e) {
    updateError.value = toApiError(e)
  } finally {
    busy.value = false
  }
}

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="site-core-heading">
    <h2 id="site-core-heading" class="scroll-mt-20 text-lg font-semibold">WordPress core</h2>
    <ProblemAlert v-if="loadError" :error="loadError" />
    <p v-else-if="!core" class="text-sm text-muted" role="status">Loading core status...</p>
    <template v-else>
      <dl class="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
        <div>
          <dt class="text-muted">Version</dt>
          <dd data-testid="core-version">{{ core.version }} (branch {{ core.branch }})</dd>
        </div>
        <div>
          <dt class="text-muted">Next release</dt>
          <dd v-if="core.target" data-testid="core-target">{{ core.target.version }} ({{ core.target.kind }} release)</dd>
          <dd v-else>Up to date as far as its policy allows</dd>
        </div>
        <div v-if="core.pending" class="sm:col-span-2">
          <dt class="text-muted">Waiting</dt>
          <dd data-testid="core-pending">
            WordPress {{ core.pending.version }}
            <template v-if="core.pending.reason === 'held'">is not installed because the site is held on its branch.</template>
            <template v-else>waits for approval.</template>
            <template v-if="core.pending.auto_approve_at"> It is approved automatically on {{ when(core.pending.auto_approve_at) }}.</template>
          </dd>
        </div>
        <div class="sm:col-span-2">
          <dt class="text-muted">Policy</dt>
          <dd data-testid="core-decision">{{ coreDecisionText(core) }}</dd>
        </div>
      </dl>

      <ul v-if="core.warnings.length > 0" class="space-y-2" data-testid="core-warnings">
        <li v-for="w in core.warnings" :key="w.code" class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" role="alert">
          <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />{{ w.message }}
        </li>
      </ul>
      <div v-if="core.last_failure" class="rounded-md border border-default bg-elevated p-3 text-sm" role="alert" data-testid="core-failure">
        <p class="font-medium">The update to {{ core.last_failure.version }} failed on {{ when(core.last_failure.failed_at) }}</p>
        <p class="mt-1 whitespace-pre-line break-words text-muted">{{ core.last_failure.message }}</p>
      </div>

      <ProblemAlert v-if="failure" :error="failure" />

      <IfCan action="core_update.approve">
        <div class="flex flex-wrap gap-2">
          <UButton
            v-if="core.pending?.reason === 'awaiting_approval'"
            icon="i-lucide-check"
            :loading="busy"
            data-testid="core-approve"
            @click="setPolicy(coreApprove(core.pending.branch))"
          >
            Approve {{ core.pending.branch }}
          </UButton>
          <UButton
            v-if="!core.policy.hold"
            icon="i-lucide-pin"
            color="neutral"
            variant="outline"
            :disabled="busy"
            data-testid="core-hold"
            @click="setPolicy(coreHold())"
          >
            Hold on branch {{ core.branch }}
          </UButton>
          <UButton
            v-if="hasOwnCorePolicy(core)"
            icon="i-lucide-users"
            color="neutral"
            variant="outline"
            :disabled="busy"
            data-testid="core-follow"
            @click="setPolicy(coreFollowGroup())"
          >
            Follow the client group
          </UButton>
          <UButton
            v-if="canUpdateCoreNow(site, core)"
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="outline"
            :disabled="busy"
            data-testid="core-update"
            @click="updateError = undefined; updateOpen = true"
          >
            Update now
          </UButton>
        </div>
      </IfCan>
      <p v-if="!can('core_update.approve')" class="text-sm text-muted">Operators can hold or approve core updates.</p>

      <ConfirmDialog
        v-if="core.target"
        v-model:open="updateOpen"
        :title="`Update to WordPress ${core.target.version}?`"
        confirm-label="Update now"
        :busy="busy"
        :error="updateError"
        @confirm="updateNow()"
      >
        <p class="text-sm">
          A backup is taken first, then the site's containers are recreated on the new release and its database is upgraded. If the health check fails, the site goes back to the previous release. This runs now, outside any maintenance window.
        </p>
      </ConfirmDialog>
    </template>
  </section>
</template>
