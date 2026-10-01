<script setup lang="ts">
// The signed-in landing page: a greeting, shortcuts to the areas that exist,
// and the latest jobs. A shell only; the platform overview (#91) replaces
// the cards later.
import { isLive, navFor } from '../utils/nav'
import { toApiError } from '../utils/api-error'

useHead({ title: 'Dashboard' })

const api = useApi()
const { user, role } = useSession()

const areas = computed(() => navFor(role.value).filter(i => isLive(i) && i.id !== 'dashboard'))
const soon = computed(() => navFor(role.value).filter(i => !isLive(i)))

const { data: recent, status, refresh } = useAsyncData('dashboard-recent-jobs', async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', { params: { query: { limit: 5 } } })
    return { jobs: data?.items ?? [], error: null }
  } catch (e) {
    return { jobs: [], error: toApiError(e) }
  }
})
</script>

<template>
  <div class="space-y-8">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Welcome back</h1>
      <p class="text-muted">
        Signed in as <span class="font-medium text-default">{{ user?.email }}</span> ({{ role }}).
      </p>
    </header>

    <section aria-labelledby="areas-heading" class="space-y-3">
      <h2 id="areas-heading" class="font-medium">Areas</h2>
      <ul class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <li v-for="item in areas" :key="item.id">
          <NuxtLink
            :to="item.route!"
            class="flex items-center gap-3 rounded-lg border border-default p-4 outline-none hover:bg-elevated focus-visible:ring-2 focus-visible:ring-primary"
          >
            <UIcon :name="item.icon" class="size-6 text-primary" aria-hidden="true" />
            <span class="font-medium">{{ item.label }}</span>
          </NuxtLink>
        </li>
      </ul>
      <p v-if="soon.length" class="text-sm text-muted">
        Coming soon: {{ soon.map(i => i.label).join(', ') }}.
      </p>
    </section>

    <section aria-labelledby="recent-heading" class="space-y-3">
      <div class="flex items-center justify-between gap-3">
        <h2 id="recent-heading" class="font-medium">Recent jobs</h2>
        <div class="flex items-center gap-2">
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-refresh-cw" aria-label="Refresh recent jobs" :loading="status === 'pending'" @click="refresh()" />
          <NuxtLink to="/jobs" class="text-sm text-primary underline underline-offset-2">All jobs</NuxtLink>
        </div>
      </div>
      <ProblemAlert v-if="recent?.error" :error="recent.error" />
      <p v-else-if="!recent" class="text-sm text-muted" role="status">Loading jobs...</p>
      <p v-else-if="recent.jobs.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        No jobs yet.
      </p>
      <JobTable v-else :jobs="recent.jobs" caption="Most recent jobs" />
    </section>
  </div>
</template>
