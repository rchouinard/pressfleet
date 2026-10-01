<script setup lang="ts">
// A site's initial WordPress admin credentials, shown once. The values live
// only in this component's props: never in storage, the URL or a log, and the
// caller drops them when the dialog closes. Copying uses the clipboard API
// (secure contexts); the fields are also selectable for the manual route.
import { ref } from 'vue'
import type { SiteAdminCredentials } from '../../utils/site'

defineProps<{ credentials: SiteAdminCredentials }>()

const copied = ref<string>()
const failed = ref(false)

async function copy(label: string, value: string, el: HTMLInputElement | null) {
  failed.value = false
  try {
    await navigator.clipboard.writeText(value)
    copied.value = label
  } catch {
    // No clipboard access (plain HTTP): select the text so Ctrl+C works.
    copied.value = undefined
    failed.value = true
    el?.select()
  }
}
const fields = [
  { key: 'username', label: 'Username' },
  { key: 'password', label: 'Password' },
] as const
const inputs = ref<Record<string, HTMLInputElement | null>>({})
</script>

<template>
  <div class="space-y-3">
    <div class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" role="note">
      <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <p>Copy the password now. It is shown only once and can't be retrieved again. If you lose it, reset it from WordPress's "Lost your password?" link.</p>
    </div>
    <dl class="space-y-3">
      <div v-for="f in fields" :key="f.key">
        <dt class="mb-1 text-sm font-medium"><label :for="`site-cred-${f.key}`">{{ f.label }}</label></dt>
        <dd class="flex flex-col gap-2 sm:flex-row">
          <input
            :id="`site-cred-${f.key}`"
            :ref="(el) => { inputs[f.key] = el as HTMLInputElement | null }"
            :value="credentials[f.key]"
            readonly
            spellcheck="false"
            autocomplete="off"
            class="min-w-0 flex-1 rounded-md border border-default bg-default px-3 py-2 font-mono text-sm outline-none focus-visible:ring-2 focus-visible:ring-primary"
            @focus="inputs[f.key]?.select()"
          >
          <UButton icon="i-lucide-copy" color="neutral" variant="outline" :data-testid="`copy-${f.key}`" @click="copy(f.label, credentials[f.key], inputs[f.key] ?? null)">Copy {{ f.label.toLowerCase() }}</UButton>
        </dd>
      </div>
      <div>
        <dt class="text-sm font-medium">Email</dt>
        <dd class="break-all text-sm">{{ credentials.email }}</dd>
      </div>
      <div>
        <dt class="text-sm font-medium">Log in at</dt>
        <dd class="break-all text-sm"><a :href="credentials.login_url" target="_blank" rel="noopener noreferrer" class="text-primary underline">{{ credentials.login_url }}</a></dd>
      </div>
    </dl>
    <p class="min-h-5 text-sm" role="status" aria-live="polite">
      <template v-if="copied">{{ copied }} copied to the clipboard.</template>
      <template v-else-if="failed">Couldn't copy automatically. The text is selected: press Ctrl+C (Cmd+C on a Mac).</template>
    </p>
  </div>
</template>
