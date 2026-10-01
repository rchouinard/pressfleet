/**
 * Permission checks for the signed-in user, backed by the one table in
 * `utils/permissions.ts`. Reactive: call `can('site.delete')` in a template
 * or computed and it updates when the session changes.
 *
 *     const { can } = useCan()
 *     <UButton v-if="can('site.delete')" ... />
 *
 * Hide what the role can't do; don't rely on it. The API answers 403 either way.
 */
export function useCan() {
  const { role } = useSession()
  return {
    role,
    can: (action: Action) => can(role.value, action),
  }
}
