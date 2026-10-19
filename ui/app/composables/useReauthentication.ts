import { shallowRef } from 'vue'
import type { ApiError } from '../utils/api-error'

interface Pending {
  /** The refusal that asked for it. */
  error: ApiError
  promise: Promise<boolean>
  resolve: (reauthenticated: boolean) => void
}

// Module state, like sessionProbe: it holds a resolver, which useState
// (serialized into the payload) shouldn't.
const pending = shallowRef<Pending>()

/**
 * The reauthentication prompt shared by the API client and the dialog in
 * app.vue (AuthReauthenticateDialog). `ask` opens the dialog and resolves
 * with whether the user reauthenticated; asking while it is open joins the
 * same prompt. `finish` is the dialog's answer.
 */
export function useReauthentication() {
  function ask(error: ApiError): Promise<boolean> {
    if (pending.value) return pending.value.promise
    let resolve!: (ok: boolean) => void
    const promise = new Promise<boolean>((r) => {
      resolve = r
    })
    pending.value = { error, promise, resolve }
    return promise
  }

  function finish(reauthenticated: boolean) {
    const p = pending.value
    pending.value = undefined
    p?.resolve(reauthenticated)
  }

  return { pending, ask, finish }
}
