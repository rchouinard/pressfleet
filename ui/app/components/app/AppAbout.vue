<script setup lang="ts">
// The "About PressFleet" dialog: the running control plane's version and
// build, with a one-line summary to copy into a bug report.
import { computed } from 'vue'
import { buildSummary, shortCommit, type PlatformBuild } from '../../utils/build'
import CopyButton from '../CopyButton.vue'

const props = defineProps<{ build: PlatformBuild }>()
const open = defineModel<boolean>('open', { required: true })

const commit = computed(() => shortCommit(props.build))
const summary = computed(() => buildSummary(props.build))
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
      </dl>
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
