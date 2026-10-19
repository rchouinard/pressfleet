<script setup lang="ts">
// Alert rules. Everyone can read them; operators and admins
// (`alert_rule.manage`) can create rules, edit and enable/disable any rule,
// and delete custom ones. A new rule needs a PromQL expression; labels and
// annotations stay API-only.
import type { AlertRule } from '../../utils/alert'
import { ANY } from '../../utils/alert-channel'

useHead({ title: 'Alert rules' })
useBreadcrumbs([{ label: 'Alerts', to: '/alerts' }, { label: 'Rules' }])

const api = useApi()
const notify = useNotify()
const { can } = useCan()
const canManage = computed(() => can('alert_rule.manage'))
const { groupItems } = useAlertLookups()

const enabledFilter = ref<string>(ANY)
const enabledItems = [
  { label: 'Enabled and disabled', value: ANY },
  { label: 'Enabled', value: 'true' },
  { label: 'Disabled', value: 'false' },
]
const query = computed(() => ({ limit: 50, enabled: enabledFilter.value === ANY ? undefined : enabledFilter.value === 'true' }))
const list = useCursorList<AlertRule>(
  'alert-rules-list',
  cursor => api.GET('/api/v1/alert-rules', { params: { query: { ...query.value, cursor } } }),
  [query],
)

function replace(rule: AlertRule) {
  const i = list.items.value.findIndex(r => r.id === rule.id)
  if (i >= 0) list.items.value[i] = rule
  // A new rule: the list is ordered by name, so let the server place it.
  else void list.refresh()
}

const busyId = ref<string | null>(null)
async function toggle(rule: AlertRule) {
  busyId.value = rule.id
  try {
    const path = rule.enabled ? '/api/v1/alert-rules/{alert_rule_id}/disable' : '/api/v1/alert-rules/{alert_rule_id}/enable'
    const { data } = await api.POST(path, { params: { path: { alert_rule_id: rule.id } } })
    if (data) replace(data)
    notify.success(`Rule ${rule.enabled ? 'disabled' : 'enabled'}`, rule.name)
    if (enabledFilter.value !== ANY) await list.refresh()
  } catch (e) {
    notify.error(e)
  } finally {
    busyId.value = null
  }
}

const editing = ref<AlertRule | null>(null)
const editOpen = ref(false)
function edit(rule: AlertRule | null) {
  editing.value = rule
  editOpen.value = true
}

const deleting = ref<AlertRule | null>(null)
const deleteOpen = ref(false)
const deleteBusy = ref(false)
function askDelete(rule: AlertRule) {
  deleting.value = rule
  deleteOpen.value = true
}
async function confirmDelete() {
  const rule = deleting.value
  if (!rule) return
  deleteBusy.value = true
  try {
    await api.DELETE('/api/v1/alert-rules/{alert_rule_id}', { params: { path: { alert_rule_id: rule.id } } })
    list.items.value = list.items.value.filter(r => r.id !== rule.id)
    notify.success('Rule deleted', rule.name)
    deleteOpen.value = false
  } catch (e) {
    notify.error(e)
  } finally {
    deleteBusy.value = false
  }
}
</script>

<template>
  <div class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">Alerts</h1>
    <AlertTabs />

    <div class="flex flex-wrap items-end justify-between gap-3">
      <UFormField label="Show" name="enabled">
        <USelect v-model="enabledFilter" :items="enabledItems" class="w-52" />
      </UFormField>
      <div class="flex gap-2">
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="list.pending.value" @click="list.refresh()">Refresh</UButton>
        <UButton v-if="canManage" icon="i-lucide-plus" @click="edit(null)">New rule</UButton>
      </div>
    </div>
    <p v-if="!canManage" class="text-sm text-muted">Your role can view rules but not change them.</p>

    <ProblemAlert v-if="list.loadError.value" :error="list.loadError.value" />

    <template v-else>
      <p v-if="list.pending.value && list.items.value.length === 0" class="text-sm text-muted" role="status">Loading rules...</p>
      <p v-else-if="list.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        No rules match. The built-in rules are created when the control plane starts.
      </p>
      <AlertRuleTable
        v-else
        :rules="list.items.value"
        :can-manage="canManage"
        :busy-id="busyId"
        caption="Alert rules"
        @edit="edit"
        @toggle="toggle"
        @delete="askDelete"
      />
      <div v-if="list.cursor.value" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="list.loadingMore.value" @click="list.loadMore()">Load more</UButton>
      </div>
    </template>

    <AlertRuleForm v-if="canManage" v-model:open="editOpen" :rule="editing" :group-items="groupItems" @saved="replace" />
    <ConfirmDelete
      v-if="canManage"
      v-model:open="deleteOpen"
      title="Delete this rule?"
      :description="deleting ? `${deleting.name} and the alerts it raised will be deleted. This cannot be undone.` : ''"
      confirm-label="Delete rule"
      :loading="deleteBusy"
      @confirm="confirmDelete"
    />
  </div>
</template>
