<script setup lang="ts">
// Edits the parts of a rule the dashboard exposes: name, description,
// severity, and for threshold rules the operator and threshold, plus how
// long the condition must hold. The expression, scope and labels stay
// API-only. Sends only what changed.
import { toApiError, type ApiError } from '../../utils/api-error'
import { ruleFormFrom, ruleUpdateBody, validateRuleForm, type AlertRule, type RuleFormState } from '../../utils/alert'

const props = defineProps<{ rule: AlertRule | null }>()
const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ saved: [rule: AlertRule] }>()

const api = useApi()
const notify = useNotify()

const form = ref<RuleFormState | null>(null)
const clientErrors = ref<Record<string, string>>({})
const apiError = ref<ApiError | null>(null)
const saving = ref(false)

watch(() => [open.value, props.rule] as const, ([isOpen, rule]) => {
  if (isOpen && rule) {
    form.value = ruleFormFrom(rule)
    clientErrors.value = {}
    apiError.value = null
  }
}, { immediate: true })

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

function fieldError(pointer: string): string | undefined {
  return clientErrors.value[pointer] ?? apiError.value?.fieldError(pointer)
}

async function save() {
  const rule = props.rule
  const f = form.value
  if (!rule || !f) return
  apiError.value = null
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
</script>

<template>
  <UModal v-model:open="open" :title="rule ? `Edit rule: ${rule.name}` : 'Edit rule'" description="Changes apply at the next evaluation, including to alerts that are firing.">
    <template #body>
      <form v-if="form && rule" id="alert-rule-form" class="space-y-4" novalidate @submit.prevent="save">
        <ProblemAlert v-if="apiError" :error="apiError" />
        <UFormField label="Name" name="name" required :error="fieldError('/name')">
          <UInput v-model="form.name" class="w-full" maxlength="100" />
        </UFormField>
        <UFormField label="Description" name="description" :error="fieldError('/description')">
          <UTextarea v-model="form.description" class="w-full" :rows="2" maxlength="1000" />
        </UFormField>
        <UFormField label="Severity" name="severity" :error="fieldError('/severity')">
          <USelect v-model="form.severity" :items="severityItems" class="w-full" />
        </UFormField>
        <template v-if="rule.kind === 'threshold'">
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Alert when the value" name="operator" :error="fieldError('/operator')">
              <USelect v-model="form.operator" :items="operatorItems" class="w-full" />
            </UFormField>
            <UFormField label="Threshold" name="threshold" required :error="fieldError('/threshold')">
              <UInput v-model="form.threshold" inputmode="decimal" class="w-full" />
            </UFormField>
          </div>
        </template>
        <p v-else class="text-sm text-muted">
          This rule is a PromQL expression that decides by itself when it is true, so it has no threshold to edit.
        </p>
        <UFormField label="Hold for (seconds)" name="for_seconds" required help="How long the condition must hold before the alert fires. 0 fires at once." :error="fieldError('/for_seconds')">
          <UInput v-model="form.forSeconds" inputmode="numeric" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="outline" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="alert-rule-form" :loading="saving">Save changes</UButton>
      </div>
    </template>
  </UModal>
</template>
