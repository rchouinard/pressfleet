<script setup lang="ts">
// Forced enrollment (design doc §6.2): when the two-factor policy covers the
// user's role and they have no second factor, their session may only view
// itself, sign out and enroll. The route middleware sends them here from
// every other page (see enrollmentRedirect in utils/two-factor.ts) with the
// page they wanted as `redirect`. They set up a factor with the account
// page's own TwoFactorSettings, see their recovery codes, and are sent on.
// The auth layout keeps the app shell, whose requests would all be refused,
// off this page.
import { safeRedirect } from '../utils/redirect'
import { twoFactorClient } from '../utils/two-factor-settings'

definePageMeta({ layout: 'auth' })
useHead({ title: 'Set up two-factor authentication' })

const route = useRoute()
const notify = useNotify()
const { user, logout, refresh } = useSession()
const twoFactor = twoFactorClient(useApi())
const rpId = useRequestURL().hostname

// The session is restricted until the server sees a factor; reload it and
// go on once it isn't. TwoFactorSettings only says so after the recovery
// codes are closed.
async function changed() {
  try {
    const current = await refresh()
    if (current && !current.enrollment_required) await navigateTo(safeRedirect(route.query.redirect), { replace: true })
  } catch (e) {
    notify.error(e)
  }
}

const signingOut = ref(false)
async function signOut() {
  signingOut.value = true
  try {
    await logout()
  } catch (e) {
    notify.error(e)
  } finally {
    signingOut.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <div class="space-y-2">
      <h1 class="text-2xl font-semibold tracking-tight">Set up two-factor authentication</h1>
      <p class="text-sm text-muted">
        Your role requires a second factor when you sign in. Add an authenticator app or a security key or passkey to continue to the dashboard.
      </p>
      <p v-if="user" class="text-sm text-muted">Signed in as <span class="font-medium text-default [overflow-wrap:anywhere]">{{ user.email }}</span>.</p>
    </div>

    <TwoFactorSettings v-if="user" :client="twoFactor" :user="user" :rp-id="rpId" :level="2" @changed="changed" />

    <UButton color="neutral" variant="outline" icon="i-lucide-log-out" :loading="signingOut" @click="signOut">Sign out</UButton>
  </div>
</template>
