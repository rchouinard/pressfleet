<script setup lang="ts">
// The signed-in user's account: who they are, their API tokens (create,
// revoke), and sign out. A new token's secret is held in `secret` only while
// the dialog is open; closing the dialog drops it. It is never stored, put in
// a URL or logged.
import { toApiError, type ApiError } from '../utils/api-error'
import { roleColors } from '../utils/users'
import { formatTimestamp, type ApiToken, type ApiTokenCreate } from '../utils/tokens'

useHead({ title: 'Account & tokens' })
useBreadcrumbs([{ label: 'Account & tokens' }])

const api = useApi()
const notify = useNotify()
const { user, logout } = useSession()

const items = ref<ApiToken[]>([])
const cursor = ref<string | null>(null)
const { data: loaded, status, refresh } = useAsyncData('own-tokens', async () => {
  try {
    const { data } = await api.GET('/api/v1/auth/tokens', { params: { query: { limit: 50 } } })
    items.value = data?.items ?? []
    cursor.value = data?.next_cursor ?? null
    return { error: null as ApiError | null }
  } catch (e) {
    items.value = []
    cursor.value = null
    return { error: toApiError(e) }
  }
})
const loadError = computed(() => loaded.value?.error ?? undefined)
const pending = computed(() => status.value === 'pending')

const loadingMore = ref(false)
async function loadMore() {
  if (!cursor.value || loadingMore.value) return
  loadingMore.value = true
  try {
    const { data } = await api.GET('/api/v1/auth/tokens', { params: { query: { limit: 50, cursor: cursor.value } } })
    items.value.push(...(data?.items ?? []))
    cursor.value = data?.next_cursor ?? null
  } catch (e) {
    notify.error(e)
  } finally {
    loadingMore.value = false
  }
}

// Create
const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError>()
const generalCreateError = computed(() => (createError.value && createError.value.errors.length === 0 ? createError.value : undefined))
const secret = ref<{ name: string, token: string }>()
const secretOpen = computed({
  get: () => secret.value !== undefined,
  set: (open: boolean) => {
    if (!open) secret.value = undefined
  },
})

function openCreate() {
  createError.value = undefined
  createOpen.value = true
}

async function create(body: ApiTokenCreate) {
  creating.value = true
  createError.value = undefined
  try {
    const { data } = await api.POST('/api/v1/auth/tokens', { body })
    createOpen.value = false
    if (data) secret.value = { name: data.name, token: data.token }
    await refresh()
  } catch (e) {
    createError.value = toApiError(e)
  } finally {
    creating.value = false
  }
}

// Revoke, after a confirmation.
const revoking = ref<ApiToken>()
const revokeOpen = computed({
  get: () => revoking.value !== undefined,
  set: (open: boolean) => {
    if (!open && !revokeBusy.value) revoking.value = undefined
  },
})
const revokeBusy = ref(false)

async function revoke() {
  const target = revoking.value
  if (!target) return
  revokeBusy.value = true
  try {
    await api.DELETE('/api/v1/auth/tokens/{token_id}', { params: { path: { token_id: target.id } } })
    notify.success('Token revoked', `${target.name} no longer works.`)
    revoking.value = undefined
    await refresh()
  } catch (e) {
    const error = toApiError(e)
    if (error.status === 404) {
      // Already gone (revoked elsewhere): the list is stale, not the action wrong.
      revoking.value = undefined
      await refresh()
    }
    notify.error(error)
  } finally {
    revokeBusy.value = false
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
  <div class="space-y-8">
    <h1 class="text-2xl font-semibold tracking-tight">Account &amp; tokens</h1>

    <section aria-labelledby="profile-heading" class="space-y-3">
      <h2 id="profile-heading" class="font-medium">Your account</h2>
      <dl v-if="user" class="grid gap-x-6 gap-y-3 rounded-md border border-default p-4 text-sm sm:grid-cols-2">
        <div>
          <dt class="text-muted">Email</dt>
          <dd class="font-medium [overflow-wrap:anywhere]">{{ user.email }}</dd>
        </div>
        <div>
          <dt class="text-muted">Role</dt>
          <dd><UBadge :color="roleColors[user.role]" variant="subtle" size="sm">{{ user.role }}</UBadge></dd>
        </div>
        <div>
          <dt class="text-muted">Last sign-in</dt>
          <dd>{{ formatTimestamp(user.last_login_at, 'Never') }}</dd>
        </div>
        <div>
          <dt class="text-muted">Signed in with</dt>
          <dd>{{ user.auth_method === 'session' ? 'Dashboard session' : 'API token' }}</dd>
        </div>
      </dl>
      <UButton color="neutral" variant="outline" icon="i-lucide-log-out" :loading="signingOut" @click="signOut">Sign out</UButton>
    </section>

    <section aria-labelledby="tokens-heading" class="space-y-3">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tokens-heading" class="font-medium">API tokens</h2>
        <div class="flex items-center gap-2">
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-refresh-cw" aria-label="Refresh tokens" :loading="pending" @click="refresh()" />
          <IfCan action="self.tokens.manage">
            <UButton icon="i-lucide-plus" @click="openCreate">Create token</UButton>
          </IfCan>
        </div>
      </div>
      <p class="text-sm text-muted">
        Tokens let scripts and CI call the API as you (<code>Authorization: Bearer ...</code>). A read-only token can never change anything; a full token can do whatever your role allows. Tokens stop working if your role is lowered or your account is disabled.
      </p>

      <ProblemAlert v-if="loadError" :error="loadError" />
      <template v-else>
        <p v-if="pending && items.length === 0" class="text-sm text-muted" role="status">Loading tokens...</p>
        <p v-else-if="items.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
          You have no API tokens.
        </p>
        <TokenTable v-else :tokens="items" caption="Your API tokens" @revoke="t => (revoking = t)" />
        <div v-if="cursor" class="flex justify-center">
          <UButton color="neutral" variant="outline" :loading="loadingMore" @click="loadMore()">Load more</UButton>
        </div>
      </template>
    </section>

    <UModal v-model:open="createOpen" title="Create API token" description="Choose a name, what the token may do, and when it expires.">
      <template #body>
        <ProblemAlert v-if="generalCreateError" :error="generalCreateError" class="mb-4" />
        <TokenForm :submitting="creating" :error="createError" @submit="create" @cancel="createOpen = false" />
      </template>
    </UModal>

    <!-- Not dismissible by clicking outside: closing is how the secret is lost. -->
    <UModal v-model:open="secretOpen" :dismissible="false" :close="false" title="Your new API token" :description="secret ? `Token ${secret.name} was created.` : undefined">
      <template #body>
        <TokenSecret v-if="secret" :token="secret.token" />
      </template>
      <template #footer>
        <UButton @click="secretOpen = false">Done, I've copied it</UButton>
      </template>
    </UModal>

    <UModal v-model:open="revokeOpen" title="Revoke this token?" :description="revoking ? `Anything using ${revoking.name} (${revoking.prefix}...) stops working immediately. This can't be undone.` : undefined">
      <template #footer>
        <UButton color="neutral" variant="outline" :disabled="revokeBusy" @click="revoking = undefined">Cancel</UButton>
        <UButton color="error" :loading="revokeBusy" @click="revoke">Revoke token</UButton>
      </template>
    </UModal>
  </div>
</template>
