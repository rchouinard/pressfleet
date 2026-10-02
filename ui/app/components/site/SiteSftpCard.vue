<script setup lang="ts">
// A site's SFTP connection details. The gateway is on the dashboard's host;
// the SSH username names the site and the key names the user, so there is no
// password to show. The port isn't in the API: the default is shown, with
// where to look if the install changed it.
import { computed } from 'vue'
import { DEFAULT_SFTP_PORT, sftpDetails } from '../../utils/sftp'
import CopyField from '../CopyField.vue'

const props = defineProps<{
  slug: string
  /** The dashboard's host name, where the gateway listens. */
  host: string
  /** Whether the viewer's role may connect (Operators and Admins). */
  canConnect: boolean
}>()

const details = computed(() => sftpDetails(props.slug, props.host))
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="sftp-heading">
    <h2 id="sftp-heading" class="text-lg font-semibold">SFTP</h2>
    <p class="text-sm text-muted">
      Upload files to this site's <code>wp-content</code> (shown as <code>/</code>) with an SSH key registered on
      <NuxtLink to="/account" class="text-primary underline">your account</NuxtLink>. There are no passwords, and no shell.
      <template v-if="!canConnect"> Your role can't connect: only Operators and Admins can.</template>
    </p>
    <CopyField label="Command" :value="details.command" />
    <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
      <div><dt class="text-muted">Host</dt><dd class="break-all font-mono text-xs">{{ details.host }}</dd></div>
      <div><dt class="text-muted">Port</dt><dd class="font-mono text-xs">{{ details.port }}</dd></div>
      <div><dt class="text-muted">Username</dt><dd class="break-all font-mono text-xs">{{ details.username }}</dd></div>
    </dl>
    <p class="text-xs text-muted">
      Port {{ DEFAULT_SFTP_PORT }} is the default; if this install changed <code>sftp.port</code>, use that port instead.
    </p>
  </section>
</template>
