import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// Unit tests run in plain Node/happy-dom against the code in app/utils and
// app/components that doesn't need the Nuxt runtime. Keep that code free of
// Nuxt auto-imports (import what you use) so it stays testable here.
export default defineConfig({
  plugins: [vue()],
  test: {
    environment: 'happy-dom',
    include: ['tests/**/*.test.ts'],
  },
})
