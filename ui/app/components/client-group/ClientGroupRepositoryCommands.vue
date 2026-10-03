<script setup lang="ts">
// The command-line way to export or delete a group's backup repository,
// with the group's ID filled in (#139): run from the platform directory on
// the host, they work even where the dashboard's deletion doesn't fit, and
// export copies the whole repository for the client.
import { computed } from 'vue'
import CopyField from '../CopyField.vue'
import { repositoryCommands } from '../../utils/client-group'

const props = defineProps<{ groupId: string }>()
const commands = computed(() => repositoryCommands(props.groupId))
</script>

<template>
  <div class="space-y-2 text-sm">
    <CopyField label="Client group ID" :value="groupId" />
    <p class="text-muted">
      On the host, in the platform directory. Export copies the whole repository and its password into a directory under the
      data directory (here the default, <code>/var/lib/pressfleet</code>) to hand to the client; move it off the host afterwards.
    </p>
    <CopyField label="Export command" :value="commands.export" />
    <CopyField label="Delete command" :value="commands.delete" />
  </div>
</template>
