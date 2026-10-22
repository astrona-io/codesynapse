<script setup lang="ts">
import type { ArchDetail } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const id = computed(() => route.params.id as string)
useHead({ title: () => `${id.value} · Architecture · ${repo.value}` })
const { data: rec, error, refresh } = useApiGet<ArchDetail>(() => repoPath(repo.value, `architecture/record?id=${encodeURIComponent(id.value)}`))
const link = reactive({ kind: 'implements', target: '', note: '' })
const linkError = ref('')
async function addLink() {
  linkError.value = ''
  try {
    await apiPost(repoPath(repo.value, 'architecture/links'), { kind: link.kind, record: id.value, target: link.target, note: link.note })
    Object.assign(link, { target: '', note: '' })
    await refresh()
  } catch (e) {
    linkError.value = apiErrorMessage(e)
  }
}
async function removeLink(linkId: string) {
  linkError.value = ''
  try {
    await apiDelete(repoPath(repo.value, `architecture/links/${linkId}`))
    await refresh()
  } catch (e) {
    linkError.value = apiErrorMessage(e)
  }
}
const targetHint = computed(() => ({ implements: 'internal/store, store.Reader or a file', relates: 'ADR-0004', 'applies-to': 'api' }[link.kind] ?? ''))

const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const detail = (rid: string) => `${base.value}/architecture/${encodeURIComponent(rid)}`
const explore = (nid: string) => ({ path: `${base.value}/explore`, query: { id: nid } })
const isCode = (nid: string) => /\|(Function|Method|Type|InterfaceMethod)\|/.test(nid)
const statusClass = (s: string) => ({ accepted: 'badge--green', active: 'badge--green', current: 'badge--green', implemented: 'badge--green', proposed: 'badge--amber', draft: 'badge--amber', 'in-review': 'badge--amber', unknown: 'badge--pink', rejected: 'badge--pink' }[s] ?? '')
const when = (iso?: string) => (iso ? new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', year: 'numeric' }) : '')
const explain = computed(() => {
  const r = rec.value
  if (!r) return ''
  if (r.binding) return 'This is a rule in force: new code is expected to follow it.'
  if (r.superseded_by?.length) return `Replaced by ${r.superseded_by.join(', ')}. It is history, not a rule.`
  if (['proposed', 'draft', 'in-review'].includes(r.status)) return 'Not decided yet. Treat it as a proposal, not a requirement.'
  if (r.status === 'rejected') return 'Rejected. Kept to explain why this was not done.'
  if (r.status === 'unknown') return 'The status is missing or not recognised, so nothing treats this record as a rule.'
  if (['overview', 'runbook', 'diagram', 'rfc'].includes(r.type)) return 'This informs; it is not a rule on its own.'
  return 'Not a rule in force.'
})
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Architecture" :title="rec ? `${rec.id}: ${rec.title}` : id"
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'Architecture', to: `${base}/architecture` }, { label: id }]"
    />

    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rec">
      <section class="panel">
        <div class="row">
          <span class="badge" :class="statusClass(rec.status)">{{ rec.status }}</span>
          <span class="badge">{{ rec.type }}</span>
          <strong v-if="rec.binding">Rule in force</strong>
        </div>
        <p>{{ explain }}</p>
        <p v-if="rec.summary" class="muted">{{ rec.summary }}</p>
        <ol class="lifecycle" aria-label="Lifecycle">
          <li v-for="s in rec.lifecycle" :key="s" :class="{ current: s === rec.status }" :aria-current="s === rec.status ? 'step' : undefined">{{ s }}<span v-if="s === rec.status" class="sr"> (current)</span></li>
        </ol>
        <dl class="meta">
          <div v-if="rec.owner"><dt>Owner</dt><dd>{{ rec.owner }}</dd></div>
          <div v-if="rec.date"><dt>Date</dt><dd>{{ rec.date }}</dd></div>
          <div v-if="rec.topic"><dt>Topic</dt><dd>{{ rec.topic }}</dd></div>
          <div v-if="rec.components?.length"><dt>Applies to</dt><dd><NuxtLink v-for="c in rec.components" :key="c" :to="{ path: `${base}/architecture`, query: { component: c } }" class="chip">{{ c }}</NuxtLink></dd></div>
          <div v-if="rec.labels?.length"><dt>Labels</dt><dd><span v-for="l in rec.labels" :key="l" class="tag">{{ l }}</span></dd></div>
          <div><dt>Source</dt><dd class="mono">{{ rec.source.path }} <span class="muted">({{ rec.location }})</span></dd></div>
          <div><dt>Revision</dt><dd class="mono">{{ rec.source.head?.slice(0, 8) || 'unknown' }} <span class="muted">indexed {{ when(rec.source.indexed_at) }} · content {{ rec.fingerprint }}</span></dd></div>
          <div v-if="rec.raw_status && rec.raw_status.toLowerCase() !== rec.status"><dt>Written as</dt><dd>{{ rec.raw_status }}</dd></div>
        </dl>
      </section>

      <section v-if="rec.problems.length" class="panel edge--amber">
        <p class="eyebrow">Needs a look</p><h2>Problems with this record</h2>
        <ul class="plain"><li v-for="p in rec.problems" :key="p.message"><span class="badge badge--amber">{{ p.kind.replace(/-/g, ' ') }}</span> {{ p.message }}</li></ul>
        <p class="muted small">Reported, not resolved: a person decides which record is right.</p>
      </section>

      <div class="grid cols-2">
        <section class="panel">
          <p class="eyebrow">History of the decision</p><h2>Supersession</h2>
          <ul v-if="rec.chain.length" class="plain">
            <li v-for="c in rec.chain" :key="c.id" :style="{ paddingLeft: `${(c.depth - 1) * 16}px` }">
              {{ c.relation === 'supersedes' ? 'Replaces' : 'Replaced by' }}
              <NuxtLink :to="detail(c.id)" class="mono">{{ c.id }}</NuxtLink> {{ c.title }} <span class="badge" :class="statusClass(c.status)">{{ c.status }}</span>
            </li>
          </ul>
          <p v-else class="muted">Nothing replaces it and it replaces nothing.</p>
        </section>
        <section class="panel">
          <p class="eyebrow">Where it is read</p><h2>Sections</h2>
          <ul v-if="rec.outline.length" class="plain"><li v-for="o in rec.outline" :key="o.name + o.lines"><span class="mono">{{ o.lines }}</span> {{ o.name }}</li></ul>
          <p v-else class="muted">No headings found.</p>
          <p class="muted small">The text stays in the file: read those lines in <span class="mono">{{ rec.source.path }}</span>.</p>
        </section>
      </div>

      <section class="panel">
        <p class="eyebrow">Implementation</p><h2>Code that carries it out <span class="muted">· {{ rec.implemented_by.length }}</span></h2>
        <div v-if="rec.implemented_by.length" class="table-wrap"><table><tbody>
          <tr v-for="c in rec.implemented_by" :key="c.id">
            <td><NuxtLink v-if="isCode(c.id)" :to="explore(c.id)" class="mono">{{ c.name }}</NuxtLink><span v-else class="mono">{{ c.name }}</span></td>
            <td><span class="badge">{{ c.kind || 'code' }}</span></td>
            <td class="mono muted">{{ c.location }}</td>
            <td class="muted small">
              <template v-if="c.source === 'link'">
                <span class="badge badge--violet">linked by {{ c.by || 'a user' }}</span>
                <span v-if="c.stale" class="badge badge--amber" :title="c.problem">needs re-linking: {{ c.problem }}</span>
                <button class="chip" :aria-label="`Remove the link to ${c.declared}`" @click="removeLink(c.link_id!)">Remove</button>
              </template>
              <template v-else>declared in the file<template v-if="c.declared"> as <span class="mono">{{ c.declared }}</span></template></template>
            </td>
          </tr>
        </tbody></table></div>
        <p v-else class="muted">No code is linked. Add <span class="mono">implements: [path or symbol]</span> to the record's front matter, or link it below.</p>
        <details class="link-form">
          <summary>Add a link</summary>
          <form class="stack" @submit.prevent="addLink">
            <label>Kind
              <select v-model="link.kind"><option value="implements">Code carries out this record</option><option value="relates">Related record</option><option value="applies-to">Applies to a component</option></select>
            </label>
            <label>Target <input v-model="link.target" required :placeholder="targetHint"></label>
            <label>Note <input v-model="link.note" maxlength="1000"></label>
            <p v-if="linkError" class="notice notice--danger" role="alert">{{ linkError }}</p>
            <button type="submit" class="btn" :disabled="!link.target.trim()">Add link</button>
          </form>
          <p class="muted small">A link is recorded with who added it and is shown beside what the file declares. It does not change the record or the code, and it is flagged if the code is renamed.</p>
        </details>
        <template v-if="rec.mentions.length">
          <h3>Code its text mentions</h3>
          <p class="muted small">Weaker than a link: the name appears in the text, which does not mean the code follows the decision.</p>
          <ul class="plain"><li v-for="m in rec.mentions" :key="m.id"><NuxtLink v-if="isCode(m.id)" :to="explore(m.id)" class="mono">{{ m.name }}</NuxtLink><span v-else class="mono">{{ m.name }}</span> <span class="mono muted">{{ m.location }}</span></li></ul>
        </template>
        <ul v-if="rec.unresolved?.length" class="plain"><li v-for="u in rec.unresolved" :key="u" class="muted small">{{ u }}</li></ul>
      </section>

      <div class="grid cols-2">
        <section v-if="rec.related.length" class="panel">
          <p class="eyebrow">Related</p><h2>Other records</h2>
          <ul class="plain"><li v-for="r in rec.related" :key="r.id"><NuxtLink :to="detail(r.id)" class="mono">{{ r.id }}</NuxtLink> {{ r.title }} <span class="badge" :class="statusClass(r.status)">{{ r.status }}</span></li></ul>
        </section>
        <section class="panel">
          <p class="eyebrow">Revisions</p><h2>Changes to the file</h2>
          <ul v-if="rec.revisions.length" class="plain"><li v-for="r in rec.revisions" :key="r.commit"><span class="mono">{{ r.commit.slice(0, 8) }}</span> {{ r.subject }} <span class="muted small">{{ when(r.time) }}</span></li></ul>
          <p v-else class="muted">No commit history was indexed. Sync with history enabled to see revisions.</p>
        </section>
      </div>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.plain { list-style: none; padding: 0; margin: 0; display: grid; gap: 8px; }
.lifecycle { display: flex; gap: 8px; flex-wrap: wrap; list-style: none; padding: 0; margin: 0 0 12px; }
.lifecycle li { padding: 2px 10px; border: 1px solid var(--border); border-radius: 999px; font-size: 12.5px; }
.lifecycle li.current { border-color: var(--accent); font-weight: 600; }
.lifecycle li.current::before { content: '● '; }
.meta { display: grid; gap: 6px; margin: 0; }
.meta > div { display: grid; grid-template-columns: 110px 1fr; gap: 12px; }
.meta dt { color: var(--muted, inherit); }
.meta dd { margin: 0; }
.sr { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); }
.small { font-size: 12.5px; }
.link-form { margin-top: 12px; }
.link-form form { margin-top: 10px; max-width: 560px; }
.link-form label { display: grid; gap: 4px; }
</style>
