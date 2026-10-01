<script setup lang="ts">
// The app shell for every signed-in page: skip link, header with the product
// name and user menu, a sidebar on large screens that becomes a drawer below
// `lg`, breadcrumbs, and the main region. Toasts are rendered by <UApp>.
// Pages put their own <h1> in the slot and set crumbs with useBreadcrumbs().
const route = useRoute()
const crumbs = useState<Crumb[]>('breadcrumbs', () => [])
const drawer = ref(false)

// Close the drawer after navigating from it.
watch(() => route.fullPath, () => {
  drawer.value = false
})
</script>

<template>
  <div class="flex min-h-dvh flex-col bg-default text-default">
    <a
      href="#main"
      class="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-50 focus:rounded-md focus:bg-default focus:px-3 focus:py-2 focus:text-sm focus:font-medium focus:shadow-lg focus:ring-2 focus:ring-primary"
    >
      Skip to main content
    </a>

    <header class="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-default bg-default/95 px-3 backdrop-blur sm:px-4">
      <UButton
        class="lg:hidden"
        color="neutral"
        variant="ghost"
        icon="i-lucide-menu"
        aria-label="Open navigation"
        :aria-expanded="drawer"
        @click="drawer = true"
      />
      <NuxtLink to="/" class="flex items-center gap-2 rounded-md px-1 text-base font-semibold tracking-tight outline-none focus-visible:ring-2 focus-visible:ring-primary">
        <UIcon name="i-lucide-server" class="size-5 text-primary" aria-hidden="true" />
        PressFleet
      </NuxtLink>
      <div class="flex-1" />
      <AppUserMenu />
    </header>

    <AppNetworkBanner />

    <div class="flex flex-1">
      <aside class="hidden w-60 shrink-0 border-r border-default p-3 lg:block">
        <div class="sticky top-[4.25rem] max-h-[calc(100dvh-5rem)] overflow-y-auto">
          <AppNav />
        </div>
      </aside>

      <USlideover
        v-model:open="drawer"
        side="left"
        title="Navigation"
        description="Go to a section of the dashboard"
        :ui="{ content: 'max-w-72', header: 'sr-only' }"
        class="lg:hidden"
      >
        <template #body>
          <AppNav />
        </template>
      </USlideover>

      <div class="min-w-0 flex-1">
        <main id="main" tabindex="-1" class="mx-auto w-full max-w-6xl px-4 py-6 outline-none sm:px-6">
          <nav v-if="crumbs.length" aria-label="Breadcrumb" class="mb-4">
            <ol class="flex flex-wrap items-center gap-1.5 text-sm text-muted">
              <li class="flex items-center gap-1.5">
                <NuxtLink to="/" class="rounded underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">Dashboard</NuxtLink>
              </li>
              <li v-for="(crumb, i) in crumbs" :key="i" class="flex min-w-0 items-center gap-1.5">
                <UIcon name="i-lucide-chevron-right" class="size-3.5 shrink-0" aria-hidden="true" />
                <NuxtLink
                  v-if="crumb.to"
                  :to="crumb.to"
                  class="rounded underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary"
                >
                  {{ crumb.label }}
                </NuxtLink>
                <span v-else aria-current="page" class="truncate font-medium text-default">{{ crumb.label }}</span>
              </li>
            </ol>
          </nav>
          <slot />
        </main>
      </div>
    </div>
  </div>
</template>
