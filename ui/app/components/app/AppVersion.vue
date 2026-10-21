<script setup lang="ts">
// The running version at the foot of the navigation, with a badge when a
// newer release is available; opens the About dialog. Hidden until the
// platform has answered.
import { computed } from 'vue'
import { availableUpdate, type PlatformBuild, type PlatformRelease } from '../../utils/build'

const props = defineProps<{ build?: PlatformBuild, release?: PlatformRelease }>()
defineEmits<{ about: [] }>()

const update = computed(() => availableUpdate(props.release))
</script>

<template>
  <button
    v-if="build"
    type="button"
    class="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-xs text-muted outline-none hover:bg-elevated focus-visible:ring-2 focus-visible:ring-primary"
    :title="update ? `PressFleet ${build.version}: ${update.latest_version} is available` : `PressFleet ${build.version}: about this build`"
    @click="$emit('about')"
  >
    <span class="truncate">PressFleet {{ build.version }}</span>
    <UBadge v-if="update" color="info" variant="subtle" size="sm" class="shrink-0" data-testid="update-badge">Update available</UBadge>
  </button>
</template>
