import { ApiError, problemFromResponse } from '../utils/api-error'
import { apiUrl } from '../utils/api-client'
import {
  runUpload, UploadCancelled, type Upload, type UploadCreate, type UploadProgress, type UploadSource, type UploadTransport,
} from '../utils/upload'

/**
 * One file upload with progress, for site imports and code deployments
 * (design doc §5.7, §5.8). Create one per file:
 *
 *     const archive = useUpload()
 *     const upload = await archive.start(file, { purpose: 'site_import', client_group_id })
 *     archive.progress.value   // { phase, done, total }
 *
 * The create, status, complete and delete calls go through the generated
 * client. Only the chunk body is sent by hand, with XMLHttpRequest, because
 * `fetch` can't report upload progress. It is a PATCH, so the CSRF token is
 * added here (the client's middleware doesn't see this request); the session
 * cookie goes along because the URL is same origin.
 */
export function useUpload() {
  const api = useApi()
  const csrf = useCsrfToken()

  const progress = ref<UploadProgress>()
  const busy = ref(false)
  let controller: AbortController | undefined

  const transport: UploadTransport = {
    async create(body: UploadCreate) {
      const { data } = await api.POST('/api/v1/uploads', { body })
      if (!data) throw new Error('The upload response was empty.')
      return data
    },
    async status(id) {
      const { data } = await api.GET('/api/v1/uploads/{upload_id}', { params: { path: { upload_id: id } } })
      if (!data) throw new Error('The upload response was empty.')
      return data
    },
    chunk: (id, offset, blob, onBytes, signal) =>
      sendChunk(apiUrl(`/api/v1/uploads/${encodeURIComponent(id)}?offset=${offset}`), blob, csrf.value, onBytes, signal),
    async complete(id, sha256) {
      const { data } = await api.POST('/api/v1/uploads/{upload_id}/complete', {
        params: { path: { upload_id: id } },
        body: { sha256 },
      })
      if (!data) throw new Error('The upload response was empty.')
      return data
    },
    async remove(id) {
      await api.DELETE('/api/v1/uploads/{upload_id}', { params: { path: { upload_id: id } } })
    },
  }

  /**
   * Uploads `file`; resolves with the complete upload, resolves `undefined`
   * if cancelled, and throws an ApiError for any other failure.
   */
  async function start(file: UploadSource, request: Omit<UploadCreate, 'size_bytes' | 'sha256' | 'filename'>): Promise<Upload | undefined> {
    controller = new AbortController()
    busy.value = true
    progress.value = { phase: 'hashing', done: 0, total: file.size }
    try {
      return await runUpload(file, request, transport, {
        signal: controller.signal,
        onProgress: (p) => {
          progress.value = p
        },
      })
    } catch (e) {
      progress.value = undefined
      if (e instanceof UploadCancelled || controller?.signal.aborted) return undefined
      throw e
    } finally {
      busy.value = false
      controller = undefined
    }
  }

  function cancel() {
    controller?.abort()
  }

  onBeforeUnmount(cancel)

  return { progress, busy, start, cancel }
}

/** PATCHes one chunk with XMLHttpRequest so the browser reports how much has left. */
function sendChunk(
  url: string,
  blob: Blob,
  csrfToken: string | undefined,
  onBytes: (sent: number) => void,
  signal?: AbortSignal,
): Promise<Upload> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('PATCH', url)
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    if (csrfToken) xhr.setRequestHeader('X-CSRF-Token', csrfToken)
    xhr.upload.onprogress = e => onBytes(e.loaded)
    xhr.onload = () => {
      const response = new Response(xhr.responseText, {
        status: xhr.status,
        headers: { 'Content-Type': xhr.getResponseHeader('Content-Type') ?? '' },
      })
      if (xhr.status >= 200 && xhr.status < 300) {
        response.json().then(resolve, reject)
      } else {
        void problemFromResponse(response).then(reject)
      }
    }
    xhr.onerror = () => reject(new ApiError({
      type: 'urn:pressfleet:ui:network',
      title: 'Network error',
      status: 0,
      detail: 'The control plane could not be reached.',
    }))
    xhr.onabort = () => reject(new DOMException('Aborted', 'AbortError'))
    signal?.addEventListener('abort', () => xhr.abort(), { once: true })
    xhr.send(blob)
  })
}
