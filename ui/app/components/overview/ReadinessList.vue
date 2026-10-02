<script setup lang="ts">
import { computed } from 'vue'
import { platformHealth, type Readiness } from '../../utils/overview'

const props = defineProps<{ readiness: Readiness | undefined }>()
const health = computed(() => platformHealth(props.readiness))
</script>

<template>
  <div class="space-y-3">
    <p class="flex items-center gap-2 font-medium" data-testid="health">
      <UIcon
        :name="health === 'healthy' ? 'i-lucide-circle-check' : 'i-lucide-circle-alert'"
        class="size-5"
        :class="health === 'healthy' ? 'text-success' : health === 'degraded' ? 'text-error' : 'text-muted'"
        aria-hidden="true"
      />
      <span v-if="health === 'healthy'">All checks pass</span>
      <span v-else-if="health === 'degraded'">A check is failing</span>
      <span v-else>Health unknown</span>
    </p>
    <ul v-if="readiness?.checks.length" class="divide-y divide-default rounded-lg border border-default text-sm">
      <li v-for="c in readiness.checks" :key="c.name" class="flex items-start justify-between gap-3 p-3">
        <span class="font-medium">{{ c.name }}</span>
        <span :class="c.status === 'ok' ? 'text-success' : 'text-error'">
          {{ c.status === 'ok' ? 'ok' : 'failed' }}<template v-if="c.error">: {{ c.error }}</template>
        </span>
      </li>
    </ul>
  </div>
</template>
