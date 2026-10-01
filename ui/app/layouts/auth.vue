<script setup lang="ts">
// The frame for pages that need no session (login): a centred column with the
// product name, the same landmarks and skip link, and the offline banner.
</script>

<template>
  <div class="flex min-h-dvh flex-col bg-default text-default">
    <a
      href="#main"
      class="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-50 focus:rounded-md focus:bg-default focus:px-3 focus:py-2 focus:text-sm focus:font-medium focus:shadow-lg focus:ring-2 focus:ring-primary"
    >
      Skip to main content
    </a>
    <AppNetworkBanner />
    <main id="main" tabindex="-1" class="mx-auto flex w-full max-w-sm flex-1 flex-col justify-center gap-6 px-4 py-10 outline-none">
      <div class="flex items-center gap-2 text-xl font-semibold tracking-tight">
        <UIcon name="i-lucide-server" class="size-6 text-primary" aria-hidden="true" />
        PressFleet
      </div>
      <slot />
    </main>
  </div>
</template>
