<script setup lang="ts">
// Shown after signing in with a recovery code: each code works once, so the
// user learns how many are left and where to make new ones before going on.
// Nuxt-free, so it is unit-tested with mount.
import { computed, nextTick, onMounted, ref } from 'vue'

const props = defineProps<{
  remaining: number
  /** Where "Regenerate recovery codes" goes. */
  regenerateTo: string
}>()
defineEmits<{ continue: [] }>()

const low = computed(() => props.remaining <= 2)
const heading = ref<HTMLElement>()
// The step replaced the form, so focus would otherwise be lost.
onMounted(async () => {
  await nextTick()
  heading.value?.focus()
})
</script>

<template>
  <section class="space-y-4" aria-labelledby="recovery-notice-heading">
    <h2 id="recovery-notice-heading" ref="heading" tabindex="-1" class="text-lg font-semibold outline-none">
      You signed in with a recovery code
    </h2>
    <div
      role="status"
      class="flex gap-2 rounded-md border p-3 text-sm"
      :class="low ? 'border-warning bg-warning/10' : 'border-default bg-elevated'"
    >
      <UIcon :name="low ? 'i-lucide-triangle-alert' : 'i-lucide-info'" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <p v-if="remaining === 0">
        That was your last recovery code. Generate new ones now, so you can still sign in if you lose your other second factors.
      </p>
      <p v-else>
        You have {{ remaining }} recovery {{ remaining === 1 ? 'code' : 'codes' }} left. Each code works once.
        <template v-if="low">Generate new ones soon, so you can still sign in if you lose your other second factors.</template>
      </p>
    </div>
    <div class="flex flex-col gap-2">
      <UButton block :to="regenerateTo" icon="i-lucide-refresh-cw" :variant="low ? 'solid' : 'outline'">Regenerate recovery codes</UButton>
      <UButton block color="neutral" :variant="low ? 'outline' : 'solid'" @click="$emit('continue')">Continue</UButton>
    </div>
  </section>
</template>
