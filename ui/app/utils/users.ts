import type { components } from '../api/schema.gen'
import type { Role } from './roles'

export type User = components['schemas']['User']
export type UserCreate = components['schemas']['UserCreate']
export type UserUpdate = components['schemas']['UserUpdate']

export const PASSWORD_MIN = 12
export const PASSWORD_MAX = 256

export const roleOptions: readonly { value: Role, label: string, hint: string }[] = [
  { value: 'viewer', label: 'Viewer', hint: 'Read-only access to sites, jobs, backups and monitoring.' },
  { value: 'operator', label: 'Operator', hint: 'Manage sites, backups, deploys and client groups.' },
  { value: 'admin', label: 'Admin', hint: 'Everything, including users, platform settings and deletions.' },
]

export const roleColors = { admin: 'primary', operator: 'info', viewer: 'neutral' } as const

export interface UserStatus {
  label: 'Active' | 'Disabled'
  color: 'success' | 'neutral'
  icon: string
}

export function userStatus(user: Pick<User, 'disabled'>): UserStatus {
  return user.disabled
    ? { label: 'Disabled', color: 'neutral', icon: 'i-lucide-user-x' }
    : { label: 'Active', color: 'success', icon: 'i-lucide-circle-check' }
}

const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export interface CreateUserInput {
  email: string
  password: string
  role: Role
}

/** Early checks for the create form; the server's 422 field errors are shown as well. */
export function validateCreateUser(input: CreateUserInput): { email?: string, password?: string } {
  const errors: { email?: string, password?: string } = {}
  if (!EMAIL.test(input.email.trim())) errors.email = 'Enter a valid email address.'
  if (input.password.length < PASSWORD_MIN) errors.password = `Use at least ${PASSWORD_MIN} characters.`
  else if (input.password.length > PASSWORD_MAX) errors.password = `Use at most ${PASSWORD_MAX} characters.`
  return errors
}

export function userCreateBody(input: CreateUserInput): UserCreate {
  return { email: input.email.trim(), password: input.password, role: input.role }
}

/**
 * The PATCH body for an edit: only what changed (the API requires at least
 * one field and says only present fields change). `undefined` means nothing
 * to send.
 */
export function userUpdateBody(user: Pick<User, 'role' | 'disabled'>, next: { role: Role, disabled: boolean }): UserUpdate | undefined {
  const body: UserUpdate = {}
  if (next.role !== user.role) body.role = next.role
  if (next.disabled !== user.disabled) body.disabled = next.disabled
  return Object.keys(body).length > 0 ? body : undefined
}

/**
 * Whether `user` is the signed-in user. The UI doesn't offer to change your
 * own role or disable yourself: locking yourself out is easy to do by
 * accident and can't be undone by you, so it is left to another admin. (The
 * server already refuses to demote or disable the last Admin.)
 */
export function isSelf(user: Pick<User, 'id'>, current: Pick<User, 'id'> | null | undefined): boolean {
  return !!current && user.id === current.id
}
