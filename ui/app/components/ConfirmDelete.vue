<script setup lang="ts">
// A confirmation dialog for a destructive action. Focus starts on Cancel
// (the dialog's first focusable control), so Enter doesn't delete by accident.
defineProps<{ title: string, description: string, confirmLabel: string, loading?: boolean }>()
const open = defineModel<boolean>('open', { required: true })
defineEmits<{ confirm: [] }>()
</script>

<template>
  <UModal v-model:open="open" :title="title" :description="description">
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="outline" @click="open = false">Cancel</UButton>
        <UButton color="error" :loading="loading" @click="$emit('confirm')">{{ confirmLabel }}</UButton>
      </div>
    </template>
  </UModal>
</template>
