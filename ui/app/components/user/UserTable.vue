<script setup lang="ts">
// Users as a table: name (the email address below it, if there is a display
// name), role, status, two-factor, last sign-in, and Edit, Reset two-factor
// and Delete buttons. Whether Delete and Reset show is the page's decision
// (`canDelete`, `canResetTwoFactor`, from useCan); neither shows for the
// signed-in user (the API refuses resetting one's own second factors), and
// Reset only for users with a second factor.
// The table scrolls sideways inside its own region on narrow screens.
import { formatTimestamp } from '../../utils/tokens'
import { isSelf, roleColors, twoFactorStatus, userLabel, userStatus, type User } from '../../utils/users'

defineProps<{ users: readonly User[], caption: string, currentUserId?: string, canDelete?: boolean, canResetTwoFactor?: boolean }>()
defineEmits<{ edit: [user: User], delete: [user: User], resetTwoFactor: [user: User] }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[48rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">User</th>
          <th scope="col" class="px-3 py-2 font-medium">Role</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Two-factor</th>
          <th scope="col" class="px-3 py-2 font-medium">Last sign-in</th>
          <th scope="col" class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="user in users" :key="user.id">
          <th scope="row" class="px-3 py-2 font-medium">
            <span class="[overflow-wrap:anywhere]">{{ userLabel(user) }}</span>
            <UBadge v-if="isSelf(user, currentUserId ? { id: currentUserId } : undefined)" color="neutral" variant="outline" size="sm" class="ml-2">You</UBadge>
            <span v-if="user.display_name" class="block text-xs font-normal text-muted [overflow-wrap:anywhere]">{{ user.email }}</span>
          </th>
          <td class="px-3 py-2"><UBadge :color="roleColors[user.role]" variant="subtle" size="sm">{{ user.role }}</UBadge></td>
          <td class="px-3 py-2">
            <UBadge :color="userStatus(user).color" variant="subtle" size="sm" :icon="userStatus(user).icon">{{ userStatus(user).label }}</UBadge>
          </td>
          <td class="px-3 py-2">
            <UBadge :color="twoFactorStatus(user).color" variant="subtle" size="sm" :icon="twoFactorStatus(user).icon">{{ twoFactorStatus(user).label }}</UBadge>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(user.last_login_at, 'Never') }}</td>
          <td class="px-3 py-2 text-right">
            <UButton
              color="neutral"
              variant="ghost"
              size="sm"
              icon="i-lucide-pencil"
              :aria-label="`Edit ${userLabel(user)}`"
              data-testid="edit"
              @click="$emit('edit', user)"
            >
              Edit
            </UButton>
            <UButton
              v-if="canResetTwoFactor && user.two_factor_enabled && !isSelf(user, currentUserId ? { id: currentUserId } : undefined)"
              color="warning"
              variant="ghost"
              size="sm"
              icon="i-lucide-shield-off"
              :aria-label="`Reset two-factor for ${userLabel(user)}`"
              data-testid="reset-two-factor"
              @click="$emit('resetTwoFactor', user)"
            >
              Reset two-factor
            </UButton>
            <UButton
              v-if="canDelete && !isSelf(user, currentUserId ? { id: currentUserId } : undefined)"
              color="error"
              variant="ghost"
              size="sm"
              icon="i-lucide-trash-2"
              :aria-label="`Delete ${userLabel(user)}`"
              data-testid="delete"
              @click="$emit('delete', user)"
            >
              Delete
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
