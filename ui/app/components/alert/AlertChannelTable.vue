<script setup lang="ts">
// Alert channels: type, destination (never a secret: the API returns only
// the URL's scheme and host), what the channel receives, and status. Every
// role sees the list; the controls (test, edit, enable/disable, delete)
// appear only with `canManage` (admin: `platform.settings.manage`).
import { channelDestination, channelTypeLabel, type AlertChannel, type TestOutcome } from '../../utils/alert-channel'
import { severityView } from '../../utils/alert'

const props = defineProps<{
  channels: readonly AlertChannel[]
  canManage: boolean
  /** ID of the channel with a request in flight. */
  busyId?: string | null
  /** Latest test-send outcome per channel ID. */
  tests?: Readonly<Record<string, TestOutcome | undefined>>
  /** Names for the rule and client group limits, by ID. */
  ruleNames?: Readonly<Record<string, string>>
  groupNames?: Readonly<Record<string, string>>
  caption: string
}>()
defineEmits<{
  edit: [channel: AlertChannel]
  toggle: [channel: AlertChannel]
  delete: [channel: AlertChannel]
  test: [channel: AlertChannel]
}>()

function receives(c: AlertChannel): string {
  const parts = [`${severityView(c.min_severity).label} and above`]
  if (c.rule_id) parts.push(`rule: ${props.ruleNames?.[c.rule_id] ?? 'one rule'}`)
  if (c.client_group_id) parts.push(`group: ${props.groupNames?.[c.client_group_id] ?? 'one client group'}`)
  return parts.join('; ')
}
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Channel</th>
          <th scope="col" class="px-3 py-2 font-medium">Destination</th>
          <th scope="col" class="px-3 py-2 font-medium">Receives</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th v-if="canManage" scope="col" class="px-3 py-2 font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="c in channels" :key="c.id">
          <th scope="row" class="px-3 py-2 font-normal">
            <span class="font-medium">{{ c.name }}</span>
            <span class="block text-xs text-muted">{{ channelTypeLabel(c) }}</span>
          </th>
          <td class="max-w-xs break-words px-3 py-2 text-xs">
            {{ channelDestination(c) }}
            <span v-if="c.webhook?.has_signing_secret" class="block text-muted">Requests are signed</span>
          </td>
          <td class="px-3 py-2 text-xs">{{ receives(c) }}</td>
          <td class="px-3 py-2">
            <AlertBadge :view="c.enabled ? { label: 'Enabled', color: 'success', icon: 'i-lucide-check' } : { label: 'Disabled', color: 'neutral', icon: 'i-lucide-ban' }" />
            <p v-if="tests?.[c.id]" class="mt-1 text-xs" role="status">
              <template v-if="tests[c.id]!.state === 'testing'">Sending test...</template>
              <template v-else-if="tests[c.id]!.success">Test delivered in {{ tests[c.id]!.durationMs }} ms.</template>
              <template v-else>Test failed: {{ tests[c.id]!.error || 'no reason given' }}</template>
            </p>
          </td>
          <td v-if="canManage" class="whitespace-nowrap px-3 py-2 text-right">
            <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-send" :loading="tests?.[c.id]?.state === 'testing'" :disabled="busyId === c.id" :aria-label="`Send a test through ${c.name}`" @click="$emit('test', c)">Test</UButton>
            <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-pencil" :disabled="busyId === c.id" :aria-label="`Edit ${c.name}`" @click="$emit('edit', c)">Edit</UButton>
            <UButton color="neutral" variant="ghost" size="sm" :loading="busyId === c.id" :aria-label="`${c.enabled ? 'Disable' : 'Enable'} ${c.name}`" @click="$emit('toggle', c)">
              {{ c.enabled ? 'Disable' : 'Enable' }}
            </UButton>
            <UButton color="error" variant="ghost" size="sm" icon="i-lucide-trash-2" :disabled="busyId === c.id" :aria-label="`Delete ${c.name}`" @click="$emit('delete', c)">Delete</UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
