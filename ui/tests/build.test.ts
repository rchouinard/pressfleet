import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import AppAbout from '../app/components/app/AppAbout.vue'
import AppVersion from '../app/components/app/AppVersion.vue'
import { availableUpdate, buildSummary, shortCommit, type PlatformBuild, type PlatformRelease } from '../app/utils/build'
import { uiStubs } from './stubs'

const build = (over: Partial<PlatformBuild> = {}): PlatformBuild => ({
  version: 'v0.4.0',
  commit: 'adbf70e45beb349cc44022eebc2afaa2d55790e8',
  modified: false,
  go_version: 'go1.27.1',
  os: 'linux',
  arch: 'amd64',
  ...over,
})

const release = (over: Partial<PlatformRelease> = {}): PlatformRelease => ({
  latest_version: 'v0.5.0',
  release_url: 'https://github.com/rchouinard/pressfleet/releases/tag/v0.5.0',
  checked_at: '2026-10-20T12:00:00Z',
  update_available: true,
  ...over,
})

describe('build helpers', () => {
  it('offers an update only when the release check says so', () => {
    expect(availableUpdate(release())?.latest_version).toBe('v0.5.0')
    expect(availableUpdate(release({ update_available: false }))).toBeUndefined()
    expect(availableUpdate(undefined)).toBeUndefined()
  })

  it('shortens the commit like `pressfleet version`', () => {
    expect(shortCommit(build())).toBe('adbf70e45beb')
    expect(shortCommit(build({ modified: true }))).toBe('adbf70e45beb-dirty')
    expect(shortCommit(build({ commit: undefined, modified: undefined }))).toBe('')
  })

  it('summarizes the build on one line', () => {
    expect(buildSummary(build())).toBe('pressfleet v0.4.0 (commit adbf70e45beb, go1.27.1, linux/amd64)')
    expect(buildSummary(build({ version: 'dev', commit: undefined }))).toBe('pressfleet dev (go1.27.1, linux/amd64)')
  })
})

const UModal = defineComponent({
  props: ['open', 'title', 'description'],
  setup(props: { open: boolean }, { slots }) {
    return () => (props.open ? h('div', { role: 'dialog' }, [slots.body?.(), slots.footer?.()]) : null)
  },
})
const global = { stubs: { ...uiStubs, UModal } }

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('AppAbout', () => {
  it('shows the build and copies the full commit', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })

    const wrapper = mount(AppAbout, { props: { build: build(), open: true }, global })
    expect(wrapper.find('[data-testid=version]').text()).toBe('v0.4.0')
    expect(wrapper.find('[data-testid=commit]').text()).toContain('adbf70e45beb')
    expect(wrapper.text()).toContain('go1.27.1')
    expect(wrapper.text()).toContain('linux/amd64')

    await wrapper.find('[data-testid=commit] [data-testid=copy]').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('adbf70e45beb349cc44022eebc2afaa2d55790e8')
  })

  it('points to `pressfleet upgrade` when a newer release is available', () => {
    const wrapper = mount(AppAbout, { props: { build: build(), release: release(), open: true }, global })
    expect(wrapper.find('[data-testid=latest]').text()).toBe('v0.5.0')
    const update = wrapper.find('[data-testid=update]')
    expect(update.text()).toContain('PressFleet v0.5.0 is available')
    expect(update.text()).toContain('pressfleet upgrade')
    const link = update.find('a')
    expect(link.attributes('href')).toBe('https://github.com/rchouinard/pressfleet/releases/tag/v0.5.0')
    expect(link.attributes('rel')).toContain('noopener')
  })

  it('says when the running version is the newest', () => {
    const wrapper = mount(AppAbout, { props: { build: build({ version: 'v0.5.0' }), release: release({ update_available: false }), open: true }, global })
    expect(wrapper.find('[data-testid=latest]').text()).toBe('v0.5.0 (up to date)')
    expect(wrapper.find('[data-testid=update]').exists()).toBe(false)
  })

  it('leaves the release out when nothing is known', () => {
    const wrapper = mount(AppAbout, { props: { build: build(), open: true }, global })
    expect(wrapper.find('[data-testid=latest]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=update]').exists()).toBe(false)
  })

  it('says when the commit is unknown', () => {
    const wrapper = mount(AppAbout, { props: { build: build({ commit: undefined, modified: undefined }), open: true }, global })
    expect(wrapper.find('[data-testid=commit]').text()).toBe('Unknown')
    expect(wrapper.find('[data-testid=commit] [data-testid=copy]').exists()).toBe(false)
  })
})

describe('AppVersion', () => {
  it('shows the version and asks for the About dialog', async () => {
    const wrapper = mount(AppVersion, { props: { build: build() } })
    expect(wrapper.text()).toBe('PressFleet v0.4.0')
    await wrapper.find('button').trigger('click')
    expect(wrapper.emitted('about')).toHaveLength(1)
  })

  it('flags an available update, in words', () => {
    const wrapper = mount(AppVersion, { props: { build: build(), release: release() }, global: { stubs: uiStubs } })
    expect(wrapper.find('[data-testid=update-badge]').text()).toBe('Update available')
    expect(wrapper.find('button').attributes('title')).toBe('PressFleet v0.4.0: v0.5.0 is available')

    const current = mount(AppVersion, { props: { build: build(), release: release({ update_available: false }) }, global: { stubs: uiStubs } })
    expect(current.find('[data-testid=update-badge]').exists()).toBe(false)
  })

  it('renders nothing before the platform answers', () => {
    expect(mount(AppVersion).find('button').exists()).toBe(false)
  })
})
