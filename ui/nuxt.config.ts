// The dev server proxies API calls to a locally running `pressfleet serve`.
const devApi = process.env.PRESSFLEET_DEV_API ?? 'http://localhost:8080'

// SPA build (`nuxt generate`): the Go binary embeds the static output, so
// there is no Node.js server in production (design doc §3).
export default defineNuxtConfig({
  ssr: false,
  compatibilityDate: '2026-09-01',
  devtools: { enabled: false },
  telemetry: false,

  modules: ['@nuxt/ui', '@nuxt/eslint'],
  css: ['~/assets/css/main.css'],

  ui: {
    // Fonts are not fetched at build time: the dashboard uses the system
    // font stack, so builds work offline and the CSP needs no font origin.
    fonts: false,
  },
  icon: {
    // Bundle the used Lucide icons instead of fetching them from the Iconify
    // API at runtime (self-hosted installs may have no outbound access, and
    // the CSP only allows same-origin requests).
    provider: 'none',
    clientBundle: { scan: true },
  },

  typescript: { strict: true },

  app: {
    head: {
      title: 'PressFleet',
      htmlAttrs: { lang: 'en' },
      meta: [{ name: 'viewport', content: 'width=device-width, initial-scale=1' }],
      link: [{ rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }],
    },
  },

  // In development the UI runs on its own port and proxies the API, so the
  // session cookie stays same-origin.
  nitro: {
    devProxy: {
      '/api': { target: `${devApi}/api`, changeOrigin: false },
      '/healthz': { target: `${devApi}/healthz`, changeOrigin: false },
    },
  },
})
