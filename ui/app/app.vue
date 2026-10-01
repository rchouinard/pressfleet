<script setup lang="ts">
// "Jobs - PressFleet"; pages set only their own title.
useHead({ titleTemplate: title => (title && title !== 'PressFleet' ? `${title} - PressFleet` : 'PressFleet') })
</script>

<template>
  <UApp :toaster="{ position: 'bottom-right', expand: true }">
    <!-- Announces the new page title to screen readers after each navigation. -->
    <NuxtRouteAnnouncer />
    <NuxtLayout>
      <NuxtPage />
    </NuxtLayout>
  </UApp>
</template>
