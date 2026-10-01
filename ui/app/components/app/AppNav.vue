<script setup lang="ts">
// The primary navigation, rendered from the registry in utils/nav.ts for the
// signed-in role. Live entries are links (the current page gets
// aria-current="page"); entries whose pages don't exist yet show dimmed with
// a "Soon" badge and are not focusable links. Used in the desktop sidebar and
// the mobile drawer.
import { groupBySection, isActive, isLive, navFor } from '../../utils/nav'

const { role } = useSession()
const route = useRoute()
const groups = computed(() => groupBySection(navFor(role.value)))
</script>

<template>
  <nav aria-label="Primary" class="space-y-5">
    <div v-for="group in groups" :key="group.section">
      <h2 class="mb-1 px-3 text-xs font-semibold uppercase tracking-wide text-muted">{{ group.label }}</h2>
      <ul class="space-y-0.5">
        <li v-for="item in group.items" :key="item.id">
          <NuxtLink
            v-if="isLive(item)"
            :to="item.route"
            class="flex min-h-10 items-center gap-3 rounded-md px-3 py-2 text-sm font-medium outline-none hover:bg-elevated focus-visible:ring-2 focus-visible:ring-primary"
            :class="isActive(item, route.path) ? 'bg-primary/10 text-primary' : 'text-default'"
            :aria-current="isActive(item, route.path) ? 'page' : undefined"
          >
            <UIcon :name="item.icon" class="size-5 shrink-0" aria-hidden="true" />
            {{ item.label }}
          </NuxtLink>
          <span
            v-else
            class="flex min-h-10 items-center gap-3 rounded-md px-3 py-2 text-sm text-dimmed"
            aria-disabled="true"
          >
            <UIcon :name="item.icon" class="size-5 shrink-0" aria-hidden="true" />
            <span class="flex-1">{{ item.label }}</span>
            <UBadge color="neutral" variant="subtle" size="sm">Soon</UBadge>
          </span>
        </li>
      </ul>
    </div>
  </nav>
</template>
