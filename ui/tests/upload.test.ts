import { describe, expect, it } from 'vitest'
import { ApiError } from '../app/utils/api-error'
import {
  hasExtension, isRetryableChunkError, percent, phaseLabel, runUpload, UploadCancelled, validateUploadFile,
  type Upload, type UploadProgress, type UploadTransport,
} from '../app/utils/upload'

function network(): ApiError {
  return new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Network error', status: 0 })
}
function problem(status: number): ApiError {
  return new ApiError({ type: 'about:blank', title: 'x', status })
}

function makeUpload(over: Partial<Upload> = {}): Upload {
  return {
    id: 'u1', purpose: 'site_import', client_group_id: 'g1', site_id: null, filename: 'a.zip', size_bytes: 10,
    received_bytes: 0, sha256: 'ab', status: 'uploading', job_id: null, max_chunk_bytes: 4,
    created_at: '', updated_at: '', expires_at: null, ...over,
  }
}

/** A server that stores bytes and can be told to fail chunks. */
function fakeServer(size: number, failures: (n: number) => ApiError | undefined = () => undefined) {
  const state = { received: 0, chunks: 0, calls: [] as string[], removed: false, created: undefined as unknown }
  const transport: UploadTransport = {
    async create(body) {
      state.calls.push('create')
      state.created = body
      return makeUpload({ size_bytes: size })
    },
    async status() {
      state.calls.push('status')
      return makeUpload({ size_bytes: size, received_bytes: state.received })
    },
    async chunk(_id, offset, blob, onBytes) {
      state.calls.push(`chunk@${offset}`)
      const f = failures(state.chunks++)
      onBytes(blob.size / 2)
      if (f) throw f
      expect(offset).toBe(state.received)
      state.received += blob.size
      return makeUpload({ size_bytes: size, received_bytes: state.received })
    },
    async complete(_id, sha) {
      state.calls.push(`complete:${sha}`)
      return makeUpload({ size_bytes: size, received_bytes: state.received, status: 'complete' })
    },
    async remove() {
      state.removed = true
    },
  }
  return { state, transport }
}

const file = (n: number, name = 'a.zip') => Object.assign(new Blob([new Uint8Array(n)]), { name }) as unknown as File
const base = { purpose: 'site_import' as const, client_group_id: 'g1' }
const fast = { sleep: async () => {}, hash: async () => 'abc123' }

describe('runUpload', () => {
  it('declares size and checksum, sends chunks in order within the server limit, then completes', async () => {
    const { state, transport } = fakeServer(10)
    const progress: UploadProgress[] = []
    const result = await runUpload(file(10), base, transport, { ...fast, onProgress: p => progress.push(p) })
    expect(result.status).toBe('complete')
    expect(state.created).toMatchObject({ purpose: 'site_import', client_group_id: 'g1', filename: 'a.zip', size_bytes: 10, sha256: 'abc123' })
    // max_chunk_bytes is 4: 4 + 4 + 2.
    expect(state.calls).toEqual(['create', 'chunk@0', 'chunk@4', 'chunk@8', 'complete:abc123'])
    expect(progress[0]).toMatchObject({ phase: 'hashing' })
    expect(progress.at(-1)).toEqual({ phase: 'done', done: 10, total: 10 })
    // Progress within a chunk is reported (half of the first 4 bytes).
    expect(progress.some(p => p.phase === 'uploading' && p.done === 2)).toBe(true)
  })

  it('resumes from received_bytes after a failed chunk', async () => {
    const { state, transport } = fakeServer(10, n => (n === 1 ? network() : undefined))
    await runUpload(file(10), base, transport, fast)
    expect(state.calls).toEqual(['create', 'chunk@0', 'chunk@4', 'status', 'chunk@4', 'chunk@8', 'complete:abc123'])
    expect(state.removed).toBe(false)
  })

  it('gives up after the retry limit, removes the upload and reports the error', async () => {
    const { state, transport } = fakeServer(10, () => network())
    await expect(runUpload(file(10), base, transport, { ...fast, retries: 2 })).rejects.toMatchObject({ status: 0 })
    expect(state.calls.filter(c => c.startsWith('chunk'))).toHaveLength(3)
    expect(state.removed).toBe(true)
  })

  it('does not retry a refusal', async () => {
    const { state, transport } = fakeServer(10, () => problem(413))
    await expect(runUpload(file(10), base, transport, fast)).rejects.toMatchObject({ status: 413 })
    expect(state.calls.filter(c => c.startsWith('chunk'))).toHaveLength(1)
  })

  it('cancels between chunks, removing the upload', async () => {
    const { state, transport } = fakeServer(10)
    const c = new AbortController()
    await expect(runUpload(file(10), base, transport, {
      ...fast,
      signal: c.signal,
      onProgress: (p) => {
        if (p.phase === 'uploading' && p.done >= 4) c.abort()
      },
    })).rejects.toBeInstanceOf(UploadCancelled)
    expect(state.removed).toBe(true)
    expect(state.calls).not.toContain('complete:abc123')
  })

  it('never starts an upload if cancelled while hashing', async () => {
    const { state, transport } = fakeServer(10)
    const c = new AbortController()
    await expect(runUpload(file(10), base, transport, {
      sleep: async () => {},
      signal: c.signal,
      hash: async () => {
        c.abort()
        return 'abc'
      },
    })).rejects.toThrow()
    expect(state.calls).toEqual([])
  })
})

describe('helpers', () => {
  it('classifies retryable chunk errors', () => {
    expect(isRetryableChunkError(network())).toBe(true)
    expect(isRetryableChunkError(problem(409))).toBe(true)
    expect(isRetryableChunkError(problem(503))).toBe(true)
    expect(isRetryableChunkError(problem(413))).toBe(false)
    expect(isRetryableChunkError(problem(403))).toBe(false)
    expect(isRetryableChunkError(new DOMException('x', 'AbortError'))).toBe(false)
  })

  it('computes percentages', () => {
    expect(percent({ done: 0, total: 0 })).toBe(0)
    expect(percent({ done: 50, total: 200 })).toBe(25)
    expect(percent({ done: 300, total: 200 })).toBe(100)
  })

  it('labels every phase', () => {
    for (const p of ['hashing', 'uploading', 'finishing', 'done'] as const) expect(phaseLabel(p)).not.toBe('')
  })

  it('checks file extensions case-insensitively', () => {
    expect(hasExtension('Site.TAR.GZ', ['.tar.gz'])).toBe(true)
    expect(hasExtension('dump.sql', ['.sql.gz'])).toBe(false)
    expect(validateUploadFile(undefined, ['.zip'], 'the package')).toBe('Choose the package.')
    expect(validateUploadFile({ name: 'a.zip', size: 0 }, ['.zip'], 'x')).toBe('The file is empty.')
    expect(validateUploadFile({ name: 'a.rar', size: 5 }, ['.zip'], 'x')).toMatch(/\.zip/)
    expect(validateUploadFile({ name: 'a.zip', size: 5 }, ['.zip'], 'x')).toBeUndefined()
  })
})
