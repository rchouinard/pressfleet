<script setup lang="ts">
// The confirmation for deleting a group's backup repository (#139), worded
// as what it is: every backup of the group's sites is gone for good. The
// operator must type the group's name. The caller owns the request (see
// ConfirmDialog): it sets `busy`, passes a failure as `error`, and closes
// the dialog on success.
import { computed, ref, watch } from 'vue'
import type { ApiError } from '../../utils/api-error'
import { nameConfirmed, type ClientGroup } from '../../utils/client-group'

const props = defineProps<{
  group: Pick<ClientGroup, 'id' | 'name'>
  /** Where the backups can be exported first; omit to leave the link out. */
  exportHref?: string
  busy?: boolean
  error?: ApiError
}>()
const open = defineModel<boolean>('open', { required: true })
defineEmits<{ confirm: [] }>()

const typed = ref('')
// A fresh dialog each time: never carry a confirmation over.
watch(open, () => {
  typed.value = ''
})
const ok = computed(() => nameConfirmed(typed.value, props.group.name))
</script>

<template>
  <UModal v-model:open="open" :title="`Delete the backups of ${group.name}?`" description="This permanently deletes the group's backup repository." :dismissible="!busy">
    <template #body>
      <div class="space-y-3 text-sm">
        <p>
          This <strong>permanently deletes every backup</strong> of this group's sites, including those of deleted sites, together
          with the repository's encryption key. Nothing can restore them afterwards.
        </p>
        <p v-if="exportHref">
          If the client should keep any of them, <NuxtLink :to="exportHref" class="text-primary underline" @click="open = false">export them first</NuxtLink>:
          each export holds a site in the two files a site import takes. To hand over the whole repository instead, use the
          export command under "Command line".
        </p>
        <UFormField :label="`Type ${group.name} to confirm`">
          <UInput v-model="typed" autocomplete="off" :disabled="busy" class="w-full" />
        </UFormField>
        <ProblemAlert v-if="error" :error="error" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton color="error" :loading="busy" :disabled="!ok" data-testid="confirm" @click="$emit('confirm')">Delete all backups</UButton>
      </div>
    </template>
  </UModal>
</template>
