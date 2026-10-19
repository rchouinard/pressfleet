<script setup lang="ts">
// "Jobs - PressFleet"; pages set only their own title.
useHead({ titleTemplate: title => (title && title !== 'PressFleet' ? `${title} - PressFleet` : 'PressFleet') })

// The reauthentication dialog the API client opens for sensitive actions
// (composables/useReauthentication.ts); one for the whole app.
const reauthentication = useReauthentication()
const { reauthenticationOptions, reauthenticate } = useSession()
</script>

<template>
  <UApp :toaster="{ position: 'bottom-right', expand: true }">
    <!-- Announces the new page title to screen readers after each navigation. -->
    <NuxtRouteAnnouncer />
    <NuxtLayout>
      <NuxtPage />
    </NuxtLayout>
    <AuthReauthenticateDialog
      :open="!!reauthentication.pending.value"
      :load-options="reauthenticationOptions"
      :submit="reauthenticate"
      @finish="reauthentication.finish"
    />
  </UApp>
</template>
