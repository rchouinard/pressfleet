<script setup lang="ts">
// Users as a table: email, role, status, last sign-in, and an Edit button.
// The table scrolls sideways inside its own region on narrow screens.
import { formatTimestamp } from '../../utils/tokens'
import { isSelf, roleColors, userStatus, type User } from '../../utils/users'

defineProps<{ users: readonly User[], caption: string, currentUserId?: string }>()
defineEmits<{ edit: [user: User] }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Email</th>
          <th scope="col" class="px-3 py-2 font-medium">Role</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Last sign-in</th>
          <th scope="col" class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="user in users" :key="user.id">
          <th scope="row" class="px-3 py-2 font-medium">
            <span class="[overflow-wrap:anywhere]">{{ user.email }}</span>
            <UBadge v-if="isSelf(user, currentUserId ? { id: currentUserId } : undefined)" color="neutral" variant="outline" size="sm" class="ml-2">You</UBadge>
          </th>
          <td class="px-3 py-2"><UBadge :color="roleColors[user.role]" variant="subtle" size="sm">{{ user.role }}</UBadge></td>
          <td class="px-3 py-2">
            <UBadge :color="userStatus(user).color" variant="subtle" size="sm" :icon="userStatus(user).icon">{{ userStatus(user).label }}</UBadge>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(user.last_login_at, 'Never') }}</td>
          <td class="px-3 py-2 text-right">
            <UButton
              color="neutral"
              variant="ghost"
              size="sm"
              icon="i-lucide-pencil"
              :aria-label="`Edit ${user.email}`"
              data-testid="edit"
              @click="$emit('edit', user)"
            >
              Edit
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
