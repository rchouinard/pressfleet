<script setup lang="ts">
// A QR code of `value` (an otpauth:// URI), drawn by uqr as SVG and shown
// as a `data:` image, which the CSP allows. The image is computed from the
// prop and kept nowhere else, so it goes when the caller drops the value.
// Dark on white whatever the theme: scanners expect that contrast.
import { computed } from 'vue'
import { renderSVG } from 'uqr'

const props = defineProps<{ value: string, alt: string }>()

const src = computed(() => `data:image/svg+xml;charset=utf-8,${encodeURIComponent(renderSVG(props.value, { ecc: 'M', border: 2, pixelSize: 8 }))}`)
</script>

<template>
  <img :src="src" :alt="alt" width="200" height="200" class="size-50 rounded-md border border-default bg-white" data-testid="qr-code">
</template>
