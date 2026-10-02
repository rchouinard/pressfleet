<script setup lang="ts">
// The "SSH keys" section of the account page (design doc §5.8): the user's
// registered public keys for the SFTP gateway and database tunnels, with add
// and remove. Every role may register keys; only Operators and Admins may
// connect with them, which is said here because a Viewer's key would
// otherwise look broken.
import { toApiError, type ApiError } from '../../utils/api-error'
import { createPagedList } from '../../utils/paged-list'
import { sshKeyPayload, validateSshKey, SSH_KEY_NAME_MAX, type SshKey, type SshKeyFormValues } from '../../utils/ssh-key'

const api = useApi()
const notify = useNotify()
const { role } = useSession()
const mayConnect = computed(() => role.value === 'operator' || role.value === 'admin')

const list = createPagedList<SshKey>(async (cursor) => {
  const { data } = await api.GET('/api/v1/users/me/ssh-keys', { params: { query: { limit: 50, cursor } } })
  return data
})
useAsyncData('own-ssh-keys', async () => {
  await list.load()
  return true
})

async function loadMore() {
  try {
    await list.loadMore()
  } catch (e) {
    notify.error(e)
  }
}

// Add.
const addOpen = ref(false)
const values = reactive<SshKeyFormValues>({ name: '', public_key: '' })
const local = ref<Partial<Record<keyof SshKeyFormValues, string>>>({})
const failure = ref<ApiError>()
const adding = ref(false)
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))
function err(field: keyof SshKeyFormValues) {
  return local.value[field] ?? failure.value?.fieldError(`/${field}`)
}

function openAdd() {
  values.name = ''
  values.public_key = ''
  local.value = {}
  failure.value = undefined
  addOpen.value = true
}

async function add() {
  local.value = validateSshKey(values)
  failure.value = undefined
  if (Object.keys(local.value).length > 0) return
  adding.value = true
  try {
    const { data } = await api.POST('/api/v1/users/me/ssh-keys', { body: sshKeyPayload(values) })
    addOpen.value = false
    notify.success('SSH key added', data?.name)
    await list.load()
  } catch (e) {
    // 409: the key is already registered (to anyone); 422: not a usable key.
    failure.value = toApiError(e)
  } finally {
    adding.value = false
  }
}

// Remove, after a confirmation.
const removing = ref<SshKey>()
const removeOpen = computed({
  get: () => removing.value !== undefined,
  set: (open: boolean) => {
    if (!open && !removeBusy.value) removing.value = undefined
  },
})
const removeBusy = ref(false)
const removeError = ref<ApiError>()
function askRemove(key: SshKey) {
  removeError.value = undefined
  removing.value = key
}
async function remove() {
  const target = removing.value
  if (!target) return
  removeBusy.value = true
  removeError.value = undefined
  try {
    await api.DELETE('/api/v1/users/me/ssh-keys/{ssh_key_id}', { params: { path: { ssh_key_id: target.id } } })
    notify.success('SSH key removed', `${target.name} can no longer connect.`)
    removing.value = undefined
    await list.load()
  } catch (e) {
    const error = toApiError(e)
    if (error.status === 404) {
      removing.value = undefined
      await list.load()
    } else {
      removeError.value = error
    }
  } finally {
    removeBusy.value = false
  }
}
</script>

<template>
  <section aria-labelledby="ssh-keys-heading" class="space-y-3">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h2 id="ssh-keys-heading" class="font-medium">SSH keys</h2>
      <div class="flex items-center gap-2">
        <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-refresh-cw" aria-label="Refresh SSH keys" :loading="list.loading.value" @click="list.load()" />
        <IfCan action="self.ssh_keys.manage">
          <UButton icon="i-lucide-plus" @click="openAdd">Add SSH key</UButton>
        </IfCan>
      </div>
    </div>
    <p class="text-sm text-muted">
      Public keys for the SFTP gateway and database tunnels: there are no passwords. Removing a key refuses new connections at once and ends open ones within seconds.
      <template v-if="!mayConnect">Your role can register keys but can't connect with them: only Operators and Admins can.</template>
    </p>

    <ProblemAlert v-if="list.error.value" :error="list.error.value" />
    <template v-else>
      <p v-if="list.loading.value && list.items.value.length === 0" class="text-sm text-muted" role="status">Loading SSH keys...</p>
      <p v-else-if="list.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        You have no SSH keys.
      </p>
      <SshKeyTable v-else :keys="list.items.value" caption="Your SSH keys" @remove="askRemove" />
      <div v-if="list.cursor.value" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="list.loadingMore.value" @click="loadMore()">Load more</UButton>
      </div>
    </template>

    <UModal v-model:open="addOpen" title="Add SSH key" description="Paste a public key, as in your .pub file. A key can belong to one user only." :dismissible="!adding">
      <template #body>
        <form id="ssh-key-form" class="space-y-4" novalidate @submit.prevent="add">
          <ProblemAlert v-if="alertError" :error="alertError" />
          <UFormField label="Public key" name="public_key" required :error="err('public_key')" description="Starts with ssh-ed25519, ecdsa-sha2-..., sk-... or ssh-rsa (2048 bits or more).">
            <UTextarea v-model="values.public_key" :rows="4" autocomplete="off" spellcheck="false" placeholder="ssh-ed25519 AAAA... you@laptop" class="w-full font-mono" :disabled="adding" />
          </UFormField>
          <UFormField label="Name" name="name" hint="Optional" :error="err('name')" description="Tells keys apart. Defaults to the key's comment.">
            <UInput v-model="values.name" :maxlength="SSH_KEY_NAME_MAX" autocomplete="off" class="w-full" :disabled="adding" />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex w-full flex-wrap justify-end gap-2">
          <UButton color="neutral" variant="outline" :disabled="adding" @click="addOpen = false">Cancel</UButton>
          <UButton type="submit" form="ssh-key-form" :loading="adding">Add key</UButton>
        </div>
      </template>
    </UModal>

    <ConfirmDialog
      v-model:open="removeOpen"
      :title="`Remove ${removing?.name ?? 'this key'}?`"
      confirm-label="Remove key"
      :busy="removeBusy"
      :error="removeError"
      @confirm="remove()"
    >
      <p class="text-sm">Connections using this key are refused at once and open sessions end within seconds. You can add the key again later.</p>
    </ConfirmDialog>
  </section>
</template>
