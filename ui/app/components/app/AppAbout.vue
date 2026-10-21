<script setup lang="ts">
// The "About PressFleet" dialog: the running control plane's version and
// build, with a one-line summary to copy into a bug report, and the newest
// release when the release check found one, with how to upgrade to it.
import { computed } from 'vue'
import { availableUpdate, buildSummary, shortCommit, type PlatformBuild, type PlatformRelease } from '../../utils/build'
import CopyButton from '../CopyButton.vue'

const props = defineProps<{ build: PlatformBuild, release?: PlatformRelease }>()
const open = defineModel<boolean>('open', { required: true })

const commit = computed(() => shortCommit(props.build))
const summary = computed(() => buildSummary(props.build))
const update = computed(() => availableUpdate(props.release))
</script>

<template>
  <UModal v-model:open="open" title="About PressFleet" description="The build of the running control plane.">
    <template #body>
      <dl class="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt class="text-muted">Version</dt>
        <dd class="font-mono text-xs" data-testid="version">{{ build.version }}</dd>
        <dt class="text-muted">Commit</dt>
        <dd class="flex min-w-0 items-center gap-1 font-mono text-xs" data-testid="commit">
          <template v-if="commit">
            <span class="truncate" :title="build.commit">{{ commit }}</span>
            <CopyButton :value="build.commit!" label="commit" />
          </template>
          <span v-else class="font-sans text-muted">Unknown</span>
        </dd>
        <dt class="text-muted">Go</dt>
        <dd class="font-mono text-xs">{{ build.go_version }}</dd>
        <dt class="text-muted">Platform</dt>
        <dd class="font-mono text-xs">{{ build.os }}/{{ build.arch }}</dd>
        <template v-if="release">
          <dt class="text-muted">Latest release</dt>
          <dd class="text-xs" data-testid="latest">
            <span class="font-mono">{{ release.latest_version }}</span>
            <span v-if="!update" class="text-muted"> (up to date)</span>
          </dd>
        </template>
      </dl>
      <div v-if="update" class="mt-4 rounded-md border border-default p-3 text-sm" data-testid="update">
        <p class="font-medium">PressFleet {{ update.latest_version }} is available</p>
        <p class="mt-1 text-muted">
          To upgrade, install its <code class="font-mono text-xs">pressfleet</code> CLI on the host and run
          <code class="font-mono text-xs">pressfleet upgrade</code>.
        </p>
        <a
          :href="update.release_url"
          target="_blank"
          rel="noopener noreferrer"
          class="mt-2 inline-block rounded text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary"
        >
          Release notes and downloads
        </a>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap items-center justify-between gap-2">
        <span class="flex items-center gap-1 text-xs text-muted">
          Build summary
          <CopyButton :value="summary" label="build summary" />
        </span>
        <UButton color="neutral" variant="outline" @click="open = false">Close</UButton>
      </div>
    </template>
  </UModal>
</template>
