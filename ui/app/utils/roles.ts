import type { components } from '../api/schema.gen'

/** Platform-wide role (design doc §4.6). */
export type Role = components['schemas']['Role']

const rank: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 }

/**
 * Whether `role` is at least `minimum`. Use it to hide controls the user can't
 * use; the API enforces the real permission, so this is only a convenience.
 * Role-aware composables (#81) build on it.
 */
export function roleAtLeast(role: Role | undefined, minimum: Role): boolean {
  return role !== undefined && rank[role] >= rank[minimum]
}
