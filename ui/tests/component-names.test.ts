import { readdirSync, readFileSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Nuxt registers a component under its directory and file names joined,
// with a file name that repeats its directory's name not repeated:
// components/alert/AlertBadge.vue is <AlertBadge>, but
// components/overview/CapacityWarning.vue is <OverviewCapacityWarning>. A
// template that uses a name Nuxt doesn't register renders nothing, with no
// build or type error, so this checks every template against the names.

const app = resolve(process.cwd(), 'app')

function vueFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap(e =>
    e.isDirectory() ? vueFiles(join(dir, e.name)) : e.name.endsWith('.vue') ? [join(dir, e.name)] : [])
}

const pascal = (s: string) => s.split(/[-_]/).map(p => p.charAt(0).toUpperCase() + p.slice(1)).join('')

/** The name Nuxt registers for a component file, e.g. overview/CapacityWarning.vue as OverviewCapacityWarning. */
function componentName(path: string): string {
  const parts = path.replace(/\.vue$/, '').split('/')
  const file = parts.pop()!
  const prefix = parts.map(pascal).join('')
  return file.startsWith(prefix) ? file : prefix + file
}

const registered = new Set(vueFiles(join(app, 'components')).map(f => componentName(relative(join(app, 'components'), f))))

// Components from Vue, Nuxt and Nuxt UI, which the app's own names never start with.
const external = /^(U[A-Z]|Nuxt|Lazy|ClientOnly$|Teleport$|Transition|KeepAlive$|Suspense$|Icon$)/

describe('component names', () => {
  it('follow Nuxt\'s directory prefix rule', () => {
    expect(componentName('alert/AlertBadge.vue')).toBe('AlertBadge')
    expect(componentName('overview/CapacityWarning.vue')).toBe('OverviewCapacityWarning')
    expect(componentName('client-group/ClientGroupTable.vue')).toBe('ClientGroupTable')
    expect(componentName('ProblemAlert.vue')).toBe('ProblemAlert')
  })

  it('in every template are components Nuxt registers, or imported', () => {
    const unknown: string[] = []
    for (const file of vueFiles(app)) {
      const source = readFileSync(file, 'utf8')
      const template = source.slice(source.indexOf('<template>'))
      const imported = new Set([...source.matchAll(/import\s+(\w+)\s+from\s/g)].map(m => m[1]))
      for (const [, tag] of template.matchAll(/<([A-Z][A-Za-z0-9]*)/g)) {
        if (!registered.has(tag!) && !imported.has(tag!) && !external.test(tag!)) unknown.push(`${relative(app, file)}: <${tag}>`)
      }
    }
    expect([...new Set(unknown)]).toEqual([])
  })
})
