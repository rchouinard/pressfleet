<script setup lang="ts">
// "Log in to WordPress": one click to wp-admin as the site's default
// administrator, and "Log in as..." to pick another one (design doc §5.9).
// Props and callbacks only, so it is testable without the Nuxt runtime; the
// page supplies the two API calls and gates it with `site.admin_login`.
//
// The click handler opens the tab synchronously (popup blockers refuse
// `window.open` after an await), then asks for the token and submits it to
// the tab in a hidden POST form. The token lives in one local variable for
// that moment: not in state, a URL, or a log.
import { computed, ref } from 'vue'
import { ApiError, toApiError } from '../../utils/api-error'
import {
  adminLoginBlockedReason, administratorLabel, closeLoginTab, isChooseUserError, openLoginTab,
  previewCredentialsNote, submitLoginForm, type AdminLogin, type SiteAdministrator,
} from '../../utils/admin-login'
import type { Site } from '../../utils/site'

const props = defineProps<{
  site: Pick<Site, 'status' | 'maintenance' | 'primary_domain' | 'preview_protection'>
  /** Creates a login token, for `user` or the site's default administrator. */
  createLogin: (user?: string) => Promise<AdminLogin>
  listAdministrators: () => Promise<SiteAdministrator[]>
}>()

const busy = ref(false)
const error = ref<ApiError>()

const blocked = computed(() => adminLoginBlockedReason(props.site))
const note = computed(() => previewCredentialsNote(props.site))

const popupBlocked = new ApiError({
  type: 'urn:pressfleet:ui:popup-blocked',
  title: 'The new tab was blocked',
  status: 0,
  detail: 'Allow pop-ups for this dashboard and try again.',
})

/** Runs synchronously from a click, so the tab it opens is allowed. */
function start(user?: string) {
  if (busy.value || blocked.value) return
  error.value = undefined
  const tab = openLoginTab()
  if (!tab) {
    error.value = popupBlocked
    return
  }
  busy.value = true
  void finish(tab, user)
}

async function finish(tab: NonNullable<ReturnType<typeof openLoginTab>>, user?: string) {
  try {
    submitLoginForm(document, tab, await props.createLogin(user))
    pickerOpen.value = false
  } catch (e) {
    closeLoginTab(tab)
    const err = toApiError(e)
    if (user === undefined && isChooseUserError(err)) {
      openPicker()
    } else {
      error.value = err
    }
  } finally {
    busy.value = false
  }
}

// The picker of administrators.
const pickerOpen = ref(false)
const administrators = ref<SiteAdministrator[]>([])
const loadingAdmins = ref(false)

async function openPicker() {
  if (blocked.value) return
  error.value = undefined
  administrators.value = []
  pickerOpen.value = true
  loadingAdmins.value = true
  try {
    administrators.value = await props.listAdministrators()
  } catch (e) {
    error.value = toApiError(e)
  } finally {
    loadingAdmins.value = false
  }
}

const menu = [[{ label: 'Log in as...', icon: 'i-lucide-user-round-cog', onSelect: () => void openPicker() }]]
</script>

<template>
  <div class="flex flex-col items-start gap-1 sm:items-end">
    <div class="flex">
      <UButton
        icon="i-lucide-log-in"
        data-testid="admin-login"
        :loading="busy"
        :disabled="blocked !== undefined || busy"
        :aria-describedby="blocked || note ? 'admin-login-hint' : undefined"
        @click="start()"
      >
        Log in to WordPress
      </UButton>
      <UDropdownMenu :items="menu" :disabled="blocked !== undefined || busy">
        <UButton
          icon="i-lucide-chevron-down"
          color="neutral"
          variant="outline"
          aria-label="More login options"
          data-testid="admin-login-more"
          :disabled="blocked !== undefined || busy"
        />
      </UDropdownMenu>
    </div>
    <p v-if="blocked || note" id="admin-login-hint" class="max-w-xs text-xs text-muted sm:text-right">{{ blocked ?? note }}</p>
    <ProblemAlert v-if="error && !pickerOpen" :error="error" class="max-w-md" />

    <UModal v-model:open="pickerOpen" title="Log in as..." description="Choose the WordPress administrator to log in as.">
      <template #body>
        <p v-if="loadingAdmins" class="text-sm text-muted" role="status">Loading administrators...</p>
        <ProblemAlert v-if="error" :error="error" class="mb-3" />
        <p v-if="!loadingAdmins && administrators.length === 0 && !error" class="text-sm text-muted">This site has no administrators.</p>
        <ul v-if="administrators.length > 0" class="space-y-2">
          <li v-for="a in administrators" :key="a.login">
            <UButton
              block
              color="neutral"
              variant="outline"
              class="justify-between"
              data-testid="administrator"
              :disabled="busy"
              @click="start(a.login)"
            >
              {{ administratorLabel(a) }}
              <UBadge v-if="a.default" color="neutral" variant="subtle">Default</UBadge>
            </UButton>
          </li>
        </ul>
        <p v-if="note" class="mt-3 text-xs text-muted">{{ note }}</p>
      </template>
    </UModal>
  </div>
</template>
