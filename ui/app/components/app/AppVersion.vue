<script setup lang="ts">
// The running version at the foot of the navigation; opens the About
// dialog. Hidden until the platform has answered.
import type { PlatformBuild } from '../../utils/build'

defineProps<{ build?: PlatformBuild }>()
defineEmits<{ about: [] }>()
</script>

<template>
  <button
    v-if="build"
    type="button"
    class="w-full truncate rounded-md px-3 py-2 text-left text-xs text-muted outline-none hover:bg-elevated focus-visible:ring-2 focus-visible:ring-primary"
    :title="`PressFleet ${build.version}: about this build`"
    @click="$emit('about')"
  >
    PressFleet {{ build.version }}
  </button>
</template>
