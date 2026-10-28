<script setup lang="ts">
import type { ThemeChoice } from '~/composables/useTheme'

// The account button of the top bar: who is signed in and in which role,
// and a menu with the account pages, the theme and Sign out. Without
// accounts (single-tenant) it holds the links and the theme only.
const session = useSession()
const route = useRoute()
const { choice: theme } = useTheme()
const open = ref(false)
const root = ref<HTMLElement | null>(null)
const button = ref<HTMLButtonElement | null>(null)
// Escape closes the menu and gives focus back to the button that opened it.
function escape() {
  if (!open.value) return
  open.value = false
  button.value?.focus()
}
const id = useId()
watch(() => route.fullPath, () => (open.value = false))

const user = computed(() => session.me.value?.user)
const initials = computed(() => {
  const name = user.value?.name?.trim() || user.value?.email || ''
  const parts = name.split(/[\s@._-]+/).filter(Boolean)
  return ((parts[0]?.[0] ?? '') + (parts[1]?.[0] ?? '')).toUpperCase() || '·'
})
const role = computed(() => {
  const m = session.current.value
  return m ? `${m.role[0]!.toUpperCase()}${m.role.slice(1)} · ${m.org.name}` : session.isPlatformAdmin.value ? 'Platform admin' : ''
})
const links = computed(() => session.accounts.value
  ? [
      { to: '/settings/profile', label: 'Your account' },
      { to: '/settings/profile?tab=organisations', label: 'Your organisations' },
      { to: '/settings', label: 'Settings & API keys' },
      { to: '/stats', label: 'Statistics' },
      { to: '/help', label: 'Help' },
    ]
  : [
      { to: '/settings', label: 'Settings' },
      { to: '/stats', label: 'Statistics' },
      { to: '/help', label: 'Help' },
    ])
const welcome = useWelcome()
async function openGuide() {
  welcome.reopen()
  open.value = false
  await navigateTo(session.restricted.value ? '/admin/overview' : '/')
}
const themes: { id: ThemeChoice; label: string }[] = [{ id: 'system', label: 'Auto' }, { id: 'light', label: 'Light' }, { id: 'dark', label: 'Dark' }]

function outside(e: MouseEvent) {
  if (open.value && root.value && !root.value.contains(e.target as Node)) open.value = false
}
onMounted(() => document.addEventListener('mousedown', outside))
onBeforeUnmount(() => document.removeEventListener('mousedown', outside))
</script>

<template>
  <div ref="root" class="um" @keydown.escape="escape">
    <button ref="button" type="button" class="um__button" :aria-expanded="open" :aria-controls="`${id}-menu`" @click="open = !open">
      <span v-if="user" class="um__avatar" aria-hidden="true">{{ initials }}</span>
      <span v-if="user" class="um__who">
        <span class="um__name">{{ user.name || user.email }}</span>
        <span v-if="role" class="um__role">{{ role }}</span>
      </span>
      <span v-else>Settings</span>
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 9l6 6 6-6" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" /></svg>
    </button>
    <div v-if="open" :id="`${id}-menu`" class="um__menu" role="group" aria-label="Account">
      <div v-if="user" class="um__head">
        <span class="um__name">{{ user.name || user.email }}</span>
        <span class="um__email">{{ user.email }}</span>
      </div>
      <NuxtLink v-for="l in links" :key="l.to" :to="l.to" class="um__item">{{ l.label }}</NuxtLink>
      <button v-if="user" type="button" class="um__item um__out" @click="openGuide">Welcome guide</button>
      <div class="um__theme" role="radiogroup" aria-label="Theme">
        <button v-for="t in themes" :key="t.id" type="button" class="chip" :class="{ active: theme === t.id }" role="radio" :aria-checked="theme === t.id" @click="theme = t.id">{{ t.label }}</button>
      </div>
      <button v-if="user" type="button" class="um__item um__out" @click="session.logout()">Sign out</button>
    </div>
  </div>
</template>

<style scoped>
.um { position: relative; }
.um__button { gap: 10px; min-height: 48px; padding: 4px 14px 4px 4px; background: var(--panel-2); color: var(--ink); }
.um__button:hover:not(:disabled) { background: var(--panel-2); box-shadow: inset 0 0 0 2px var(--line-strong); color: var(--ink); }
.um__avatar { width: 40px; height: 40px; border-radius: 50%; display: grid; place-items: center; background: var(--accent); color: var(--on-accent); font: 800 14px var(--font-display); }
.um__who { display: flex; flex-direction: column; align-items: flex-start; line-height: 1.25; min-width: 0; }
.um__name { font-weight: 700; font-size: 14px; max-width: 180px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.um__role { font-size: 12px; font-weight: 500; color: var(--muted); }
.um__menu { position: absolute; z-index: 30; right: 0; top: calc(100% + 8px); width: 260px; padding: 8px; display: flex; flex-direction: column; gap: 2px; background: var(--panel); border: 2px solid var(--line-strong); border-radius: var(--radius-md); box-shadow: var(--shadow-pop); }
.um__head { display: flex; flex-direction: column; padding: 8px 12px 10px; border-bottom: 2px solid var(--line); margin-bottom: 4px; }
.um__email { font-size: 13px; color: var(--muted); overflow-wrap: anywhere; }
.um__item { display: flex; align-items: center; min-height: 40px; padding: 8px 12px; border-radius: var(--radius-sm); color: var(--ink); font-weight: 600; font-size: 14px; background: none; justify-content: flex-start; }
.um__item:hover, .um__item:hover:not(:disabled) { background: var(--panel-2); color: var(--ink); text-decoration: none; }
.um__theme { display: flex; gap: 6px; padding: 8px 12px; border-top: 2px solid var(--line); border-bottom: 2px solid var(--line); margin: 4px 0; }
.um__theme .chip { flex: 1; justify-content: center; min-height: 32px; padding: 4px 8px; }
.um__out { width: 100%; }
@media (max-width: 720px) { .um__who { display: none; } }
</style>
