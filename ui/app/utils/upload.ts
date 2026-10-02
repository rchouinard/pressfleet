import type { components } from '../api/schema.gen'
import { ApiError, toApiError } from './api-error'
import { sha256File, type Sliceable } from './sha256'

export type Upload = components['schemas']['Upload']
export type UploadCreate = components['schemas']['UploadCreate']

/** What the uploader is doing, for the progress label. */
export type UploadPhase = 'hashing' | 'uploading' | 'finishing' | 'done'

export interface UploadProgress {
  phase: UploadPhase
  /** Bytes done in this phase (hashed, or sent). */
  done: number
  total: number
}

/**
 * The four API calls the resumable-upload protocol needs. `useUpload` wires
 * them to the generated client and, for the chunk, to XMLHttpRequest (the
 * only way to see upload progress); tests wire fakes.
 */
export interface UploadTransport {
  create(body: UploadCreate): Promise<Upload>
  status(id: string): Promise<Upload>
  /** Sends `blob` as the bytes from `offset`; `onBytes` gets the bytes of it sent so far. */
  chunk(id: string, offset: number, blob: Blob, onBytes: (sent: number) => void, signal?: AbortSignal): Promise<Upload>
  complete(id: string, sha256: string): Promise<Upload>
  remove(id: string): Promise<void>
}

export interface RunUploadOptions {
  onProgress?: (p: UploadProgress) => void
  signal?: AbortSignal
  /** Failed chunks are retried this many times in a row before giving up. Default 3. */
  retries?: number
  /** Largest chunk to send, whatever the server allows. Default 16 MiB. */
  chunkSize?: number
  /** Waits between retries; tests pass an instant one. */
  sleep?: (ms: number) => Promise<void>
  /** Replaces the file hasher in tests. */
  hash?: (file: Sliceable, onProgress: (done: number, total: number) => void, signal?: AbortSignal) => Promise<string>
}

/** A file to upload: a `File`, or anything shaped like one. */
export type UploadSource = Sliceable & { readonly name: string } & Blob

const defaultSleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))

/** Whether a failed chunk is worth sending again (a dropped connection or a server hiccup), as opposed to a refusal. */
export function isRetryableChunkError(e: unknown): boolean {
  if (!(e instanceof ApiError)) return !(e instanceof DOMException && e.name === 'AbortError')
  // 0 is a network failure; 409 means the offset moved, which a status call fixes.
  return e.status === 0 || e.status === 409 || e.status >= 500
}

/**
 * Uploads `file` and returns the completed upload. Hashes it first (the API
 * needs its SHA-256), starts the upload with that checksum, sends it in
 * chunks and completes it. If a chunk fails the upload's `received_bytes` is
 * asked for and sending resumes from there, as the API specifies, so a
 * dropped connection loses at most one chunk. A failure or cancel after the
 * upload started removes it again (best effort), so a retry starts afresh.
 */
export async function runUpload(
  file: UploadSource,
  request: Omit<UploadCreate, 'size_bytes' | 'sha256' | 'filename'>,
  transport: UploadTransport,
  options: RunUploadOptions = {},
): Promise<Upload> {
  const { onProgress, signal } = options
  const retries = options.retries ?? 3
  const sleep = options.sleep ?? defaultSleep
  const hash = options.hash ?? sha256File
  const total = file.size

  onProgress?.({ phase: 'hashing', done: 0, total })
  const sha256 = await hash(file, (done, t) => onProgress?.({ phase: 'hashing', done, total: t }), signal)
  throwIfAborted(signal)

  let upload = await transport.create({ ...request, filename: file.name, size_bytes: total, sha256 })
  try {
    let failures = 0
    while (upload.received_bytes < total) {
      throwIfAborted(signal)
      const offset = upload.received_bytes
      const size = Math.min(upload.max_chunk_bytes, options.chunkSize ?? 16 * 1024 * 1024, total - offset)
      try {
        upload = await transport.chunk(
          upload.id,
          offset,
          file.slice(offset, offset + size) as Blob,
          sent => onProgress?.({ phase: 'uploading', done: offset + sent, total }),
          signal,
        )
        failures = 0
        onProgress?.({ phase: 'uploading', done: upload.received_bytes, total })
      } catch (e) {
        if (signal?.aborted || !isRetryableChunkError(e) || ++failures > retries) throw e
        await sleep(Math.min(250 * 2 ** (failures - 1), 4000))
        // None of a failed chunk counts: resume from what the server has.
        upload = await transport.status(upload.id)
      }
    }
    throwIfAborted(signal)
    onProgress?.({ phase: 'finishing', done: total, total })
    upload = await transport.complete(upload.id, sha256)
    onProgress?.({ phase: 'done', done: total, total })
    return upload
  } catch (e) {
    // A checksum mismatch already removed the upload; anything else leaves a
    // partial file that would sit until it expires, so clean it up.
    await transport.remove(upload.id).catch(() => undefined)
    throw signal?.aborted ? new UploadCancelled() : toApiError(e)
  }
}

/** The user cancelled; not a failure to report. */
export class UploadCancelled extends Error {
  constructor() {
    super('The upload was cancelled.')
    this.name = 'UploadCancelled'
  }
}

function throwIfAborted(signal?: AbortSignal) {
  if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')
}

/** Percentage 0-100 for a progress bar; 0 for an empty total. */
export function percent(p: Pick<UploadProgress, 'done' | 'total'>): number {
  if (p.total <= 0) return 0
  return Math.min(100, Math.floor((p.done / p.total) * 100))
}

/** "Checking the file", "Uploading", ... for the progress label. */
export function phaseLabel(phase: UploadPhase): string {
  switch (phase) {
    case 'hashing': return 'Checking the file'
    case 'uploading': return 'Uploading'
    case 'finishing': return 'Verifying the upload'
    case 'done': return 'Uploaded'
  }
}

/** File extensions the import and deploy endpoints accept (OpenAPI importSite, deploySiteCode). */
export const archiveExtensions = ['.tar.gz', '.tar.bz2', '.tar', '.zip'] as const
export const dumpExtensions = ['.sql.gz', '.sql'] as const
export const packageExtensions = ['.zip', '.tar.gz'] as const

/** Whether `name` ends in one of `extensions` (case-insensitive). */
export function hasExtension(name: string, extensions: readonly string[]): boolean {
  const lower = name.toLowerCase()
  return extensions.some(ext => lower.endsWith(ext))
}

/** A message for a file that can't be uploaded, or undefined when it is fine. */
export function validateUploadFile(file: { name: string, size: number } | undefined, extensions: readonly string[], what: string): string | undefined {
  if (!file) return `Choose ${what}.`
  if (file.size <= 0) return 'The file is empty.'
  if (!hasExtension(file.name, extensions)) return `Use a file ending in ${extensions.join(', ')}.`
  return undefined
}
