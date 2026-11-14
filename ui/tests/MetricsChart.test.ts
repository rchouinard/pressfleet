import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import * as vue from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { MetricDescriptor, MetricResult } from '../app/utils/metrics'

const plots: { el: HTMLElement, destroyed: boolean }[] = []

vi.mock('uplot', () => ({
  default: class {
    destroyed = false
    constructor(_opts: unknown, _data: unknown, public el: HTMLElement) {
      plots.push(this)
    }

    destroy() {
      this.destroyed = true
    }

    setSize() {}
  },
}))
vi.mock('uplot/dist/uPlot.min.css', () => ({}))

const observed: Element[] = []
class FakeResizeObserver {
  observe(el: Element) {
    observed.push(el)
  }

  disconnect() {}
}

const descriptor = { id: 'm', title: 'Requests', description: 'Requests per second', unit: 'per_second', kind: 'rate' } as MetricDescriptor
const range = { start: 1_700_000_000, end: 1_700_003_600 }
const empty: MetricResult = { id: 'm', unit: 'per_second', kind: 'rate', series: [] }
const filled: MetricResult = {
  id: 'm', unit: 'per_second', kind: 'rate',
  series: [{ labels: {}, points: [{ time: '2023-11-14T22:14:00Z', value: 1 }, { time: '2023-11-14T22:15:00Z', value: 2 }] }],
} as MetricResult

beforeEach(() => {
  plots.length = 0
  observed.length = 0
  for (const name of ['ref', 'computed', 'watch', 'onMounted', 'onBeforeUnmount'] as const) vi.stubGlobal(name, vue[name])
  vi.stubGlobal('useColorMode', () => vue.ref('light'))
  vi.stubGlobal('ResizeObserver', FakeResizeObserver)
})
afterEach(() => vi.unstubAllGlobals())

describe('MetricsChart', () => {
  it('draws, and watches the size of, a chart whose data arrives after it mounted empty', async () => {
    const { default: MetricsChart } = await import('../app/components/metrics/MetricsChart.vue')
    const w = mount(MetricsChart, { props: { descriptor, result: empty, range } })
    await flushPromises()
    expect(plots).toHaveLength(0)

    await w.setProps({ result: filled })
    await flushPromises()

    const host = w.find('[role="img"]').element
    expect(plots.map(p => p.el)).toEqual([host])
    expect(observed).toEqual([host])
  })

  it('observes the host once when the chart was drawn from the start', async () => {
    const { default: MetricsChart } = await import('../app/components/metrics/MetricsChart.vue')
    const w = mount(MetricsChart, { props: { descriptor, result: filled, range } })
    await flushPromises()
    expect(plots).toHaveLength(1)
    expect(observed).toEqual([w.find('[role="img"]').element])
  })
})
