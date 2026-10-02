<script setup lang="ts">
// The audit log (design doc §4.6): every state-changing request, denial, and
// login, newest first, filterable by user, action, target, and time range,
// with cursor pagination. Admin only: the route is gated on `audit.view`, and
// the server answers 403 to anyone else regardless. Filters apply when the
// form is submitted, not on every keystroke.
import { auditQuery, emptyAuditFilters, targetTypes, validateAuditRange, type AuditEvent } from '../utils/audit'

definePageMeta({ requires: 'audit.view' })
useHead({ title: 'Audit log' })
useBreadcrumbs([{ label: 'Audit log' }])

const api = useApi()
const { userItems, userNames } = useUserLookup()

const ANY = 'any'
const form = reactive(emptyAuditFilters())
const applied = ref(emptyAuditFilters())
const rangeError = ref<string>()
const query = computed(() => auditQuery(applied.value))

const actorItems = computed(() => [{ label: 'Anyone', value: ANY }, ...userItems.value])
const targetItems = [{ label: 'Any target', value: ANY }, ...targetTypes.map(t => ({ label: t.label, value: t.value }))]
const actor = computed({ get: () => form.actorUserId || ANY, set: (v: string) => (form.actorUserId = v === ANY ? '' : v) })
const targetType = computed({ get: () => form.targetType || ANY, set: (v: string) => (form.targetType = v === ANY ? '' : v) })

const { items, cursor, loadError, pending, loadingMore, loadMore, refresh } = useCursorList<AuditEvent>(
  'audit-events',
  async c => ({ data: (await api.GET('/api/v1/audit-events', { params: { query: { limit: 50, ...query.value, cursor: c } } })).data }),
  [query],
)

function apply() {
  rangeError.value = validateAuditRange(form)
  if (rangeError.value) return
  applied.value = { ...form }
}

function clear() {
  Object.assign(form, emptyAuditFilters())
  rangeError.value = undefined
  applied.value = emptyAuditFilters()
}

const filtered = computed(() => Object.keys(query.value).length > 0)
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">Audit log</h1>
      <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="pending" @click="refresh()">Refresh</UButton>
    </div>

    <form class="grid gap-3 rounded-md border border-default p-4 sm:grid-cols-2 lg:grid-cols-3" novalidate @submit.prevent="apply">
      <UFormField label="User" name="actor">
        <USelect v-model="actor" :items="actorItems" class="w-full" />
      </UFormField>
      <UFormField label="Action" name="action" description="For example site.delete or self.login.">
        <UInput v-model="form.action" autocomplete="off" :maxlength="100" class="w-full" />
      </UFormField>
      <UFormField label="Target type" name="target_type">
        <USelect v-model="targetType" :items="targetItems" class="w-full" />
      </UFormField>
      <UFormField label="Target ID" name="target_id">
        <UInput v-model="form.targetId" autocomplete="off" class="w-full font-mono" />
      </UFormField>
      <UFormField label="From" name="since" :error="rangeError">
        <UInput v-model="form.since" type="datetime-local" class="w-full" />
      </UFormField>
      <UFormField label="Until" name="until">
        <UInput v-model="form.until" type="datetime-local" class="w-full" />
      </UFormField>
      <div class="flex items-end gap-2 sm:col-span-2 lg:col-span-3">
        <UButton type="submit" icon="i-lucide-filter">Apply filters</UButton>
        <UButton color="neutral" variant="ghost" @click="clear">Clear</UButton>
      </div>
    </form>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else>
      <p v-if="pending && items.length === 0" class="text-sm text-muted" role="status">Loading events...</p>
      <p v-else-if="items.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        {{ filtered ? 'No events match these filters.' : 'No events recorded yet, or they are older than the retention period.' }}
      </p>
      <AuditTable v-else :events="items" :names="userNames" caption="Audit events, newest first" />

      <div v-if="cursor" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="loadingMore" @click="loadMore()">Load more</UButton>
      </div>
    </template>
  </div>
</template>
