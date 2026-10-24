<script setup lang="ts">
import type { AttackSurface, EntryPoint, Sink } from '~/utils/types'

// The repository's attack surface: every HTTP route, MCP tool and CLI
// command it registers, the guards seen where it is registered, and the
// sensitive operations its handler reaches. Filters live in the URL.
const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
const kind = computed(() => (route.query.kind as string | undefined) ?? '')
const reaching = computed(() => (route.query.reaching as string | undefined) ?? '')
const unguarded = computed(() => route.query.unguarded === 'true')
const q = ref((route.query.q as string | undefined) ?? '')
const set = (key: string, value: string) => router.replace({ query: { ...route.query, [key]: value || undefined } })
let typing: ReturnType<typeof setTimeout> | undefined
watch(q, (v) => { clearTimeout(typing); typing = setTimeout(() => set('q', v.trim()), 250) })

const query = computed(() => {
  const p = new URLSearchParams({ limit: '200' })
  if (kind.value) p.set('kind', kind.value)
  if (reaching.value) p.set('reaching', reaching.value)
  if (unguarded.value) p.set('unguarded', 'true')
  if (route.query.q) p.set('q', String(route.query.q))
  return p.toString()
})
const { data, error, pending } = useApiGet<AttackSurface>(() => repoPath(repo.value, `entry-points?${query.value}`))
// The overview tiles describe the whole surface, not the filtered list.
const { data: all } = useApiGet<AttackSurface>(() => repoPath(repo.value, 'entry-points?limit=200'))

const KINDS = [{ id: '', label: 'All' }, { id: 'http', label: 'HTTP routes' }, { id: 'mcp', label: 'MCP tools' }, { id: 'cli', label: 'CLI commands' }]
const TAGS = ['exec', 'sql', 'template', 'deserialize', 'filesystem', 'network', 'env', 'auth']
const WEIGHT: Record<string, number> = { exec: 3, sql: 3, cypher: 3, template: 3, unsafe: 3, syscall: 3, deserialize: 2, filesystem: 2, network: 2, env: 2 }
const sinkCls = (s: Sink) => ({ 3: 'sev--high', 2: 'sev--medium' }[WEIGHT[s.tag] ?? 1] ?? 'sev--low')
const kindLabel = (k: string) => ({ http: 'HTTP', mcp: 'MCP tool', cli: 'CLI' }[k] ?? k)
const guarded = (e: EntryPoint) => (e.guards?.length ?? 0) > 0 || e.checks_auth
const severe = computed(() => (all.value?.entry_points ?? []).filter((e) => e.reaches?.some((s) => (WEIGHT[s.tag] ?? 0) >= 3)).length)
const handlerLink = (e: EntryPoint) => `/repos/${encodeURIComponent(repo.value)}/explore?id=${encodeURIComponent(e.handler ?? '')}`
const at = (e: EntryPoint) => {
  const [file, line] = e.registered.split(':')
  return { file, line }
}
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Attack surface" title="Entry points"
      lede="Every HTTP route, MCP tool and CLI command this repository registers, the guards seen where it is registered, and the sensitive operations its handler reaches through the call graph. Unprotected ones that reach the most dangerous operations come first."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Entry points' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>

    <div v-if="all" class="grid grid--stats">
      <StatTile label="Entry points" :value="all.total" :hint="Object.entries(all.by_kind).map(([k, n]) => `${n} ${kindLabel(k)}`).join(' · ') || 'none found'" />
      <StatTile label="No guard seen" :value="all.unguarded" hint="nothing at registration, no authorization call reached" />
      <StatTile label="Reach exec, SQL or templates" :value="severe" hint="the operations attackers look for first" />
    </div>

    <section class="panel filters" aria-label="Filters">
      <div class="row">
        <button v-for="k in KINDS" :key="k.id" type="button" class="chip" :class="{ active: kind === k.id }" :aria-pressed="kind === k.id" @click="set('kind', k.id)">{{ k.label }}</button>
      </div>
      <div class="row">
        <span class="label">Reaches</span>
        <button type="button" class="chip" :class="{ active: !reaching }" :aria-pressed="!reaching" @click="set('reaching', '')">anything</button>
        <button v-for="t in TAGS" :key="t" type="button" class="chip" :class="{ active: reaching === t }" :aria-pressed="reaching === t" @click="set('reaching', t)">{{ t }}</button>
      </div>
      <div class="row">
        <label class="check"><input type="checkbox" :checked="unguarded" @change="set('unguarded', ($event.target as HTMLInputElement).checked ? 'true' : '')"> No guard seen only</label>
        <span class="spacer" />
        <input v-model="q" type="search" class="filter" placeholder="Filter by path or name…" aria-label="Filter entry points">
      </div>
    </section>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="data">
      <div v-if="!data.entry_points.length" class="empty">
        <strong>{{ all?.total ? 'Nothing matches' : 'No entry points found' }}</strong>
        {{ all?.total ? 'Loosen the filters above.' : 'Go net/http, chi, gin, echo, cobra and MCP servers, Flask, FastAPI, click and Typer, and Express, Fastify and Hono registrations are recognised.' }}
      </div>
      <ul v-else class="eps" :class="{ refreshing: pending }">
        <li v-for="e in data.entry_points" :key="e.kind + e.name + e.registered" class="panel ep">
          <div class="ep__head">
            <span class="badge">{{ kindLabel(e.kind) }}</span>
            <span class="ep__name mono">{{ e.name }}</span>
            <span v-if="!guarded(e)" class="badge badge--amber">No guard seen</span>
            <span v-if="e.checks_auth" class="badge badge--green">Checks authorization</span>
          </div>
          <p class="ep__meta small">
            <template v-if="e.handler">Handled by <NuxtLink :to="handlerLink(e)" class="mono">{{ e.handler }}</NuxtLink></template>
            <template v-else-if="e.inline_handler">Inline handler: read it at the registration</template>
            · registered at <span class="mono">{{ at(e).file }}:{{ at(e).line }}</span>
          </p>
          <div v-if="e.guards?.length" class="row">
            <span class="label">Guards</span>
            <span v-for="g in e.guards" :key="g" class="pill mono">{{ g }}</span>
          </div>
          <div v-if="e.reaches?.length" class="row">
            <span class="label">Reaches</span>
            <span v-for="s in e.reaches" :key="s.tag" class="sev" :class="sinkCls(s)" :title="`via ${s.via}, ${s.depth} call(s) from the handler`">{{ s.tag }} <span class="via">via {{ s.via }}</span></span>
          </div>
        </li>
      </ul>
      <details v-if="data.notes.length" class="notes">
        <summary>What this can and cannot see</summary>
        <ul><li v-for="n in data.notes" :key="n" class="muted small">{{ n }}</li></ul>
      </details>
    </template>
  </div>
</template>

<style scoped>
.filters { gap: 14px; }
.check { display: inline-flex; align-items: center; gap: 8px; min-height: 44px; }
.filter { max-width: 320px; }
.eps { list-style: none; margin: 0; padding: 0; display: grid; gap: 12px; }
.ep { gap: 10px; }
.ep__head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.ep__name { font-size: 15px; font-weight: 600; word-break: break-all; }
.ep__meta { color: var(--muted); }
.via { font-weight: 400; opacity: 0.85; }
.notes summary { cursor: pointer; color: var(--muted); font-size: 13px; }
.refreshing { opacity: 0.6; }
</style>
