<script setup lang="ts">
// User management (design doc §4.6): list, create, change role, disable or
// re-enable, delete. Admin only: the route is gated on `user.view`, and the server
// answers 403 to anyone else regardless.
import { toApiError, type ApiError } from '../utils/api-error'
import type { User, UserCreate, UserUpdate } from '../utils/users'

definePageMeta({ requires: 'user.view' })
useHead({ title: 'Users' })
useBreadcrumbs([{ label: 'Users' }])

const api = useApi()
const notify = useNotify()
const { user: me } = useSession()
const { can } = useCan()

const items = ref<User[]>([])
const cursor = ref<string | null>(null)
const { data: loaded, status, refresh } = useAsyncData('users-list', async () => {
  try {
    const { data } = await api.GET('/api/v1/users', { params: { query: { limit: 50 } } })
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
    const { data } = await api.GET('/api/v1/users', { params: { query: { limit: 50, cursor: cursor.value } } })
    items.value.push(...(data?.items ?? []))
    cursor.value = data?.next_cursor ?? null
  } catch (e) {
    notify.error(e)
  } finally {
    loadingMore.value = false
  }
}

// One dialog for both create and edit.
const createOpen = ref(false)
const editing = ref<User>()
const editOpen = computed({
  get: () => editing.value !== undefined,
  set: (open: boolean) => {
    if (!open) closeEdit()
  },
})
const submitting = ref(false)
const formError = ref<ApiError>()

function openCreate() {
  formError.value = undefined
  createOpen.value = true
}
function openEdit(user: User) {
  formError.value = undefined
  editing.value = user
}
function closeEdit() {
  editing.value = undefined
  formError.value = undefined
}

async function create(body: UserCreate) {
  submitting.value = true
  formError.value = undefined
  try {
    const { data } = await api.POST('/api/v1/users', { body })
    createOpen.value = false
    notify.success('User created', data?.email)
    await refresh()
  } catch (e) {
    // Field errors show in the form; everything else (409 duplicate email, 403) too.
    formError.value = toApiError(e)
  } finally {
    submitting.value = false
  }
}

async function update(body: UserUpdate) {
  const target = editing.value
  if (!target) return
  submitting.value = true
  formError.value = undefined
  try {
    const { data } = await api.PATCH('/api/v1/users/{user_id}', { params: { path: { user_id: target.id } }, body })
    closeEdit()
    notify.success('User updated', data?.email)
    await refresh()
  } catch (e) {
    formError.value = toApiError(e)
  } finally {
    submitting.value = false
  }
}

// Delete, after a confirmation. The server refuses to delete the last Admin
// (409); the dialog shows its reason.
const deleting = ref<User>()
const deleteOpen = computed({
  get: () => deleting.value !== undefined,
  set: (open: boolean) => {
    if (!open && !deleteBusy.value) {
      deleting.value = undefined
      deleteError.value = undefined
    }
  },
})
const deleteBusy = ref(false)
const deleteError = ref<ApiError>()

function openDelete(user: User) {
  deleteError.value = undefined
  deleting.value = user
}

async function remove() {
  const target = deleting.value
  if (!target) return
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    await api.DELETE('/api/v1/users/{user_id}', { params: { path: { user_id: target.id } } })
    deleting.value = undefined
    notify.success('User deleted', target.email)
    await refresh()
  } catch (e) {
    deleteError.value = toApiError(e)
  } finally {
    deleteBusy.value = false
  }
}

const generalFormError = computed(() => (formError.value && formError.value.errors.length === 0 ? formError.value : undefined))
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">Users</h1>
      <div class="flex items-center gap-2">
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="pending" @click="refresh()">Refresh</UButton>
        <IfCan action="user.create">
          <UButton icon="i-lucide-user-plus" @click="openCreate">Add user</UButton>
        </IfCan>
      </div>
    </div>
    <p class="text-sm text-muted">
      Roles apply to a user's sessions and API tokens at once. Disabling a user signs them out and revokes their tokens.
    </p>

    <ProblemAlert v-if="loadError" :error="loadError" />
    <template v-else>
      <p v-if="pending && items.length === 0" class="text-sm text-muted" role="status">Loading users...</p>
      <UserTable v-else :users="items" caption="Users" :current-user-id="me?.id" :can-delete="can('user.delete')" @edit="openEdit" @delete="openDelete" />
      <div v-if="cursor" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="loadingMore" @click="loadMore()">Load more</UButton>
      </div>
    </template>

    <UModal v-model:open="createOpen" title="Add user" description="Create an account and choose what it may do.">
      <template #body>
        <ProblemAlert v-if="generalFormError" :error="generalFormError" class="mb-4" />
        <UserForm :submitting="submitting" :error="formError" @create="create" @cancel="createOpen = false" />
      </template>
    </UModal>

    <UModal v-model:open="editOpen" title="Edit user" description="Change the role or disable the account.">
      <template #body>
        <ProblemAlert v-if="generalFormError" :error="generalFormError" class="mb-4" />
        <UserForm
          v-if="editing"
          :key="editing.id"
          :user="editing"
          :self="editing.id === me?.id"
          :submitting="submitting"
          :error="formError"
          @update="update"
          @cancel="closeEdit"
        />
      </template>
    </UModal>

    <ConfirmDialog
      v-model:open="deleteOpen"
      title="Delete this user?"
      :description="deleting ? `${deleting.email} is signed out and loses their API tokens and SSH keys. This can't be undone.` : undefined"
      confirm-label="Delete user"
      :busy="deleteBusy"
      :error="deleteError"
      @confirm="remove"
    />
  </div>
</template>
