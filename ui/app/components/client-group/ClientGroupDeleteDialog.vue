<script setup lang="ts">
// The confirmation for deleting a client group. A group with a backup
// repository can't be deleted on its own (#139): the dialog says so, shows
// the group's ID and the commands that deal with the repository, links to
// the group's page to export backups first, and, for callers who may
// offboard, offers to delete the repository in the same step, which takes
// typing the group's name. The caller owns the request (see ConfirmDialog):
// `confirm` carries whether to delete the repository too.
import { computed, ref, watch } from 'vue'
import type { ApiError } from '../../utils/api-error'
import { nameConfirmed, siteCountLabel, type ClientGroup } from '../../utils/client-group'
import ClientGroupRepositoryCommands from './ClientGroupRepositoryCommands.vue'

const props = defineProps<{
  group?: Pick<ClientGroup, 'id' | 'name' | 'site_count' | 'backup_repository'>
  /** Whether the caller may delete the backups (client_group.offboard). */
  canOffboard: boolean
  busy?: boolean
  error?: ApiError
}>()
const open = defineModel<boolean>('open', { required: true })
defineEmits<{ confirm: [withRepository: boolean] }>()

const withRepository = ref(false)
const typed = ref('')
watch(open, () => {
  withRepository.value = false
  typed.value = ''
})

const name = computed(() => props.group?.name ?? 'client group')
const hasSites = computed(() => (props.group?.site_count ?? 0) > 0)
// The server may have seen a repository the list didn't (one created since).
const hasRepository = computed(() => !!props.group?.backup_repository
  || (props.error?.status === 409 && /backup repository/.test(props.error.detail ?? '')))
const ok = computed(() => {
  if (hasSites.value) return false
  if (!hasRepository.value) return true
  return withRepository.value && nameConfirmed(typed.value, name.value)
})
</script>

<template>
  <UModal v-model:open="open" :title="`Delete ${name}?`" description="This can't be undone." :dismissible="!busy">
    <template #body>
      <div class="space-y-3 text-sm">
        <p v-if="hasSites">
          The group still has {{ siteCountLabel(group?.site_count ?? 0) }}. Delete them first.
        </p>
        <template v-else-if="hasRepository && group">
          <p>
            The group still has a backup repository with the backups of its sites. A group can't be deleted while its backups exist,
            so they are never lost by accident.
          </p>
          <p>
            If the client should keep any of them,
            <NuxtLink :to="`/client-groups/${group.id}`" class="text-primary underline" @click="open = false">export them from the group's page</NuxtLink>
            first.
          </p>
          <template v-if="canOffboard">
            <UCheckbox v-model="withRepository" :disabled="busy" label="Also permanently delete the backup repository and every backup in it" />
            <UFormField v-if="withRepository" :label="`Type ${group.name} to confirm`">
              <UInput v-model="typed" autocomplete="off" :disabled="busy" class="w-full" />
            </UFormField>
          </template>
          <details class="rounded-md border border-default p-3">
            <summary class="cursor-pointer font-medium">Command line</summary>
            <div class="mt-3">
              <ClientGroupRepositoryCommands :group-id="group.id" />
            </div>
          </details>
        </template>
        <ProblemAlert v-if="error" :error="error" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton color="error" :loading="busy" :disabled="!ok" data-testid="confirm" @click="$emit('confirm', hasRepository && withRepository)">
          {{ hasRepository && withRepository ? 'Delete group and backups' : 'Delete' }}
        </UButton>
      </div>
    </template>
  </UModal>
</template>
