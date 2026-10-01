<script setup lang="ts">
// The app-level error page (unknown routes and uncaught errors). Defining it
// keeps Nuxt's built-in error pages out of the bundle; those inject an inline
// script that the Content-Security-Policy would block.
import type { NuxtError } from '#app'

const props = defineProps<{ error: NuxtError }>()

const notFound = computed(() => props.error.status === 404)
useHead({ title: computed(() => (notFound.value ? 'Page not found' : 'Something went wrong')) })
</script>

<template>
  <UApp>
    <main id="main" class="mx-auto flex min-h-screen max-w-xl flex-col items-start justify-center gap-4 px-4 sm:px-6">
      <p class="text-sm font-medium text-muted">{{ error.status }}</p>
      <h1 class="text-3xl font-semibold tracking-tight">
        {{ notFound ? 'Page not found' : 'Something went wrong' }}
      </h1>
      <p class="text-muted">
        {{ notFound ? 'There is nothing at this address.' : 'An unexpected error occurred in the dashboard.' }}
      </p>
      <UButton icon="i-lucide-arrow-left" @click="clearError({ redirect: '/' })">Back to the dashboard</UButton>
    </main>
  </UApp>
</template>
