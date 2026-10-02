<script setup lang="ts">
// A newly opened database session: the temporary credentials and the tunnel
// command, shown once. Everything lives in the `opened` prop; the page drops
// it when the dialog closes, and nothing is stored, put in a URL or logged.
// The API can't show the password again, and neither can this component.
import type { DatabaseSessionOpened } from '../../utils/database'
import { formatTimestamp } from '../../utils/tokens'
import CopyField from '../CopyField.vue'

defineProps<{ opened: DatabaseSessionOpened }>()
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" role="note">
      <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <p>
        Copy these now. The password is shown only once and can't be retrieved later; end the session and open a new one if you lose it.
        The session ({{ opened.session.access === 'read_write' ? 'read-write' : 'read-only' }}) ends on {{ formatTimestamp(opened.session.expires_at) }}.
      </p>
    </div>

    <section class="space-y-1" aria-labelledby="db-tunnel-heading">
      <h3 id="db-tunnel-heading" class="text-sm font-semibold">1. Open the tunnel</h3>
      <p class="text-sm text-muted">
        Run this in a terminal and leave it running. It needs an SSH key registered on your own account (see Account &amp; tokens).
      </p>
      <CopyField label="Tunnel command" :value="opened.tunnel.ssh_command" />
    </section>

    <section class="space-y-1" aria-labelledby="db-connect-heading">
      <h3 id="db-connect-heading" class="text-sm font-semibold">2. Connect your database tool</h3>
      <p class="text-sm text-muted">To {{ opened.tunnel.local_host }}, port {{ opened.tunnel.local_port }}, with:</p>
      <div class="grid gap-x-3 sm:grid-cols-2">
        <CopyField label="Host" :value="opened.tunnel.local_host" />
        <CopyField label="Port" :value="String(opened.tunnel.local_port)" />
        <CopyField label="Username" :value="opened.credentials.username" />
        <CopyField label="Password" :value="opened.credentials.password" />
        <CopyField label="Database" :value="opened.credentials.database" />
      </div>
    </section>
  </div>
</template>
