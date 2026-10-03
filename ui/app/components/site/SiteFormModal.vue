<script setup lang="ts">
// The provisioning form, in a modal. Client-side checks run first; the
// server's 422 field errors then show next to their fields and anything else
// (409, 403, network) in a ProblemAlert. The caller does the request in
// `submit` and the modal stays open until it resolves; a throw is shown here.
// PHP versions come from the catalog the caller loads (`usePhpCatalog`);
// "Platform default" is preselected and sends no version, as a blank field did.
import { computed, reactive, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import { PHP_DEFAULT_CHOICE, phpCreateOptions, phpStatus, type PhpVersionInfo } from '../../utils/php'
import {
  ADMIN_USERNAME_MAX, SITE_NAME_MAX, validateSite,
  type SiteFormValues,
} from '../../utils/site'

const props = defineProps<{
  groups: { label: string, value: string }[]
  /** Preselects a client group (the list's filter). */
  defaultGroupId?: string
  submit: (values: SiteFormValues) => Promise<void>
  /** The PHP versions the API lists; empty while loading or after a failure. */
  phpVersions?: readonly PhpVersionInfo[]
  /** The version new sites get when none is chosen (the catalog's `default`). */
  phpDefault?: string
  /** Why the catalog couldn't be loaded; only the platform default is offered then. */
  phpError?: ApiError
}>()
const open = defineModel<boolean>('open', { required: true })

const values = reactive<SiteFormValues>({ name: '', client_group_id: '', admin_email: '', admin_username: '', php_version: '' })
const local = ref<Partial<Record<keyof SiteFormValues, string>>>({})
const failure = ref<ApiError>()
const busy = ref(false)

watch(open, (isOpen) => {
  if (!isOpen) return
  values.name = ''
  values.client_group_id = props.defaultGroupId ?? ''
  values.admin_email = ''
  values.admin_username = ''
  values.php_version = ''
  local.value = {}
  failure.value = undefined
}, { immediate: true })

const phpChoices = computed(() => phpCreateOptions(props.phpVersions, props.phpDefault))
const phpChoice = computed({
  get: () => values.php_version === '' ? PHP_DEFAULT_CHOICE : values.php_version,
  set: (v: string) => { values.php_version = v === PHP_DEFAULT_CHOICE ? '' : v },
})
// A chosen version that is no longer offered (the catalog reloaded) falls back to the default.
watch(phpChoices, (items) => {
  if (!items.some(i => i.value === phpChoice.value)) values.php_version = ''
})
const phpEffective = computed(() => values.php_version || props.phpDefault)
const phpChosen = computed(() => (phpEffective.value ? phpStatus(phpEffective.value, props.phpVersions) : undefined))

function err(field: keyof SiteFormValues) {
  return local.value[field] ?? failure.value?.fieldError(`/${field}`)
}
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

async function onSubmit() {
  local.value = validateSite(values)
  failure.value = undefined
  if (Object.keys(local.value).length > 0) return
  busy.value = true
  try {
    await props.submit({ ...values })
    open.value = false
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="New site"
    description="Creates a WordPress site on a preview domain. Provisioning runs in the background."
    :dismissible="!busy"
  >
    <template #body>
      <form id="site-form" class="space-y-4" novalidate @submit.prevent="onSubmit">
        <ProblemAlert v-if="alertError" :error="alertError" />
        <UFormField label="Name" name="name" required :error="err('name')">
          <UInput v-model="values.name" :maxlength="SITE_NAME_MAX" autocomplete="off" class="w-full" />
        </UFormField>
        <UFormField label="Client group" name="client_group_id" required :error="err('client_group_id')">
          <USelect v-model="values.client_group_id" :items="groups" placeholder="Choose a client group" class="w-full" />
        </UFormField>
        <UFormField label="Admin email" name="admin_email" required :error="err('admin_email')">
          <UInput v-model="values.admin_email" type="email" autocomplete="off" class="w-full" />
        </UFormField>
        <UFormField label="Admin username" name="admin_username" hint="Optional" :error="err('admin_username')" description="A name is generated if you leave this blank.">
          <UInput v-model="values.admin_username" :maxlength="ADMIN_USERNAME_MAX" autocomplete="off" class="w-full" />
        </UFormField>
        <UFormField label="PHP version" name="php_version" :error="err('php_version')" description="End-of-life versions aren't offered. You can change the version later in the site's settings.">
          <USelect v-model="phpChoice" :items="phpChoices" class="w-full" data-testid="php-select" />
        </UFormField>
        <p v-if="phpChosen?.warning" class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" data-testid="php-chosen-warning">
          <UIcon :name="phpChosen.icon" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />{{ phpChosen.warning }}
        </p>
        <div v-if="phpError" class="space-y-2" data-testid="php-catalog-error">
          <p class="text-sm">The PHP versions couldn't be loaded, so only the platform default is offered.</p>
          <ProblemAlert :error="phpError" />
        </div>
        <p class="text-sm text-muted">
          The WordPress admin password is generated for you. Once the site is running, an operator can reveal it, once, on the site's page.
        </p>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="site-form" :loading="busy">Create site</UButton>
      </div>
    </template>
  </UModal>
</template>
