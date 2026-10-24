<script setup lang="ts">
import type { AppNotifications } from '~/utils/types'

// The bell of the top bar: what happened lately in your repositories (new
// serious findings after a sync, failed syncs, new reviews). The API keeps
// your "seen up to" mark with accounts; without them this browser does.
const session = useSession()
const route = useRoute()
const open = ref(false)
const root = ref<HTMLElement | null>(null)
const id = useId()
const { data, refresh } = useApiGet<AppNotifications>('notifications')
const localSeen = ref('')
const SEEN_KEY = 'cs_notifications_seen'
onMounted(() => { try { localSeen.value = localStorage.getItem(SEEN_KEY) ?? '' } catch { /* no storage: everything stays new */ } })

const seenAt = computed(() => (session.accounts.value ? data.value?.seen_at ?? '' : localSeen.value))
const items = computed(() => (data.value?.items ?? []).map((n) => ({ ...n, unread: !seenAt.value || n.at > seenAt.value })))
const unread = computed(() => items.value.filter((n) => n.unread).length)

async function markRead() {
  const at = new Date().toISOString().replace(/\.\d+Z$/, 'Z')
  if (session.accounts.value) {
    try { await apiPost('notifications/seen', { at }) } catch { return }
    await refresh()
  } else {
    localSeen.value = at
    try { localStorage.setItem(SEEN_KEY, at) } catch { /* kept for this page only */ }
  }
}

// Poll while the tab is visible; catch up when it is shown again.
let timer: ReturnType<typeof setInterval> | undefined
const poll = () => { if (document.visibilityState === 'visible') refresh() }
onMounted(() => {
  timer = setInterval(poll, 60_000)
  document.addEventListener('visibilitychange', poll)
  document.addEventListener('mousedown', outside)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  document.removeEventListener('visibilitychange', poll)
  document.removeEventListener('mousedown', outside)
})
function outside(e: MouseEvent) {
  if (open.value && root.value && !root.value.contains(e.target as Node)) open.value = false
}
watch(() => route.fullPath, () => (open.value = false))
</script>

<template>
  <div ref="root" class="nb" @keydown.escape="open = false">
    <button type="button" class="nb__button" :aria-expanded="open" :aria-controls="`${id}-panel`" :aria-label="unread ? `Notifications, ${unread} new` : 'Notifications'" @click="open = !open">
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M6 16V11a6 6 0 0 1 12 0v5l1.5 2h-15L6 16zM10 20a2 2 0 0 0 4 0" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" /></svg>
      <span v-if="unread" class="nb__count" aria-hidden="true">{{ unread > 9 ? '9+' : unread }}</span>
    </button>
    <div v-if="open" :id="`${id}-panel`" class="nb__panel" role="region" aria-label="Notifications">
      <div class="nb__head">
        <span class="nb__title">Notifications</span>
        <button v-if="unread" type="button" class="link small" @click="markRead">Mark all as read</button>
      </div>
      <ul v-if="items.length" class="nb__list">
        <li v-for="n in items" :key="n.id">
          <NuxtLink :to="n.link" class="nb__item" :class="{ 'is-unread': n.unread }">
            <span class="nb__tone" :class="`nb__tone--${n.tone}`" aria-hidden="true" />
            <span class="nb__text">
              <span class="nb__item-title">{{ n.title }}</span>
              <span v-if="n.body" class="nb__body">{{ n.body }}</span>
              <span class="nb__when">{{ relTime(n.at) }}</span>
            </span>
            <span v-if="n.unread" class="nb__dot"><span class="sr-only">new</span></span>
          </NuxtLink>
        </li>
      </ul>
      <p v-else class="nb__empty">Nothing new in the last two weeks. New serious findings after a sync, failed syncs and new reviews show up here.</p>
    </div>
  </div>
</template>

<style scoped>
.nb { position: relative; }
.nb__button { position: relative; width: 48px; min-height: 48px; padding: 0; background: var(--panel-2); color: var(--ink); }
.nb__button:hover:not(:disabled) { background: var(--panel-2); box-shadow: inset 0 0 0 2px var(--line-strong); color: var(--ink); }
.nb__count { position: absolute; top: 4px; right: 2px; min-width: 20px; height: 20px; padding: 0 5px; border-radius: 999px; background: var(--accent); color: var(--on-accent); font: 800 11.5px/20px var(--font-sans); text-align: center; }
.nb__panel { position: absolute; z-index: 30; right: 0; top: calc(100% + 8px); width: min(380px, calc(100vw - 32px)); padding: 8px; background: var(--panel); border: 2px solid var(--line-strong); border-radius: 20px; box-shadow: var(--shadow-pop); }
.nb__head { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 8px 12px 10px; border-bottom: 2px solid var(--line); margin-bottom: 4px; }
.nb__title { font: 800 16px var(--font-display); letter-spacing: -0.02em; }
.nb__list { list-style: none; margin: 0; padding: 0; max-height: 420px; overflow-y: auto; }
.nb__item { display: flex; gap: 12px; align-items: flex-start; padding: 10px 12px; border-radius: 14px; color: var(--ink); }
.nb__item:hover { background: var(--panel-2); text-decoration: none; color: var(--ink); }
.nb__tone { flex: none; width: 10px; height: 10px; margin-top: 6px; border-radius: 50%; background: var(--info); }
.nb__tone--bad { background: var(--danger); }
.nb__tone--warn { background: var(--warning); }
.nb__text { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.nb__item-title { font-weight: 600; font-size: 14px; line-height: 1.35; }
.is-unread .nb__item-title { font-weight: 700; }
.nb__body { font-size: 13px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nb__when { font-size: 12px; color: var(--muted); }
.nb__dot { flex: none; width: 10px; height: 10px; margin-top: 6px; border-radius: 50%; background: var(--accent); }
.nb__empty { padding: 12px; font-size: 13.5px; color: var(--muted); }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
