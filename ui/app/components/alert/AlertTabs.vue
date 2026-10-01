<script setup lang="ts">
// The sub-navigation of the alerts area: history, rules, channels. Plain
// links (not tabs widgets) because each is its own page.
const route = useRoute()
const links = [
  { label: 'History', to: '/alerts', icon: 'i-lucide-history' },
  { label: 'Rules', to: '/alerts/rules', icon: 'i-lucide-list-filter' },
  { label: 'Channels', to: '/alerts/channels', icon: 'i-lucide-send' },
]
// History is also the parent of /alerts/<id>; the other two match exactly.
function active(to: string): boolean {
  if (to === '/alerts') return route.path === '/alerts' || !/^\/alerts\/(rules|channels)\/?$/.test(route.path)
  return route.path === to || route.path === `${to}/`
}
</script>

<template>
  <nav aria-label="Alerts sections" class="flex flex-wrap gap-1 border-b border-default pb-2">
    <NuxtLink
      v-for="l in links"
      :key="l.to"
      :to="l.to"
      :aria-current="active(l.to) ? 'page' : undefined"
      class="inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium outline-none focus-visible:ring-2 focus-visible:ring-primary"
      :class="active(l.to) ? 'bg-elevated text-highlighted' : 'text-muted hover:bg-elevated hover:text-default'"
    >
      <UIcon :name="l.icon" class="size-4" aria-hidden="true" />
      {{ l.label }}
    </NuxtLink>
  </nav>
</template>
