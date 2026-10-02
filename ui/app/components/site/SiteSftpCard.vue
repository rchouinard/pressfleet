<script setup lang="ts">
// A site's SFTP connection details. The gateway is on the dashboard's host;
// the SSH username names the site and the key names the user, so there is no
// password to show. Host and port come from the API (`GET /api/v1/platform`).
import { computed } from 'vue'
import { sftpDetails } from '../../utils/sftp'
import CopyField from '../CopyField.vue'

const props = defineProps<{
  slug: string
  /** The gateway's host name (the dashboard domain). */
  host: string
  /** The configured `sftp.port`. */
  port: number
  /** Whether the gateway runs on this install. */
  enabled: boolean
  /** Whether the viewer's role may connect (Operators and Admins). */
  canConnect: boolean
}>()

const details = computed(() => sftpDetails(props.slug, props.host, props.port))
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="sftp-heading">
    <h2 id="sftp-heading" class="text-lg font-semibold">SFTP</h2>
    <p class="text-sm text-muted">
      Upload files to this site's <code>wp-content</code> (shown as <code>/</code>) with an SSH key registered on
      <NuxtLink to="/account" class="text-primary underline">your account</NuxtLink>. There are no passwords, and no shell.
      <template v-if="!canConnect"> Your role can't connect: only Operators and Admins can.</template>
    </p>
    <p v-if="!enabled" class="text-sm text-muted">The SFTP gateway is turned off on this install.</p>
    <CopyField label="Command" :value="details.command" />
    <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
      <div><dt class="text-muted">Host</dt><dd class="break-all font-mono text-xs">{{ details.host }}</dd></div>
      <div><dt class="text-muted">Port</dt><dd class="font-mono text-xs">{{ details.port }}</dd></div>
      <div><dt class="text-muted">Username</dt><dd class="break-all font-mono text-xs">{{ details.username }}</dd></div>
    </dl>
  </section>
</template>
