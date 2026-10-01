<script setup lang="ts">
// Alert rules: name, scope, condition, duration, severity, enabled. Controls
// (edit, enable/disable, delete) appear only with `canManage`, which the page
// takes from the `alert_rule.manage` permission; the server stays the
// authority and answers 403 either way.
import { formatForSeconds, ruleCondition, severityView, type AlertRule } from '../../utils/alert'

defineProps<{
  rules: readonly AlertRule[]
  canManage: boolean
  /** ID of the rule with a request in flight. */
  busyId?: string | null
  caption: string
}>()
defineEmits<{
  edit: [rule: AlertRule]
  toggle: [rule: AlertRule]
  delete: [rule: AlertRule]
}>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Rule</th>
          <th scope="col" class="px-3 py-2 font-medium">Scope</th>
          <th scope="col" class="px-3 py-2 font-medium">Condition</th>
          <th scope="col" class="px-3 py-2 font-medium">For</th>
          <th scope="col" class="px-3 py-2 font-medium">Severity</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th v-if="canManage" scope="col" class="px-3 py-2 font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="r in rules" :key="r.id">
          <th scope="row" class="px-3 py-2 font-normal">
            <span class="font-medium">{{ r.name }}</span>
            <UBadge v-if="r.builtin" color="neutral" variant="outline" size="sm" class="ml-1">Built-in</UBadge>
            <span v-if="r.description" class="block max-w-prose text-xs text-muted">{{ r.description }}</span>
          </th>
          <td class="px-3 py-2">{{ r.scope === 'site' ? (r.client_group_id ? 'Sites in one client group' : 'Every site') : 'Platform' }}</td>
          <td class="whitespace-nowrap px-3 py-2 font-mono text-xs">{{ ruleCondition(r) }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatForSeconds(r.for_seconds) }}</td>
          <td class="px-3 py-2"><AlertBadge :view="severityView(r.severity)" /></td>
          <td class="px-3 py-2">
            <AlertBadge :view="r.enabled ? { label: 'Enabled', color: 'success', icon: 'i-lucide-check' } : { label: 'Disabled', color: 'neutral', icon: 'i-lucide-ban' }" />
          </td>
          <td v-if="canManage" class="whitespace-nowrap px-3 py-2 text-right">
            <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-pencil" :aria-label="`Edit ${r.name}`" :disabled="busyId === r.id" @click="$emit('edit', r)">Edit</UButton>
            <UButton color="neutral" variant="ghost" size="sm" :loading="busyId === r.id" :aria-label="`${r.enabled ? 'Disable' : 'Enable'} ${r.name}`" @click="$emit('toggle', r)">
              {{ r.enabled ? 'Disable' : 'Enable' }}
            </UButton>
            <UButton v-if="!r.builtin" color="error" variant="ghost" size="sm" icon="i-lucide-trash-2" :aria-label="`Delete ${r.name}`" :disabled="busyId === r.id" @click="$emit('delete', r)">Delete</UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
