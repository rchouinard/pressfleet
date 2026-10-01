<script setup lang="ts">
// Renders its content only when the signed-in role may take `action`
// (utils/permissions.ts). The `denied` slot is for the rare control that
// should explain itself instead of disappearing. The rule is to hide what the
// role can't do; the server still answers 403.
import type { Action } from '../utils/permissions'

defineProps<{ action: Action }>()
const { can } = useCan()
</script>

<template>
  <slot v-if="can(action)" />
  <slot v-else name="denied" />
</template>
