<script setup lang="ts">
// Alert channels and the delivery history. Every role can read both; only
// admins (`platform.settings.manage`) create, edit, delete, enable/disable
// and test-send. Credentials are write-only: the list shows a webhook's
// scheme and host and whether secrets are set, never the secrets.
import { ANY, type AlertChannel, type TestOutcome } from '../../utils/alert-channel'
import type { AlertDelivery, AlertDeliveryState } from '../../utils/alert'

useHead({ title: 'Alert channels' })
useBreadcrumbs([{ label: 'Alerts', to: '/alerts' }, { label: 'Channels' }])

const api = useApi()
const notify = useNotify()
const toast = useToast()
const { can } = useCan()
const canManage = computed(() => can('platform.settings.manage'))
const { ruleItems, groupItems, ruleNames, groupNames } = useAlertLookups()

const channels = useCursorList<AlertChannel>(
  'alert-channels-list',
  cursor => api.GET('/api/v1/alert-channels', { params: { query: { limit: 50, cursor } } }),
)

function replace(c: AlertChannel) {
  const i = channels.items.value.findIndex(x => x.id === c.id)
  if (i >= 0) channels.items.value[i] = c
  else void channels.refresh()
}

// Form, delete, toggle, test

const formChannel = ref<AlertChannel | null>(null)
const formOpen = ref(false)
function openForm(channel: AlertChannel | null) {
  formChannel.value = channel
  formOpen.value = true
}

const busyId = ref<string | null>(null)
async function toggle(c: AlertChannel) {
  busyId.value = c.id
  try {
    const path = c.enabled ? '/api/v1/alert-channels/{alert_channel_id}/disable' : '/api/v1/alert-channels/{alert_channel_id}/enable'
    const { data } = await api.POST(path, { params: { path: { alert_channel_id: c.id } } })
    if (data) replace(data)
    notify.success(`Channel ${c.enabled ? 'disabled' : 'enabled'}`, c.name)
  } catch (e) {
    notify.error(e)
  } finally {
    busyId.value = null
  }
}

const deleting = ref<AlertChannel | null>(null)
const deleteOpen = ref(false)
const deleteBusy = ref(false)
function askDelete(c: AlertChannel) {
  deleting.value = c
  deleteOpen.value = true
}
async function confirmDelete() {
  const c = deleting.value
  if (!c) return
  deleteBusy.value = true
  try {
    await api.DELETE('/api/v1/alert-channels/{alert_channel_id}', { params: { path: { alert_channel_id: c.id } } })
    channels.items.value = channels.items.value.filter(x => x.id !== c.id)
    notify.success('Channel deleted', c.name)
    deleteOpen.value = false
    void deliveries.refresh()
  } catch (e) {
    notify.error(e)
  } finally {
    deleteBusy.value = false
  }
}

const tests = ref<Record<string, TestOutcome | undefined>>({})
async function sendTest(c: AlertChannel) {
  tests.value[c.id] = { state: 'testing' }
  try {
    const { data } = await api.POST('/api/v1/alert-channels/{alert_channel_id}/test', { params: { path: { alert_channel_id: c.id } } })
    // A channel that fails the test is still a 200: read `success`.
    tests.value[c.id] = { state: 'done', success: data?.success ?? false, error: data?.error ?? null, durationMs: data?.duration_ms }
    if (data?.success) notify.success('Test notification delivered', `${c.name} accepted it in ${data.duration_ms} ms.`)
    else toast.add({ title: `${c.name} did not accept the test`, description: data?.error || 'No reason given.', icon: 'i-lucide-circle-alert', color: 'error', duration: 10000 })
  } catch (e) {
    tests.value[c.id] = undefined
    notify.error(e)
  }
}

// Delivery history

const deliveryState = ref<string>(ANY)
const deliveryChannel = ref<string>(ANY)
const stateItems = [
  { label: 'Any state', value: ANY },
  { label: 'Failed', value: 'failed' },
  { label: 'Retrying', value: 'pending' },
  { label: 'Delivered', value: 'delivered' },
]
const channelItems = computed(() => [{ label: 'Any channel', value: ANY }, ...channels.items.value.map(c => ({ label: c.name, value: c.id }))])
const deliveryQuery = computed(() => ({
  limit: 25,
  state: deliveryState.value === ANY ? undefined : (deliveryState.value as AlertDeliveryState),
  channel_id: deliveryChannel.value === ANY ? undefined : deliveryChannel.value,
}))
const deliveries = useCursorList<AlertDelivery>(
  'alert-deliveries-list',
  cursor => api.GET('/api/v1/alert-deliveries', { params: { query: { ...deliveryQuery.value, cursor } } }),
  [deliveryQuery],
)
</script>

<template>
  <div class="space-y-6">
    <h1 class="text-2xl font-semibold tracking-tight">Alerts</h1>
    <AlertTabs />

    <section aria-labelledby="channels-heading" class="space-y-3">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 id="channels-heading" class="text-lg font-medium">Channels</h2>
        <div class="flex gap-2">
          <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="channels.pending.value" @click="channels.refresh()">Refresh</UButton>
          <UButton v-if="canManage" icon="i-lucide-plus" @click="openForm(null)">New channel</UButton>
        </div>
      </div>
      <p v-if="!canManage" class="text-sm text-muted">Your role can view channels but not change them. Ask an admin.</p>

      <ProblemAlert v-if="channels.loadError.value" :error="channels.loadError.value" />
      <template v-else>
        <p v-if="channels.pending.value && channels.items.value.length === 0" class="text-sm text-muted" role="status">Loading channels...</p>
        <p v-else-if="channels.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
          No channels yet, so alerts are recorded here but nobody is notified.
          <template v-if="canManage">Add an email or webhook channel to start.</template>
        </p>
        <AlertChannelTable
          v-else
          :channels="channels.items.value"
          :can-manage="canManage"
          :busy-id="busyId"
          :tests="tests"
          :rule-names="ruleNames"
          :group-names="groupNames"
          caption="Alert channels"
          @edit="openForm"
          @toggle="toggle"
          @delete="askDelete"
          @test="sendTest"
        />
        <div v-if="channels.cursor.value" class="flex justify-center">
          <UButton color="neutral" variant="outline" :loading="channels.loadingMore.value" @click="channels.loadMore()">Load more</UButton>
        </div>
      </template>
    </section>

    <section aria-labelledby="deliveries-heading" class="space-y-3">
      <div class="flex flex-wrap items-end justify-between gap-3">
        <h2 id="deliveries-heading" class="text-lg font-medium">Delivery history</h2>
        <div class="flex flex-wrap items-end gap-3">
          <UFormField label="State" name="delivery-state">
            <USelect v-model="deliveryState" :items="stateItems" class="w-40" />
          </UFormField>
          <UFormField label="Channel" name="delivery-channel">
            <USelect v-model="deliveryChannel" :items="channelItems" class="w-52" />
          </UFormField>
          <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="deliveries.pending.value" @click="deliveries.refresh()">Refresh</UButton>
        </div>
      </div>
      <ProblemAlert v-if="deliveries.loadError.value" :error="deliveries.loadError.value" />
      <template v-else>
        <p v-if="deliveries.pending.value && deliveries.items.value.length === 0" class="text-sm text-muted" role="status">Loading deliveries...</p>
        <p v-else-if="deliveries.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
          No deliveries match. They appear when an alert fires or resolves and a channel wants it. Test sends are not recorded here.
        </p>
        <AlertDeliveryTable v-else :deliveries="deliveries.items.value" link-alerts caption="Alert deliveries, newest first" />
        <div v-if="deliveries.cursor.value" class="flex justify-center">
          <UButton color="neutral" variant="outline" :loading="deliveries.loadingMore.value" @click="deliveries.loadMore()">Load more</UButton>
        </div>
      </template>
    </section>

    <template v-if="canManage">
      <AlertChannelForm v-model:open="formOpen" :channel="formChannel" :rule-items="ruleItems" :group-items="groupItems" @saved="replace" />
      <ConfirmDelete
        v-model:open="deleteOpen"
        title="Delete this channel?"
        :description="deleting ? `${deleting.name}, its stored credentials and its delivery history will be deleted. This cannot be undone.` : ''"
        confirm-label="Delete channel"
        :loading="deleteBusy"
        @confirm="confirmDelete"
      />
    </template>
  </div>
</template>
