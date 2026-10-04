<script setup lang="ts">
// The signed-in user's menu: who they are and their role, the theme choice,
// About PressFleet (the dialog lives in the layout), and sign out. The trigger shows the role badge from `md` up; the menu
// always shows it.
import type { DropdownMenuItem } from '@nuxt/ui'
import { userLabel } from '../../utils/users'

const { user, logout } = useSession()
const colorMode = useColorMode()
const notify = useNotify()
const about = useState<boolean>('about-open', () => false)

const roleColor = { admin: 'primary', operator: 'info', viewer: 'neutral' } as const

function theme(value: 'system' | 'light' | 'dark', label: string, icon: string): DropdownMenuItem {
  return {
    label,
    icon,
    type: 'checkbox',
    checked: colorMode.preference === value,
    onUpdateChecked: () => {
      colorMode.preference = value
    },
    onSelect: (e: Event) => e.preventDefault(),
  }
}

const items = computed<DropdownMenuItem[][]>(() => [
  [{ label: user.value ? userLabel(user.value) : undefined, type: 'label', slot: 'identity' }],
  [
    { label: 'Theme', type: 'label' },
    theme('system', 'System', 'i-lucide-monitor'),
    theme('light', 'Light', 'i-lucide-sun'),
    theme('dark', 'Dark', 'i-lucide-moon'),
  ],
  [
    { label: 'Account & tokens', icon: 'i-lucide-key-round', to: '/account' },
    {
      label: 'About PressFleet',
      icon: 'i-lucide-info',
      onSelect: () => {
        about.value = true
      },
    },
    {
      label: 'Sign out',
      icon: 'i-lucide-log-out',
      onSelect: async () => {
        try {
          await logout()
        } catch (e) {
          notify.error(e)
        }
      },
    },
  ],
])
</script>

<template>
  <UDropdownMenu v-if="user" :items="items" :content="{ align: 'end' }">
    <UButton color="neutral" variant="ghost" icon="i-lucide-circle-user-round" trailing-icon="i-lucide-chevron-down" aria-label="User menu">
      <span class="hidden max-w-48 truncate sm:inline">{{ userLabel(user) }}</span>
      <UBadge class="hidden md:inline-flex" :color="roleColor[user.role]" variant="subtle" size="sm">{{ user.role }}</UBadge>
    </UButton>

    <template #identity>
      <div class="flex min-w-0 flex-col gap-1 text-left">
        <span class="truncate text-sm font-medium text-default">{{ userLabel(user) }}</span>
        <span v-if="user.display_name" class="truncate text-xs text-muted">{{ user.email }}</span>
        <span class="flex items-center gap-2 text-xs text-muted">
          Role
          <UBadge :color="roleColor[user.role]" variant="subtle" size="sm">{{ user.role }}</UBadge>
        </span>
      </div>
    </template>
  </UDropdownMenu>
</template>
