<script setup lang="ts">
// A site's settings: PHP version (with end-of-life warnings), resource limits,
// maintenance mode, cache purge and preview password protection. Plain props
// and callbacks (no Nuxt runtime) so it is testable; SiteSettings.vue wires
// them to the API. Each callback does its request and resolves when the job is
// queued; a throw is shown in that section. Controls need `canUpdate` (and the
// operator-level maintenance and purge flags), and explain themselves instead
// when the site's state would make the API answer 409.
import { computed, reactive, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import { phpOptions, phpStatus } from '../../utils/php'
import {
  phpPayload, previewProtectionPayload, resourceFormValues, resourcesPayload, settingsBlockedReason,
  validateResources, type ResourceFormErrors, type SiteUpdate,
} from '../../utils/site-settings'
import type { Site } from '../../utils/site'

const props = defineProps<{
  site: Pick<Site, 'status' | 'php_version' | 'resources' | 'maintenance' | 'preview_protection' | 'preview_domain'>
  /** `site.update` for the signed-in role. */
  canUpdate: boolean
  /** `site.maintenance.set`. */
  canMaintain: boolean
  /** `site.cache.purge`. */
  canPurge: boolean
  update: (body: SiteUpdate) => Promise<void>
  setMaintenance: (enabled: boolean) => Promise<void>
  purgeCache: () => Promise<void>
  regeneratePassword: () => Promise<void>
}>()

type Section = 'php' | 'resources' | 'maintenance' | 'cache' | 'preview'
const busy = ref<Section>()
const failures = reactive<Partial<Record<Section, ApiError>>>({})

async function run(section: Section, fn: () => Promise<void>): Promise<boolean> {
  busy.value = section
  failures[section] = undefined
  try {
    await fn()
    return true
  } catch (e) {
    failures[section] = toApiError(e)
    return false
  } finally {
    busy.value = undefined
  }
}

const blocked = computed(() => settingsBlockedReason(props.site))
const idle = computed(() => busy.value === undefined)

// PHP version.
const php = ref(props.site.php_version)
watch(() => props.site.php_version, (v) => { php.value = v })
const currentPhp = computed(() => phpStatus(props.site.php_version))
const chosenPhp = computed(() => phpStatus(php.value))
const phpChoices = computed(() => phpOptions(props.site.php_version))
const phpBody = computed(() => phpPayload(props.site.php_version, php.value))
const phpError = computed(() => failures.php?.fieldError('/php_version'))
async function savePhp() {
  const body = phpBody.value
  if (body) await run('php', () => props.update(body))
}

// Resource limits.
const res = reactive(resourceFormValues(props.site.resources))
const resLocal = ref<ResourceFormErrors>({})
watch(() => props.site.resources, (r) => { Object.assign(res, resourceFormValues(r)) }, { deep: true })
const resBody = computed(() => resourcesPayload(props.site.resources, res))
function resError(f: keyof ResourceFormErrors): string | undefined {
  return resLocal.value[f] ?? failures.resources?.fieldError(`/resources/${f}`)
}
const resAlert = computed(() => (failures.resources && failures.resources.errors.length === 0 ? failures.resources : undefined))
async function saveResources() {
  resLocal.value = validateResources(res)
  failures.resources = undefined
  if (Object.keys(resLocal.value).length > 0) return
  const body = resBody.value
  if (body) await run('resources', () => props.update(body))
}

// Maintenance.
async function toggleMaintenance() {
  await run('maintenance', () => props.setMaintenance(!props.site.maintenance))
}

// Cache purge.
const purged = ref(false)
async function purge() {
  purged.value = false
  purged.value = await run('cache', props.purgeCache)
}

// Preview protection.
const confirmRegenerate = ref(false)
const previewOn = computed(() => props.site.preview_protection.enabled)
async function toggleProtection() {
  await run('preview', () => props.update(previewProtectionPayload(!previewOn.value)))
}
async function regenerate() {
  confirmRegenerate.value = false
  await run('preview', props.regeneratePassword)
}
</script>

<template>
  <section class="space-y-6 rounded-md border border-default p-4" aria-labelledby="site-settings-heading">
    <h2 id="site-settings-heading" class="text-lg font-semibold">Settings</h2>
    <p v-if="blocked" class="text-sm text-muted" role="status" data-testid="blocked">{{ blocked }}</p>

    <!-- PHP version -->
    <div class="space-y-2" aria-labelledby="set-php-heading">
      <h3 id="set-php-heading" class="font-medium">PHP version</h3>
      <p class="flex flex-wrap items-center gap-2 text-sm">
        <span>PHP {{ site.php_version }}</span>
        <UBadge :color="currentPhp.color" variant="subtle" data-testid="php-status">
          <UIcon :name="currentPhp.icon" class="size-3.5" aria-hidden="true" />{{ currentPhp.label }}
        </UBadge>
      </p>
      <p v-if="currentPhp.warning" class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" role="alert" data-testid="php-warning">
        <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />{{ currentPhp.warning }}
      </p>
      <template v-if="canUpdate && !blocked">
        <UFormField label="Change PHP version" name="php_version" :error="phpError" description="The site's php container is recreated on the new image, which takes a few seconds.">
          <USelect v-model="php" :items="phpChoices" :disabled="!idle" class="w-full sm:w-72" />
        </UFormField>
        <p v-if="phpBody && chosenPhp.warning" class="text-sm text-muted" data-testid="php-chosen-warning">{{ chosenPhp.warning }}</p>
        <ProblemAlert v-if="failures.php && !phpError" :error="failures.php" />
        <UButton icon="i-lucide-save" :loading="busy === 'php'" :disabled="!phpBody || !idle" data-testid="save-php" @click="savePhp()">Change PHP version</UButton>
      </template>
    </div>

    <!-- Resource limits -->
    <div class="space-y-2" aria-labelledby="set-res-heading">
      <h3 id="set-res-heading" class="font-medium">Resource limits</h3>
      <p class="text-sm text-muted">For the site as a whole; the platform splits them between its containers.</p>
      <template v-if="canUpdate && !blocked">
        <form class="grid gap-3 sm:grid-cols-3" novalidate @submit.prevent="saveResources()">
          <UFormField label="CPU (cores)" name="cpu" :error="resError('cpu')">
            <UInput v-model="res.cpu" inputmode="decimal" autocomplete="off" class="w-full" />
          </UFormField>
          <UFormField label="Memory reservation (MiB)" name="memory_reservation_mb" :error="resError('memory_reservation_mb')">
            <UInput v-model="res.memory_reservation_mb" inputmode="numeric" autocomplete="off" class="w-full" />
          </UFormField>
          <UFormField label="Memory limit (MiB)" name="memory_limit_mb" :error="resError('memory_limit_mb')">
            <UInput v-model="res.memory_limit_mb" inputmode="numeric" autocomplete="off" class="w-full" />
          </UFormField>
          <div class="sm:col-span-3">
            <ProblemAlert v-if="resAlert" :error="resAlert" class="mb-2" />
            <UButton type="submit" icon="i-lucide-save" :loading="busy === 'resources'" :disabled="!resBody || !idle" data-testid="save-resources">Save limits</UButton>
          </div>
        </form>
      </template>
      <p v-else class="text-sm">{{ site.resources.cpu }} CPU, {{ site.resources.memory_reservation_mb }} MiB reserved, {{ site.resources.memory_limit_mb }} MiB limit</p>
    </div>

    <!-- Maintenance mode -->
    <div class="space-y-2" aria-labelledby="set-maint-heading">
      <h3 id="set-maint-heading" class="font-medium">Maintenance mode</h3>
      <p class="text-sm" data-testid="maintenance-state">
        {{ site.maintenance ? 'On: every domain of the site shows the maintenance page (503).' : 'Off: the site is being served.' }}
      </p>
      <ProblemAlert v-if="failures.maintenance" :error="failures.maintenance" />
      <UButton
        v-if="canMaintain && !blocked"
        icon="i-lucide-construction"
        color="neutral"
        variant="outline"
        :loading="busy === 'maintenance'"
        :disabled="!idle"
        data-testid="toggle-maintenance"
        @click="toggleMaintenance()"
      >
        {{ site.maintenance ? 'Turn maintenance mode off' : 'Turn maintenance mode on' }}
      </UButton>
    </div>

    <!-- Cache purge -->
    <div class="space-y-2" aria-labelledby="set-cache-heading">
      <h3 id="set-cache-heading" class="font-medium">Caches</h3>
      <p class="text-sm text-muted">Empties the object cache and the page cache. The site keeps serving while the next requests are rendered afresh.</p>
      <ProblemAlert v-if="failures.cache" :error="failures.cache" />
      <p v-if="purged" class="text-sm" role="status">Cache purge queued.</p>
      <UButton
        v-if="canPurge && !blocked"
        icon="i-lucide-eraser"
        color="neutral"
        variant="outline"
        :loading="busy === 'cache'"
        :disabled="!idle"
        data-testid="purge-cache"
        @click="purge()"
      >
        Purge caches
      </UButton>
    </div>

    <!-- Preview protection -->
    <div class="space-y-2" aria-labelledby="set-preview-heading">
      <h3 id="set-preview-heading" class="font-medium">Preview password protection</h3>
      <p class="text-sm" data-testid="preview-state">
        {{ previewOn ? `On: ${site.preview_domain} asks for a login (user ${site.preview_protection.username ?? 'not set yet'}).` : `Off: ${site.preview_domain} is open to anyone with the link.` }}
        Production domains are never protected.
      </p>
      <ProblemAlert v-if="failures.preview" :error="failures.preview" />
      <div v-if="canUpdate && !blocked" class="flex flex-wrap gap-2">
        <UButton
          :icon="previewOn ? 'i-lucide-lock-open' : 'i-lucide-lock'"
          color="neutral"
          variant="outline"
          :loading="busy === 'preview'"
          :disabled="!idle"
          data-testid="toggle-preview"
          @click="toggleProtection()"
        >
          {{ previewOn ? 'Turn protection off' : 'Turn protection on' }}
        </UButton>
        <UButton
          v-if="!confirmRegenerate"
          icon="i-lucide-key-round"
          color="neutral"
          variant="outline"
          :disabled="!idle"
          data-testid="regenerate-preview"
          @click="confirmRegenerate = true"
        >
          Generate a new password
        </UButton>
        <template v-else>
          <p class="basis-full text-sm" role="alert">The old password stops working once the update has finished. The new one is shown once.</p>
          <UButton icon="i-lucide-key-round" :disabled="!idle" data-testid="confirm-regenerate" @click="regenerate()">Replace the password</UButton>
          <UButton color="neutral" variant="ghost" @click="confirmRegenerate = false">Cancel</UButton>
        </template>
      </div>
    </div>
  </section>
</template>
