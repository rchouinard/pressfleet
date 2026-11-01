<script setup lang="ts">
// "Push to production", in a modal: the last safeguard before an operator
// overwrites a production site. It lists what the chosen scope does to
// production (utils/staging-sync.ts: only the warnings that apply), shows
// when the data that would be lost starts, and asks for the production site's
// slug before a push with the database or all files can start. The caller does the request in
// `submit`. A 422 on `/database` is the core guard (the server's message,
// with a link to production's core policy); other 422s show next to their
// fields and anything else in a ProblemAlert. Props only, so it mounts under
// Vitest.
import { computed, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import type { Site } from '../../utils/site'
import {
  CORE_GUARD_FIELD, copyFilesOptions, corePolicyHref, emptyPushScope, pushConfirmed,
  pushWarnings, slugRequired, validateScope, type SyncScope,
} from '../../utils/staging-sync'

const props = defineProps<{
  /** The staging site being pushed. */
  staging: Pick<Site, 'name' | 'php_version' | 'wp_core_version' | 'staging'>
  /** The production site it is pushed to, as just read. */
  production: Pick<Site, 'id' | 'name' | 'slug' | 'php_version' | 'wp_core_version' | 'maintenance'>
  submit: (scope: SyncScope) => Promise<void>
}>()
const open = defineModel<boolean>('open', { required: true })

const scope = ref<SyncScope>(emptyPushScope())
const typed = ref('')
const failure = ref<ApiError>()
const busy = ref(false)

watch(open, (isOpen) => {
  if (!isOpen) return
  scope.value = emptyPushScope()
  typed.value = ''
  failure.value = undefined
}, { immediate: true })

const filesItems = copyFilesOptions.map(o => ({ label: o.label, value: o.value, description: o.description }))

const scopeError = computed(() => validateScope(scope.value))
const warnings = computed(() => (scopeError.value
  ? []
  : pushWarnings({ scope: scope.value, staging: props.staging, production: props.production })))
const dangers = computed(() => warnings.value.filter(w => w.level === 'danger'))
const infos = computed(() => warnings.value.filter(w => w.level === 'info'))

const needsSlug = computed(() => slugRequired(scope.value))
const ready = computed(() => pushConfirmed(scope.value, typed.value, props.production.slug))

// The core guard's refusal, in the server's words.
const coreRefusal = computed(() => failure.value?.fieldError(CORE_GUARD_FIELD))
const filesError = computed(() => failure.value?.fieldError('/files'))
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

async function onSubmit() {
  failure.value = undefined
  if (!ready.value) return
  busy.value = true
  try {
    await props.submit({ ...scope.value })
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
    :title="`Push ${staging.name} to production`"
    :description="`Copies this staging site into ${production.name}, replacing what you choose on the live site.`"
    :dismissible="!busy"
  >
    <template #body>
      <form id="push-form" class="space-y-4" novalidate @submit.prevent="onSubmit">
        <ProblemAlert v-if="alertError" :error="alertError" />
        <div
          v-if="coreRefusal"
          role="alert"
          class="rounded-md border border-red-300 bg-red-50 p-4 text-sm text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
          data-testid="core-guard"
        >
          <p class="font-medium">The database can't be pushed: production's core policy doesn't allow it</p>
          <p class="mt-1 break-words" data-testid="core-guard-message">{{ coreRefusal }}</p>
          <p class="mt-2">
            <NuxtLink :to="corePolicyHref(production.id)" class="underline" data-testid="core-policy-link">Open {{ production.name }}'s WordPress core policy</NuxtLink>
            to approve the branch, then try again. Pushing without the database doesn't need it.
          </p>
          <p v-if="failure?.requestId" class="mt-2 text-xs opacity-75">Request ID: {{ failure.requestId }}</p>
        </div>

        <UFormField label="Files" name="files" :error="filesError">
          <URadioGroup v-model="scope.files" :items="filesItems" data-testid="push-files" />
        </UFormField>
        <UCheckbox v-model="scope.database" label="Replace production's database with the staging site's" data-testid="push-database" />
        <p v-if="scopeError" class="text-sm text-red-700 dark:text-red-300" role="alert" data-testid="scope-error">{{ scopeError }}</p>

        <template v-else>
          <section
            v-if="dangers.length > 0"
            class="space-y-2 rounded-md border border-red-300 bg-red-50 p-3 text-sm text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
            aria-label="Data that will be lost"
            data-testid="push-dangers"
          >
            <p class="flex items-center gap-2 font-medium">
              <UIcon name="i-lucide-triangle-alert" class="size-4 shrink-0" aria-hidden="true" />
              Production's own changes will be lost
            </p>
            <ul class="list-disc space-y-1 pl-5">
              <li v-for="w in dangers" :key="w.id" :data-testid="`warn-${w.id}`">{{ w.text }}</li>
            </ul>
          </section>

          <section class="space-y-2 rounded-md border border-default bg-elevated p-3 text-sm" aria-label="What a push does" data-testid="push-infos">
            <p class="font-medium">What this does</p>
            <ul class="list-disc space-y-1 pl-5">
              <li v-for="w in infos" :key="w.id" :data-testid="`warn-${w.id}`">{{ w.text }}</li>
            </ul>
          </section>
        </template>

        <UFormField v-if="needsSlug" :label="`Type ${production.slug} to confirm`" name="confirm" hint="This push overwrites production's data, so it asks for the production site's slug">
          <UInput v-model="typed" autocomplete="off" :disabled="busy" class="w-full" data-testid="slug-input" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="push-form" color="error" :loading="busy" :disabled="!ready" data-testid="push-submit">
          {{ needsSlug ? 'Overwrite production and push' : 'Push to production' }}
        </UButton>
      </div>
    </template>
  </UModal>
</template>
