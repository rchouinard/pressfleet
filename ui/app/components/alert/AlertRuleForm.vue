<script setup lang="ts">
// Creates a rule, or edits the parts of one the dashboard exposes: name,
// description, severity, whether a site rule includes staging sites, and for threshold rules the operator and
// threshold, plus how long the condition must hold. A new rule also takes
// its scope, kind, PromQL expression and client group; once it exists they
// stay API-only, as do labels and annotations. An edit sends only what
// changed.
import { toApiError, type ApiError } from '../../utils/api-error'
import { ANY } from '../../utils/alert-channel'
import {
  emptyNewRuleForm, ruleCreateBody, ruleFormFrom, ruleUpdateBody, validateNewRuleForm, validateRuleForm,
  type AlertRule, type NewRuleFormState, type RuleFormState,
} from '../../utils/alert'

const props = defineProps<{
  /** The rule to edit, or null to create one. */
  rule: AlertRule | null
  /** Client groups a new site rule can be limited to. */
  groupItems?: { label: string, value: string }[]
}>()
const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ saved: [rule: AlertRule] }>()

const api = useApi()
const notify = useNotify()

const form = ref<RuleFormState | null>(null)
// The same object as `form` while creating; null while editing.
const created = ref<NewRuleFormState | null>(null)
const clientErrors = ref<Record<string, string>>({})
const apiError = ref<ApiError | null>(null)
const saving = ref(false)

watch(() => [open.value, props.rule] as const, ([isOpen, rule]) => {
  if (isOpen) {
    created.value = rule ? null : emptyNewRuleForm()
    form.value = rule ? ruleFormFrom(rule) : created.value
    clientErrors.value = {}
    apiError.value = null
  }
}, { immediate: true })

const kind = computed(() => props.rule?.kind ?? created.value?.kind)
const scope = computed(() => props.rule?.scope ?? created.value?.scope)

const severityItems = [
  { label: 'Info', value: 'info' },
  { label: 'Warning', value: 'warning' },
  { label: 'Critical', value: 'critical' },
]
const operatorItems = [
  { label: 'is greater than', value: 'gt' },
  { label: 'is at least', value: 'gte' },
  { label: 'is less than', value: 'lt' },
  { label: 'is at most', value: 'lte' },
  { label: 'equals', value: 'eq' },
  { label: 'does not equal', value: 'ne' },
]
const scopeItems = [
  { label: 'Every site (one alert per site)', value: 'site' },
  { label: 'Platform', value: 'platform' },
]
const kindItems = [
  { label: 'Compare the value with a threshold', value: 'threshold' },
  { label: 'The expression decides by itself', value: 'expression' },
]
const anyGroup = computed(() => [{ label: 'Any client group', value: ANY }, ...(props.groupItems ?? [])])
// A select can't hold "no value", so "any client group" is a sentinel.
const groupModel = computed({
  get: () => created.value?.clientGroupId || ANY,
  set: (v: string) => {
    if (created.value) created.value.clientGroupId = v === ANY ? '' : v
  },
})

function fieldError(pointer: string): string | undefined {
  return clientErrors.value[pointer] ?? apiError.value?.fieldError(pointer)
}

async function save() {
  const rule = props.rule
  const f = form.value
  if (!f) return
  apiError.value = null
  if (!rule) {
    await create()
    return
  }
  clientErrors.value = validateRuleForm(rule, f)
  if (Object.keys(clientErrors.value).length) return
  const body = ruleUpdateBody(rule, f)
  if (Object.keys(body).length === 0) {
    open.value = false
    return
  }
  saving.value = true
  try {
    const { data } = await api.PATCH('/api/v1/alert-rules/{alert_rule_id}', {
      params: { path: { alert_rule_id: rule.id } },
      body,
    })
    notify.success('Rule updated', data?.name)
    if (data) emit('saved', data)
    open.value = false
  } catch (e) {
    apiError.value = toApiError(e)
  } finally {
    saving.value = false
  }
}

async function create() {
  const f = created.value
  if (!f) return
  clientErrors.value = validateNewRuleForm(f)
  if (Object.keys(clientErrors.value).length) return
  saving.value = true
  try {
    const { data } = await api.POST('/api/v1/alert-rules', { body: ruleCreateBody(f) })
    notify.success('Rule created', data?.name)
    if (data) emit('saved', data)
    open.value = false
  } catch (e) {
    apiError.value = toApiError(e)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="rule ? `Edit rule: ${rule.name}` : 'New alert rule'"
    :description="rule ? 'Changes apply at the next evaluation, including to alerts that are firing.' : 'A rule evaluates a PromQL expression against the metrics store and raises an alert while its condition holds.'"
  >
    <template #body>
      <form v-if="form" id="alert-rule-form" class="space-y-4" novalidate @submit.prevent="save">
        <ProblemAlert v-if="apiError" :error="apiError" />
        <UFormField label="Name" name="name" required :error="fieldError('/name')">
          <UInput v-model="form.name" class="w-full" maxlength="100" />
        </UFormField>
        <UFormField label="Description" name="description" :error="fieldError('/description')">
          <UTextarea v-model="form.description" class="w-full" :rows="2" maxlength="1000" />
        </UFormField>
        <template v-if="created">
          <UFormField label="Scope" name="scope" help="A site rule's series must carry a site_id label; series without one are ignored." :error="fieldError('/scope')">
            <USelect v-model="created.scope" :items="scopeItems" class="w-full" />
          </UFormField>
          <UFormField v-if="created.scope === 'site'" label="Client group" name="client_group_id" :error="fieldError('/client_group_id')">
            <USelect v-model="groupModel" :items="anyGroup" class="w-full" />
          </UFormField>
          <UFormField label="Expression" name="expr" required help="PromQL, checked by the metrics store when the rule is evaluated." :error="fieldError('/expr')">
            <UTextarea v-model="created.expr" class="w-full font-mono" :rows="2" maxlength="4096" placeholder="pressfleet_site_up" />
          </UFormField>
          <UFormField label="Condition" name="kind" :error="fieldError('/kind')">
            <USelect v-model="created.kind" :items="kindItems" class="w-full" />
          </UFormField>
        </template>
        <UFormField label="Severity" name="severity" :error="fieldError('/severity')">
          <USelect v-model="form.severity" :items="severityItems" class="w-full" />
        </UFormField>
        <template v-if="kind === 'threshold'">
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Alert when the value" name="operator" :error="fieldError('/operator')">
              <USelect v-model="form.operator" :items="operatorItems" class="w-full" />
            </UFormField>
            <UFormField label="Threshold" name="threshold" required :error="fieldError('/threshold')">
              <UInput v-model="form.threshold" inputmode="decimal" class="w-full" />
            </UFormField>
          </div>
        </template>
        <p v-else-if="rule" class="text-sm text-muted">
          This rule is a PromQL expression that decides by itself when it is true, so it has no threshold to edit.
        </p>
        <UCheckbox
          v-if="scope === 'site'"
          v-model="form.includeStaging"
          label="Include staging sites"
          description="Staging sites are disposable and often idle or stopped, so rules skip them unless you opt in."
        />
        <UFormField label="Hold for (seconds)" name="for_seconds" required help="How long the condition must hold before the alert fires. 0 fires at once." :error="fieldError('/for_seconds')">
          <UInput v-model="form.forSeconds" inputmode="numeric" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="outline" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="alert-rule-form" :loading="saving">{{ rule ? 'Save changes' : 'Create rule' }}</UButton>
      </div>
    </template>
  </UModal>
</template>
