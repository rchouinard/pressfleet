import type { components } from '../api/schema.gen'

export type ClientGroup = components['schemas']['ClientGroup']
export type ClientGroupCreate = components['schemas']['ClientGroupCreate']
export type ClientGroupUpdate = components['schemas']['ClientGroupUpdate']

/** The editable fields, as the form holds them. */
export interface ClientGroupValues {
  name: string
  notes: string
}

export const NAME_MAX = 100
export const NOTES_MAX = 10000

/** Client-side checks that mirror the spec; the server's 422 stays authoritative. */
export function validateClientGroup(v: ClientGroupValues): Partial<Record<keyof ClientGroupValues, string>> {
  const errors: Partial<Record<keyof ClientGroupValues, string>> = {}
  const name = v.name.trim()
  if (name.length === 0) errors.name = 'Enter a name.'
  else if (name.length > NAME_MAX) errors.name = `Use at most ${NAME_MAX} characters.`
  if (v.notes.length > NOTES_MAX) errors.notes = `Use at most ${NOTES_MAX} characters.`
  return errors
}

/** The body for creating a group; empty notes are left out. */
export function createPayload(v: ClientGroupValues): ClientGroupCreate {
  const body: ClientGroupCreate = { name: v.name.trim() }
  if (v.notes !== '') body.notes = v.notes
  return body
}

/**
 * The PATCH body: only fields that changed (the API requires at least one),
 * or `undefined` when nothing changed so the caller can skip the request.
 */
export function updatePayload(original: Pick<ClientGroup, 'name' | 'notes'>, v: ClientGroupValues): ClientGroupUpdate | undefined {
  const body: ClientGroupUpdate = {}
  if (v.name.trim() !== original.name) body.name = v.name.trim()
  if (v.notes !== original.notes) body.notes = v.notes
  return Object.keys(body).length > 0 ? body : undefined
}

/** "1 site", "3 sites". */
export function siteCountLabel(n: number): string {
  return n === 1 ? '1 site' : `${n} sites`
}
