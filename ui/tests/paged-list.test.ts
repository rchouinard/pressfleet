import { describe, expect, it } from 'vitest'
import { ApiError } from '../app/utils/api-error'
import { createPagedList, type Page } from '../app/utils/paged-list'

function pages(...ps: Page<number>[]) {
  const calls: (string | undefined)[] = []
  const fetchPage = async (cursor: string | undefined) => {
    calls.push(cursor)
    const p = ps[cursor ? Number(cursor) : 0]
    return p
  }
  return { fetchPage, calls }
}

describe('createPagedList', () => {
  it('loads the first page, then appends on loadMore until the cursor ends', async () => {
    const { fetchPage, calls } = pages(
      { items: [1, 2], next_cursor: '1' },
      { items: [3], next_cursor: null },
    )
    const list = createPagedList(fetchPage)

    await list.load()
    expect(list.items.value).toEqual([1, 2])
    expect(list.cursor.value).toBe('1')

    await list.loadMore()
    expect(list.items.value).toEqual([1, 2, 3])
    expect(list.cursor.value).toBeNull()

    await list.loadMore() // nothing left: no request
    expect(calls).toEqual([undefined, '1'])
  })

  it('reload replaces the items and starts from the first page again', async () => {
    const { fetchPage } = pages({ items: [1], next_cursor: '1' }, { items: [2], next_cursor: null })
    const list = createPagedList(fetchPage)
    await list.load()
    await list.loadMore()
    await list.load()
    expect(list.items.value).toEqual([1])
    expect(list.cursor.value).toBe('1')
  })

  it('keeps a failed first page as an ApiError and clears it on the next success', async () => {
    let fail = true
    const list = createPagedList<number>(async () => {
      if (fail) throw new ApiError({ type: 'about:blank', title: 'Forbidden', status: 403 })
      return { items: [1], next_cursor: null }
    })
    await list.load()
    expect(list.error.value?.status).toBe(403)
    expect(list.items.value).toEqual([])

    fail = false
    await list.load()
    expect(list.error.value).toBeUndefined()
    expect(list.items.value).toEqual([1])
  })

  it('throws from loadMore and leaves the shown items alone', async () => {
    let n = 0
    const list = createPagedList<number>(async () => {
      if (n++ === 0) return { items: [1], next_cursor: 'x' }
      throw new Error('network')
    })
    await list.load()
    await expect(list.loadMore()).rejects.toThrow('network')
    expect(list.items.value).toEqual([1])
    expect(list.cursor.value).toBe('x')
    expect(list.loadingMore.value).toBe(false)
  })

  it('ignores a stale load that finishes after a newer one', async () => {
    const resolvers: ((p: Page<number>) => void)[] = []
    const list = createPagedList<number>(() => new Promise((r) => { resolvers.push(r) }))
    const first = list.load()
    const second = list.load()
    resolvers[1]!({ items: [2], next_cursor: null })
    resolvers[0]!({ items: [1], next_cursor: null })
    await Promise.all([first, second])
    expect(list.items.value).toEqual([2])
    expect(list.loading.value).toBe(false)
  })
})
