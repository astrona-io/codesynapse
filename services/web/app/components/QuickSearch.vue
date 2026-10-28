<script setup lang="ts">
import type { SiteSearch, SiteSearchHit } from '~/utils/types'

// The search box of the top bar: finds pages and repositories as you type,
// then symbols, entry points, findings and docs from the API (GET /search)
// in the repositories you may see, and opens the one picked. ⌘K / Ctrl+K
// focuses it from anywhere; arrow keys move, Enter opens, Escape closes.
export interface SearchItem { label: string; to: string; group: string; hint?: string }

const props = defineProps<{ items: SearchItem[]; repo?: string }>()
const router = useRouter()
const query = ref('')
const open = ref(false)
const active = ref(0)
const input = ref<HTMLInputElement | null>(null)
const id = useId()

const remote = ref<SearchItem[]>([])
const searching = ref(false)
const local = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return []
  const words = q.split(/\s+/)
  return props.items
    .filter((it) => words.every((w) => `${it.label} ${it.hint ?? ''} ${it.group}`.toLowerCase().includes(w)))
    .sort((a, b) => Number(!a.label.toLowerCase().startsWith(q)) - Number(!b.label.toLowerCase().startsWith(q)))
    .slice(0, 6)
})
const results = computed(() => {
  const seen = new Set(local.value.map((it) => it.to))
  return [...local.value, ...remote.value.filter((it) => !seen.has(it.to))].slice(0, 20)
})

const kindGroup: Record<string, string> = {
  Function: 'Symbol', Method: 'Symbol', InterfaceMethod: 'Symbol', Type: 'Type',
  Route: 'Entry point', Finding: 'Finding', Section: 'Doc', Repository: 'Repository',
}
function toItem(h: SiteSearchHit): SearchItem {
  const base = `/repos/${encodeURIComponent(h.repo)}`
  const group = h.kind === 'Repository' ? 'Repository' : `${kindGroup[h.kind] ?? h.kind} · ${h.repo}`
  switch (h.kind) {
    case 'Repository': return { label: h.name, to: base, group }
    case 'Finding': return { label: h.name, to: `${base}/security?q=${encodeURIComponent(h.name)}`, group, hint: [h.severity, h.location].filter(Boolean).join(' · ') }
    case 'Section': return { label: h.name, to: `${base}/docs?file=${encodeURIComponent(h.file ?? '')}`, group, hint: h.location }
    default: return { label: h.name, to: `${base}/explore?id=${encodeURIComponent(h.id ?? h.name)}`, group, hint: h.location }
  }
}

// Ask the API once typing pauses; a newer query cancels an older one.
let timer: ReturnType<typeof setTimeout> | undefined
let inflight: AbortController | undefined
watch(query, (q) => {
  active.value = 0
  open.value = true
  clearTimeout(timer)
  inflight?.abort()
  const text = q.trim()
  if (text.length < 2) { remote.value = []; searching.value = false; return }
  searching.value = true
  timer = setTimeout(async () => {
    const ctl = new AbortController()
    inflight = ctl
    try {
      const params = new URLSearchParams({ q: text })
      if (props.repo) params.set('repo', props.repo)
      const res = await apiGet<SiteSearch>(`search?${params}`, { signal: ctl.signal })
      if (!ctl.signal.aborted) remote.value = res.results.map(toItem)
    } catch {
      if (!ctl.signal.aborted) remote.value = []
    } finally {
      if (!ctl.signal.aborted) searching.value = false
    }
  }, 250)
})
onBeforeUnmount(() => { clearTimeout(timer); inflight?.abort() })

function go(it: SearchItem | undefined) {
  if (!it) return
  open.value = false
  query.value = ''
  input.value?.blur()
  router.push(it.to)
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') { e.preventDefault(); active.value = Math.min(active.value + 1, results.value.length - 1) }
  else if (e.key === 'ArrowUp') { e.preventDefault(); active.value = Math.max(active.value - 1, 0) }
  else if (e.key === 'Enter') { e.preventDefault(); go(results.value[active.value]) }
  else if (e.key === 'Escape') { open.value = false; input.value?.blur() }
}
function shortcut(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    input.value?.focus()
  }
}
onMounted(() => window.addEventListener('keydown', shortcut))
onBeforeUnmount(() => window.removeEventListener('keydown', shortcut))
// Said once typing settles, in a region that stays mounted so screen
// readers hear the change.
const announce = computed(() => {
  if (!open.value || !query.value.trim()) return ''
  if (searching.value) return 'Searching…'
  return results.value.length ? `${results.value.length} results` : 'No results'
})
const isMac = ref(false)
onMounted(() => { isMac.value = /Mac|iPhone|iPad/.test(navigator.platform) })
</script>

<template>
  <div class="qs" @focusout="(e: FocusEvent) => { if (!(e.currentTarget as HTMLElement).contains(e.relatedTarget as Node)) open = false }">
    <label class="qs__box" :for="`${id}-q`">
      <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true"><circle cx="7" cy="7" r="5" stroke="currentColor" stroke-width="2" /><path d="M11 11l3 3" stroke="currentColor" stroke-width="2" stroke-linecap="round" /></svg>
      <span class="sr-only">Search</span>
      <input
        :id="`${id}-q`" ref="input" v-model="query" type="search" placeholder="Search symbols, repos, findings…" autocomplete="off"
        role="combobox" aria-autocomplete="list" :aria-expanded="open && results.length > 0" :aria-controls="`${id}-list`"
        :aria-activedescendant="results.length ? `${id}-opt-${active}` : undefined"
        @keydown="onKey" @focus="open = true"
      >
      <kbd>{{ isMac ? '⌘' : 'Ctrl' }} K</kbd>
    </label>
    <ul v-if="open && query.trim()" :id="`${id}-list`" class="qs__list" role="listbox" aria-label="Search results">
      <li v-for="(it, i) in results" :id="`${id}-opt-${i}`" :key="it.to + it.label" role="option" :aria-selected="i === active">
        <a :href="it.to" :class="{ active: i === active }" @mousedown.prevent="go(it)" @mouseenter="active = i">
          <span class="qs__text"><span class="qs__label">{{ it.label }}</span><span v-if="it.hint" class="qs__hint">{{ it.hint }}</span></span>
          <span class="qs__group">{{ it.group }}</span>
        </a>
      </li>
      <li v-if="searching" class="qs__none">Searching the graph…</li>
      <li v-else-if="!results.length" class="qs__none">Nothing matches “{{ query.trim() }}”.</li>
    </ul>
    <span class="sr-only" role="status">{{ announce }}</span>
  </div>
</template>

<style scoped>
.qs { position: relative; flex: 1 1 320px; min-width: 0; max-width: 520px; }
.qs__box { display: flex; align-items: center; gap: 10px; min-height: 44px; padding: 0 8px 0 14px; border-radius: var(--pill); background: var(--panel-2); color: var(--ink); font-weight: 400; cursor: text; }
.qs__box:focus-within { box-shadow: inset 0 0 0 2px var(--line-strong); }
.qs__box input { flex: 1; min-width: 0; min-height: 0; padding: 0; border: 0; background: transparent; box-shadow: none; font-size: 14px; }
.qs__box input:focus { box-shadow: none; }
kbd { flex: none; padding: 3px 8px; border-radius: var(--radius-xs); background: var(--panel); font: 600 11.5px var(--font-mono); color: var(--muted); }
.qs__list { position: absolute; z-index: 30; top: calc(100% + 8px); left: 0; right: 0; margin: 0; padding: 8px; list-style: none; background: var(--panel); border: 2px solid var(--line-strong); border-radius: var(--radius-md); box-shadow: var(--shadow-pop); max-height: 420px; overflow-y: auto; }
.qs__list a { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: 40px; padding: 8px 12px; border-radius: var(--radius-sm); color: var(--ink); }
.qs__list a.active { background: var(--panel-2); text-decoration: none; }
.qs__text { display: flex; flex-direction: column; min-width: 0; }
.qs__label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
.qs__hint { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font: 12px var(--font-mono); color: var(--muted); }
.qs__group { flex: none; max-width: 45%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; color: var(--muted); }
.qs__none { padding: 10px 12px; color: var(--muted); font-size: 14px; }
</style>
