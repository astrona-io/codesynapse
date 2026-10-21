<script setup lang="ts">
import type { Review, ReviewComment, ReviewCounts, ReviewSummary, RuleGuide } from '~/utils/types'

const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const selectedId = computed(() => (route.query.id as string | undefined) ?? '')
const { data: list, refresh: refreshList } = useApiGet<{ reviews: ReviewSummary[] }>(() => repoPath(repo.value, 'reviews'))
const { data: catalog } = useApiGet<{ rules: RuleGuide[] }>('rules')
const ruleTitle = (id?: string) => catalog.value?.rules.find((r) => r.rule === id)?.title

const review = ref<Review | null>(null)
const counts = ref<ReviewCounts | null>(null)
const loadError = ref('')
watch(selectedId, async (id) => {
  review.value = null
  if (!id) return
  try {
    const res = await $fetch<{ review: Review; counts: ReviewCounts }>(apiPath(`reviews/${id}`))
    review.value = res.review
    counts.value = res.counts
  } catch (e) {
    loadError.value = apiErrorMessage(e)
  }
}, { immediate: true })
watch(list, (l) => {
  if (!selectedId.value && l?.reviews.length) router.replace({ query: { id: l.reviews[0]!.id } })
})

const statusFilter = ref<'open' | 'accepted' | 'deferred' | 'all'>('open')
const SEV = ['critical', 'high', 'medium', 'low', 'info']
const byFile = computed(() => {
  const groups = new Map<string, ReviewComment[]>()
  for (const c of review.value?.comments ?? []) {
    if (statusFilter.value !== 'all' && c.status !== statusFilter.value) continue
    if (!groups.has(c.file)) groups.set(c.file, [])
    groups.get(c.file)!.push(c)
  }
  for (const cs of groups.values()) cs.sort((a, b) => (a.line ?? 0) - (b.line ?? 0))
  return [...groups.entries()].sort((a, b) => worst(b[1]) - worst(a[1]) || a[0].localeCompare(b[0]))
})
function worst(cs: ReviewComment[]) {
  return Math.max(...cs.map((c) => SEV.length - SEV.indexOf(c.severity)))
}

const busy = ref('')
async function setStatus(c: ReviewComment, status: ReviewComment['status']) {
  busy.value = c.id
  try {
    const res = await $fetch<{ review: Review; counts: ReviewCounts }>(apiPath(`reviews/${review.value!.id}/comments/${c.id}`), { method: 'PATCH', body: { status, note: '' } })
    review.value = res.review
    counts.value = res.counts
    refreshList()
  } finally {
    busy.value = ''
  }
}
const deleting = ref(false)
const confirmDelete = ref(false)
async function removeReview() {
  deleting.value = true
  try {
    await $fetch(apiPath(`reviews/${review.value!.id}`), { method: 'DELETE' })
    confirmDelete.value = false
    await router.replace({ query: {} })
    await refreshList()
  } finally {
    deleting.value = false
  }
}

const STATUS: Record<string, { label: string; cls: string }> = {
  accepted: { label: 'Accepted', cls: 'badge--orange' },
  rejected: { label: 'Rejected', cls: '' },
  wontfix: { label: 'Rejected', cls: '' },
  deferred: { label: 'Deferred', cls: 'badge--amber' },
  resolved: { label: 'Fixed', cls: 'badge--green' },
}
const verdictText: Record<string, { label: string; cls: string }> = {
  approve: { label: 'Approved', cls: 'badge--green' },
  comment: { label: 'Comments', cls: 'badge--cyan' },
  request_changes: { label: 'Changes requested', cls: 'badge--pink' },
}
const lines = (c: ReviewComment) => (c.line ? (c.end_line && c.end_line > c.line ? `L${c.line}–${c.end_line}` : `L${c.line}`) : '')
const explore = (id: string) => ({ path: `${base.value}/explore`, query: { id } })
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Code review" title="Reviews"
      lede="Reviews submitted by AI agents (or people) through MCP: a verdict, a summary and comments on exact lines with suggested fixes. Resolve comments here or let the agent mark them fixed."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'Reviews' }]"
    />

    <div v-if="!list?.reviews.length" class="panel how">
      <p class="eyebrow eyebrow--green">No reviews yet</p>
      <h2>Ask your AI agent for a review</h2>
      <p class="muted">With CodeSynapse connected over MCP (run <code>just mcp</code> once), ask your agent:</p>
      <pre class="cmd">Review my current changes in {{ repo }} with CodeSynapse and submit the review.</pre>
      <p class="muted">Clients that support MCP prompts also offer <strong>code-review</strong> as a command. The agent analyses the change with the graph (risk, tests, security rules) and calls <code>submit_review</code>; the review appears here.</p>
    </div>

    <div v-else class="layout">
      <aside class="panel list">
        <p class="eyebrow">Reviews · {{ list.reviews.length }}</p>
        <button v-for="r in list.reviews" :key="r.id" class="item" :class="{ 'is-active': r.id === selectedId }" @click="router.replace({ query: { id: r.id } })">
          <span class="item-title">{{ r.title }} <span v-if="r.kind === 'report'" class="badge badge--violet">report</span></span>
          <span class="row tight">
            <span class="badge" :class="verdictText[r.verdict]?.cls">{{ verdictText[r.verdict]?.label ?? r.verdict }}</span>
            <span class="muted small">{{ r.counts.open }} to decide · {{ r.counts.accepted ?? 0 }} accepted · {{ r.counts.resolved }} fixed</span>
          </span>
          <span class="dim small">{{ r.author }} · {{ relTime(r.created_at) }}</span>
        </button>
      </aside>

      <section class="detail">
        <p v-if="loadError" class="panel error">{{ loadError }}</p>
        <template v-if="review && counts">
          <div class="panel panel--glass">
            <div class="panel__header">
              <div>
                <p class="eyebrow">{{ review.author }} via {{ review.source }} · {{ relTime(review.created_at) }}<span v-if="review.commit"> · commit <span class="mono">{{ review.commit.slice(0, 10) }}</span></span></p>
                <h2 class="title">{{ review.title }}</h2>
              </div>
              <span class="badge big" :class="verdictText[review.verdict]?.cls">{{ verdictText[review.verdict]?.label ?? review.verdict }}</span>
            </div>
            <!-- eslint-disable-next-line vue/no-v-html -- renderMarkdown escapes all HTML first -->
            <div v-if="review.summary && review.kind === 'report'" class="report" v-html="renderMarkdown(review.summary)" />
            <p v-else-if="review.summary" class="summary">{{ review.summary }}</p>
            <div class="row">
              <span class="pill"><span class="dot" :class="counts.open ? 'dot--warn' : 'dot--ok'" />{{ counts.open ? `${plural(counts.open, 'recommendation')} to decide` : 'Everything decided' }}<template v-if="counts.accepted"> · {{ counts.accepted }} accepted to fix</template></span>
              <SevBadge v-for="s in SEV.filter((x) => counts!.by_severity[x])" :key="s" :severity="s" />
              <span class="spacer" />
              <button class="chip" :class="{ active: statusFilter === 'open' }" @click="statusFilter = 'open'">To decide · {{ counts.open }}</button>
              <button class="chip" :class="{ active: statusFilter === 'accepted' }" @click="statusFilter = 'accepted'">Accepted · {{ counts.accepted ?? 0 }}</button>
              <button class="chip" :class="{ active: statusFilter === 'deferred' }" @click="statusFilter = 'deferred'">Deferred · {{ counts.deferred ?? 0 }}</button>
              <button class="chip" :class="{ active: statusFilter === 'all' }" @click="statusFilter = 'all'">All · {{ counts.total }}</button>
              <button class="secondary tiny" @click="confirmDelete = true">Delete review</button>
            </div>
          </div>

          <div v-if="!byFile.length" class="empty"><strong>Nothing here</strong>No recommendations in this view. Switch to “All” to see every one.</div>
          <section v-for="[file, cs] in byFile" :key="file" class="panel file">
            <p class="mono file-name">{{ file }}</p>
            <article v-for="c in cs" :key="c.id" class="comment" :class="[`sev-${c.severity}`, `status-${c.status}`, { done: c.status === 'resolved' || c.status === 'rejected' || c.status === 'wontfix' }]">
              <header class="row tight">
                <SevBadge :severity="c.severity" />
                <span class="tag">{{ c.category }}</span>
                <span class="mono dim">{{ lines(c) }}</span>
                <NuxtLink v-if="c.symbol_id" :to="explore(c.symbol_id)" class="mono small">{{ c.symbol }}</NuxtLink>
                <NuxtLink v-if="c.rule" :to="`${base}/security`" class="tag sec">{{ ruleTitle(c.rule) ?? c.rule }}</NuxtLink>
                <span class="spacer" />
                <span v-if="c.status !== 'open'" class="badge" :class="STATUS[c.status]?.cls">{{ STATUS[c.status]?.label ?? c.status }}<template v-if="c.status_by"> by {{ c.status_by }}</template></span>
              </header>
              <p class="message">{{ c.message }}</p>
              <div v-if="c.suggestion">
                <p class="label good">Suggested fix</p>
                <pre class="code">{{ c.suggestion }}</pre>
              </div>
              <p v-if="c.status_note" class="muted small">Note: {{ c.status_note }}</p>
              <footer class="row tight decide">
                <template v-if="c.status === 'open' || c.status === 'deferred'">
                  <button class="tiny" :disabled="busy === c.id" title="Agree — this should be fixed" @click="setStatus(c, 'accepted')">Accept</button>
                  <button class="secondary tiny" :disabled="busy === c.id" title="Disagree or false positive" @click="setStatus(c, 'rejected')">Reject</button>
                  <button v-if="c.status === 'open'" class="secondary tiny" :disabled="busy === c.id" title="Decide later" @click="setStatus(c, 'deferred')">Defer</button>
                  <button class="secondary tiny" :disabled="busy === c.id" title="Already fixed" @click="setStatus(c, 'resolved')">Mark fixed</button>
                </template>
                <template v-else-if="c.status === 'accepted'">
                  <button class="tiny" :disabled="busy === c.id" @click="setStatus(c, 'resolved')">Mark fixed</button>
                  <button class="secondary tiny" :disabled="busy === c.id" @click="setStatus(c, 'open')">Undo</button>
                </template>
                <button v-else class="secondary tiny" :disabled="busy === c.id" @click="setStatus(c, 'open')">Reopen</button>
              </footer>
            </article>
          </section>
          <ConfirmDialog :open="confirmDelete" title="Delete this review?" confirm-label="Delete review" danger :busy="deleting" @confirm="removeReview" @cancel="confirmDelete = false">
            <p>“{{ review.title }}” and its {{ plural(counts.total, 'comment') }} will be removed. Code is not affected.</p>
          </ConfirmDialog>
        </template>
      </section>
    </div>
  </div>
</template>

<style scoped>
.how { gap: 10px; }
.cmd { margin: 0; padding: 10px 12px; background: var(--bg); border: 1px solid var(--line); border-radius: var(--radius-xs); font: 13px var(--font-mono); color: var(--orange-ink); white-space: pre-wrap; }
.layout { display: grid; grid-template-columns: 300px minmax(0, 1fr); gap: 20px; align-items: start; }
.list { position: sticky; top: 24px; max-height: calc(100vh - 48px); overflow-y: auto; gap: 4px; }
.item { all: unset; box-sizing: border-box; cursor: pointer; display: grid; gap: 4px; padding: 10px 12px; border-radius: var(--radius-xs); border-left: 2px solid transparent; }
.item:hover { background: rgba(236, 231, 245, 0.04); }
.item.is-active { background: var(--tint-orange); border-left-color: var(--orange); }
.item:focus-visible { box-shadow: var(--focus); }
.item-title { font-weight: 600; color: var(--ink); }
.detail { display: grid; gap: 16px; min-width: 0; }
.title { font-size: 20px; }
.summary { white-space: pre-wrap; line-height: 1.6; }
.report :deep(h3), .report :deep(h4) { margin: 14px 0 6px; font-size: 16px; }
.report :deep(p) { margin: 0 0 8px; line-height: 1.65; }
.report :deep(ul), .report :deep(ol) { margin: 0 0 8px; padding-left: 20px; display: grid; gap: 4px; }
.report :deep(code) { font-family: var(--font-mono); font-size: 12.5px; background: var(--bg); padding: 1px 5px; border-radius: 4px; }
.report :deep(pre) { background: var(--bg); border: 1px solid var(--line); border-radius: var(--radius-xs); padding: 10px 12px; overflow-x: auto; }
.big { font-size: 11px; padding: 5px 12px; }
.tight { gap: 6px; }
.spacer { flex: 1; }
.small { font-size: 12.5px; }
.file { gap: 10px; }
.file-name { color: var(--cyan); font-size: 13.5px; }
.comment { display: grid; gap: 8px; padding: 12px 14px; border-radius: var(--radius-sm); background: var(--panel-faint); border: 1px solid var(--line); border-left: 3px solid var(--line-strong); }
.comment.sev-critical { border-left-color: var(--pink); }
.comment.sev-high { border-left-color: var(--orange); }
.comment.sev-medium { border-left-color: var(--amber); }
.comment.sev-low { border-left-color: var(--cyan); }
.comment.done { opacity: 0.6; }
.comment.status-accepted { opacity: 1; border-color: rgba(255, 122, 24, 0.35); }
.message { white-space: pre-wrap; }
.code { margin: 4px 0 0; padding: 10px 12px; border-radius: var(--radius-xs); font: 12.5px/1.5 var(--font-mono); white-space: pre-wrap; word-break: break-word; background: var(--bg); border: 1px solid var(--line); border-left: 3px solid var(--green); }
.label.good { color: var(--green); }
@media (max-width: 1000px) { .layout { grid-template-columns: 1fr; } .list { position: static; max-height: none; } }
</style>
