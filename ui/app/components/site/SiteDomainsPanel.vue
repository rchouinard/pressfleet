<script setup lang="ts">
// Go-live and alias management for one site. Plain props and callbacks (no
// Nuxt runtime) so it is testable; SiteDomains.vue wires it to the API. The
// caller does the request in `goLive` / `setAliases` and the form stays open
// until it resolves; a throw is shown here. A failed DNS check (422) lists
// each domain with what it resolves to, and offers to check again or skip it.
import { computed, ref } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  aliasesPayload, canEditAliases, canGoLive, dnsFailures, domainsBlockedReason, goLivePayload,
  isDnsCheckError, siteIsLive, MAX_ALIASES, normalizeDomain, parseAliases, validateDomains,
  type DomainFormErrors, type SiteAliases, type SiteGoLive,
} from '../../utils/domains'
import type { Site } from '../../utils/site'

const props = defineProps<{
  site: Pick<Site, 'status' | 'primary_domain' | 'aliases' | 'preview_domain'>
  /** `site.domains.manage` for the signed-in role. */
  canManage: boolean
  goLive: (body: SiteGoLive) => Promise<void>
  setAliases: (body: SiteAliases) => Promise<void>
}>()

type Mode = 'golive' | 'aliases'
const mode = ref<Mode>()
const primary = ref('')
const aliasText = ref('')
const skipDns = ref(false)
const local = ref<DomainFormErrors>({})
const failure = ref<ApiError>()
const sent = ref<{ primary?: string, aliases: string[] }>({ aliases: [] })
const busy = ref(false)

const live = computed(() => siteIsLive(props.site))
const blocked = computed(() => domainsBlockedReason(props.site))

function open(m: Mode) {
  mode.value = m
  primary.value = m === 'golive' ? (props.site.primary_domain ?? '') : ''
  aliasText.value = props.site.aliases.join('\n')
  skipDns.value = false
  local.value = {}
  failure.value = undefined
}
function close() {
  mode.value = undefined
  failure.value = undefined
}

const dnsFailed = computed(() => failure.value !== undefined && isDnsCheckError(failure.value))
const dnsLines = computed(() => (failure.value && dnsFailed.value ? dnsFailures(failure.value, sent.value) : []))
// A failure without field errors (409, 403, network, DNS check that couldn't run) is shown whole.
const alertError = computed(() => (failure.value && !dnsFailed.value && failure.value.errors.length === 0 ? failure.value : undefined))
const primaryError = computed(() => local.value.primary ?? (dnsFailed.value ? undefined : failure.value?.fieldError('/primary_domain')))
const aliasError = computed(() => {
  if (local.value.aliases) return local.value.aliases
  if (dnsFailed.value) return undefined
  return failure.value?.errors.find(e => e.field.startsWith('/aliases'))?.detail
})

async function onSubmit() {
  const m = mode.value
  if (!m) return
  const aliases = parseAliases(aliasText.value)
  local.value = m === 'golive'
    ? validateDomains(primary.value, aliases)
    : validateDomains(undefined, aliases, props.site.primary_domain ?? undefined)
  failure.value = undefined
  if (Object.keys(local.value).length > 0) return
  const prim = m === 'golive' ? normalizeDomain(primary.value) : undefined
  sent.value = { primary: prim, aliases }
  busy.value = true
  try {
    if (prim !== undefined) await props.goLive(goLivePayload(prim, aliases, skipDns.value))
    else await props.setAliases(aliasesPayload(aliases, skipDns.value))
    close()
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="site-domains-heading">
    <h2 id="site-domains-heading" class="text-lg font-semibold">Domains</h2>

    <dl class="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
      <div>
        <dt class="text-muted">Primary domain</dt>
        <dd class="break-all" data-testid="primary">{{ site.primary_domain ?? 'Not live yet' }}</dd>
      </div>
      <div>
        <dt class="text-muted">Preview domain</dt>
        <dd class="break-all">{{ site.preview_domain }}<span v-if="live" class="text-muted"> (redirects to the primary domain)</span></dd>
      </div>
      <div class="sm:col-span-2">
        <dt class="text-muted">Aliases</dt>
        <dd v-if="site.aliases.length > 0">
          <ul class="break-all" data-testid="aliases">
            <li v-for="a in site.aliases" :key="a">{{ a }}</li>
          </ul>
        </dd>
        <dd v-else>None</dd>
      </div>
    </dl>

    <p v-if="!live" class="text-sm text-muted">
      The site is on its preview domain. Point the new domain's DNS at this host, then go live: the site gets a certificate, its URLs are rewritten to the new domain and the caches are purged. The preview domain then redirects to it.
    </p>
    <p v-if="blocked" class="text-sm text-muted" role="status">{{ blocked }}</p>

    <div v-if="canManage && mode === undefined" class="flex flex-wrap gap-2">
      <UButton v-if="canGoLive(site)" icon="i-lucide-globe" data-testid="open-golive" @click="open('golive')">
        {{ live ? 'Change primary domain' : 'Go live' }}
      </UButton>
      <UButton v-if="canEditAliases(site)" icon="i-lucide-link" color="neutral" variant="outline" data-testid="open-aliases" @click="open('aliases')">
        Edit aliases
      </UButton>
    </div>

    <form v-if="canManage && mode" class="space-y-4 border-t border-default pt-4" novalidate @submit.prevent="onSubmit">
      <p v-if="mode === 'golive' && live" class="text-sm text-muted">
        Moving to another primary domain rewrites the site's URLs again. The old primary domain stops being served unless you list it as an alias.
      </p>
      <ProblemAlert v-if="alertError" :error="alertError" />

      <UFormField v-if="mode === 'golive'" label="Primary domain" name="primary_domain" required :error="primaryError" description="For example www.example.com">
        <UInput v-model="primary" autocomplete="off" autocapitalize="off" spellcheck="false" class="w-full" />
      </UFormField>
      <UFormField :label="mode === 'golive' ? 'Aliases' : 'Aliases (replaces the current list)'" name="aliases" hint="Optional" :error="aliasError" :description="`One domain per line, up to ${MAX_ALIASES}. Each redirects to the primary domain.`">
        <UTextarea v-model="aliasText" :rows="4" autocomplete="off" spellcheck="false" class="w-full" />
      </UFormField>

      <div v-if="dnsFailed" class="rounded-md border border-default bg-elevated p-3 text-sm" role="alert" data-testid="dns-result">
        <p class="font-medium">{{ failure?.title }}</p>
        <p v-if="failure?.detail" class="mt-1 text-muted">{{ failure.detail }}</p>
        <ul v-if="dnsLines.length > 0" class="mt-2 list-disc space-y-1 pl-5">
          <li v-for="l in dnsLines" :key="l.domain + l.detail"><code class="break-all">{{ l.domain }}</code>: {{ l.detail }}</li>
        </ul>
        <p class="mt-2 text-muted">Nothing was changed. Update the DNS records and give them time to propagate, then check again.</p>
        <p v-if="failure?.requestId" class="mt-1 text-xs text-muted">Request ID: {{ failure.requestId }}</p>
      </div>
      <ProblemAlert v-else-if="failure && failure.errors.length > 0" :error="failure" />

      <UCheckbox v-model="skipDns" label="Skip the DNS check" description="For hosts behind a CDN or load balancer, whose domains don't resolve to this host. Certificates still need the domains to reach this host on port 80." />

      <div class="flex flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="close()">Cancel</UButton>
        <UButton type="submit" :loading="busy" data-testid="submit">{{ dnsFailed ? 'Check again' : mode === 'golive' ? (live ? 'Change domain' : 'Go live') : 'Save aliases' }}</UButton>
      </div>
    </form>
  </section>
</template>
