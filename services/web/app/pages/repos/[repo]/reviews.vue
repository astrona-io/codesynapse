<script setup lang="ts">
import type { Review, ReviewComment, ReviewCounts, ReviewSummary, RuleGuide } from '~/utils/types'

type Status = ReviewComment['status']
type View = 'open' | 'accepted' | 'deferred' | 'done' | 'all'

const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const selectedId = computed(() => (route.query.id as string | undefined) ?? '')
const { data: list, refresh: refreshList } = useApiGet<{ reviews: ReviewSummary[] }>(() => repoPath(repo.value, 'reviews'))
const { data: catalog } = useApiGet<{ rules: RuleGuide[] }>('rules')
const ruleTitle = (id?: string) => catalog.value?.rules.find((r) => r.rule === id)?.title
const { data: settings } = useApiGet<{ push_enabled: boolean; push_allowed: boolean; git_token_configured: boolean }>('settings')
const { data: repos } = useApiGet<{ repos: { name: string; branch: string; remote: string; managed: boolean }[] }>('repos')
const repoInfo = computed(() => repos.value?.repos.find((r) => r.name === repo.value))
// Why fix branches are unavailable for this repository, or '' when they work.
const fixBlocked = computed(() => {
  if (!settings.value) return 'Loading…'
  if (!settings.value.push_allowed) return 'Fix branches are off. Set CODESYNAPSE_ALLOW_PUSH=true in .env and restart the API.'
  if (!settings.value.push_enabled) return 'Configure an AI model first (AI models page).'
  if (!settings.value.git_token_configured) return 'Add a git token with write access: just git-token'
  if (repoInfo.value && !repoInfo.value.managed) return 'This repository is read from ./sources. Use “Switch to clone” on its summary page to enable fix branches.'
  return ''
})

const review = ref<Review | null>(null)
const counts = ref<ReviewCounts | null>(null)
// Code around each comment, read from the checkout on demand (never stored).
const code = ref<Record<string, CommentCode>>({})
const loadError = ref('')
const view = ref<View>('open')
const summaryOpen = ref(false)
watch(selectedId, async (id) => {
  review.value = null
  code.value = {}
  loadError.value = ''
  summaryOpen.value = false
  if (!id) return
  $fetch<{ code: Record<string, CommentCode> }>(apiPath(`reviews/${id}/code`))
    .then((res) => { if (selectedId.value === id) code.value = res.code })
    .catch(() => {}) // the cards work without code
  try {
    const res = await $fetch<{ review: Review; counts: ReviewCounts }>(apiPath(`reviews/${id}`))
    review.value = res.review
    counts.value = res.counts
    view.value = res.counts.open ? 'open' : res.counts.accepted ? 'accepted' : 'all'
  } catch (e) {
    loadError.value = apiErrorMessage(e)
  }
}, { immediate: true })
watch(list, (l) => {
  if (!selectedId.value && l?.reviews.length) router.replace({ query: { id: l.reviews[0]!.id } })
})
const isReport = computed(() => review.value?.kind === 'report')

// --- decisions -------------------------------------------------------------

const CLOSED: Status[] = ['resolved', 'rejected', 'wontfix']
const inView = (c: ReviewComment, v: View) =>
  v === 'all' || (v === 'done' ? CLOSED.includes(c.status) : c.status === v)

const done = computed(() => (counts.value ? counts.value.resolved + (counts.value.rejected ?? 0) : 0))
const VIEWS = computed(() => [
  { id: 'open' as View, label: 'To decide', n: counts.value?.open ?? 0 },
  { id: 'accepted' as View, label: 'To fix', n: counts.value?.accepted ?? 0 },
  { id: 'deferred' as View, label: 'Later', n: counts.value?.deferred ?? 0 },
  { id: 'done' as View, label: 'Done', n: done.value },
  { id: 'all' as View, label: 'All', n: counts.value?.total ?? 0 },
])

const SEV = ['critical', 'high', 'medium', 'low', 'info']
const rank = (c: ReviewComment) => SEV.indexOf(c.severity)
const byFile = computed(() => {
  const groups = new Map<string, ReviewComment[]>()
  for (const c of review.value?.comments ?? []) {
    if (!inView(c, view.value)) continue
    if (!groups.has(c.file)) groups.set(c.file, [])
    groups.get(c.file)!.push(c)
  }
  for (const cs of groups.values()) cs.sort((a, b) => (a.line ?? 0) - (b.line ?? 0))
  return [...groups.entries()].sort((a, b) => Math.min(...a[1].map(rank)) - Math.min(...b[1].map(rank)) || a[0].localeCompare(b[0]))
})

// What each decision means, shown in the guide and on the buttons.
const DECISIONS = [
  { status: 'accepted' as Status, label: 'Accept', short: 'Yes, fix it', cls: 'accept', icon: '✓',
    what: 'You agree this should be fixed.',
    then: 'It moves to To fix. From there CodeSynapse can write the fix on a new branch and open a draft pull request, so your CI runs on it. Agents connected over MCP can also pick it up. It stays on the list until it is marked fixed.' },
  { status: 'rejected' as Status, label: 'Reject', short: 'Not valid', cls: 'reject', icon: '✕',
    what: 'It is wrong, a false positive or not worth doing.',
    then: 'It is closed and kept under Done for the record. Add a reason so the next reviewer, person or AI, knows why.' },
  { status: 'deferred' as Status, label: 'Defer', short: 'Decide later', cls: 'defer', icon: '⏸',
    what: 'Fair point, but not now.',
    then: 'It is parked under Later and does not count as decided. Come back to it any time.' },
  { status: 'resolved' as Status, label: 'Mark fixed', short: 'Already done', cls: 'fixed', icon: '★',
    what: 'The code has been fixed.',
    then: 'It is closed as fixed. Use it after fixing an accepted item, or when it was already handled.' },
]
const STATUS_TEXT: Record<string, { label: string; cls: string; next: string }> = {
  accepted: { label: 'Accepted', cls: 'badge--orange', next: 'On the To fix list' },
  rejected: { label: 'Rejected', cls: '', next: 'Closed' },
  wontfix: { label: 'Rejected', cls: '', next: 'Closed' },
  deferred: { label: 'Deferred', cls: 'badge--amber', next: 'Parked for later' },
  resolved: { label: 'Fixed', cls: 'badge--green', next: 'Closed as fixed' },
}

const busy = ref('')
const actionError = ref('')
const rejecting = ref('') // comment id with the reason box open
const reason = ref('')
const toast = ref<{ text: string; id: string; prev: Status } | null>(null)
let toastTimer: ReturnType<typeof setTimeout> | undefined

async function decide(c: ReviewComment, status: Status, note = '', announce = true) {
  busy.value = c.id
  actionError.value = ''
  const prev = c.status
  try {
    const res = await $fetch<{ review: Review; counts: ReviewCounts }>(apiPath(`reviews/${review.value!.id}/comments/${c.id}`), { method: 'PATCH', body: { status, note } })
    review.value = res.review
    counts.value = res.counts
    rejecting.value = ''
    reason.value = ''
    refreshList()
    clearTimeout(toastTimer)
    toast.value = null
    if (announce) {
      const where = { accepted: 'moved to To fix', rejected: 'moved to Done', resolved: 'moved to Done', deferred: 'moved to Later', open: 'back in To decide' }[status as string] ?? ''
      toast.value = { text: `${DECISIONS.find((d) => d.status === status)?.label ?? 'Reopened'} · ${where}`, id: c.id, prev }
      toastTimer = setTimeout(() => (toast.value = null), 6000)
    }
  } catch (e) {
    actionError.value = apiErrorMessage(e)
  } finally {
    busy.value = ''
  }
}
function undo() {
  const t = toast.value
  const c = review.value?.comments.find((x) => x.id === t?.id)
  if (t && c) decide(c, t.prev, '', false)
}
// --- fix branches --------------------------------------------------------------

const fixTarget = ref<ReviewComment[] | null>(null) // comments in the confirm dialog
const fixStarting = ref(false)
const fixError = ref('')
// Matches fixes.staleAfter: an unfinished fix older than this was abandoned.
const STALE_MS = 2 * 60 * 60 * 1000
const unfinished = (c: ReviewComment) => c.fix?.state === 'queued' || c.fix?.state === 'running'
const stale = (c: ReviewComment) => unfinished(c) && Date.now() - Date.parse(c.fix!.at) > STALE_MS
const fixable = (c: ReviewComment) => c.status === 'accepted' && (!unfinished(c) || stale(c)) && c.fix?.state !== 'pushed' && !/(^|\/)\.(github|gitlab-ci|circleci)|jenkinsfile|^\.git\//i.test(c.file)
const allFixable = computed(() => (review.value?.comments ?? []).filter(fixable))
const anyRunning = computed(() => (review.value?.comments ?? []).some((c) => unfinished(c) && !stale(c)))

async function startFix() {
  if (!fixTarget.value || !review.value) return
  fixStarting.value = true
  fixError.value = ''
  try {
    await $fetch(apiPath(`reviews/${review.value.id}/fix`), { method: 'POST', body: { comment_ids: fixTarget.value.map((c) => c.id) } })
    for (const c of fixTarget.value) {
      const live = review.value.comments.find((x) => x.id === c.id)
      if (live) live.fix = { state: 'queued', at: new Date().toISOString() }
    }
    fixTarget.value = null
  } catch (e) {
    fixError.value = apiErrorMessage(e)
  } finally {
    fixStarting.value = false
  }
}
// Poll the review while a fix job is writing, so the branch and PR appear.
let poll: ReturnType<typeof setInterval> | undefined
watch(anyRunning, (on) => {
  clearInterval(poll)
  if (!on) return
  poll = setInterval(async () => {
    if (!review.value) return
    try {
      const res = await $fetch<{ review: Review; counts: ReviewCounts }>(apiPath(`reviews/${review.value.id}`))
      review.value = res.review
      counts.value = res.counts
    } catch { /* keep polling */ }
  }, 4000)
})
onBeforeUnmount(() => clearInterval(poll))

function startReject(c: ReviewComment) {
  rejecting.value = c.id
  reason.value = ''
}

// The guide is open until the reader closes it once.
const GUIDE_KEY = 'cs.reviews.guide'
const guideOpen = ref(true)
onMounted(() => {
  try { guideOpen.value = localStorage.getItem(GUIDE_KEY) !== 'closed' } catch { /* storage unavailable */ }
})
function toggleGuide() {
  guideOpen.value = !guideOpen.value
  try { localStorage.setItem(GUIDE_KEY, guideOpen.value ? 'open' : 'closed') } catch { /* storage unavailable */ }
}

// --- presentation ------------------------------------------------------------

const longSummary = computed(() => (review.value?.summary.length ?? 0) > 420)

// The first sentence becomes the headline; the rest is the explanation.
function headline(msg: string): [string, string] {
  const m = msg.match(/^(.{20,220}?[.!?:])\s+(.*)$/s)
  return m ? [m[1]!, m[2]!] : [msg, '']
}
const md = (s: string) => renderMarkdown(s)
const loc = (c: ReviewComment) => (c.line ? `${c.file}:${c.line}${c.end_line && c.end_line > c.line ? `–${c.end_line}` : ''}` : c.file)
const explore = (id: string) => ({ path: `${base.value}/explore`, query: { id } })
const segments = (k: ReviewCounts) => [
  { n: k.resolved, cls: 'seg--fixed', label: 'fixed' },
  { n: k.accepted ?? 0, cls: 'seg--accept', label: 'to fix' },
  { n: k.rejected ?? 0, cls: 'seg--reject', label: 'rejected' },
  { n: k.deferred ?? 0, cls: 'seg--defer', label: 'later' },
].filter((s) => s.n > 0)
const decided = (k: ReviewCounts) => k.total - k.open - (k.deferred ?? 0)
const verdictText: Record<string, { label: string; cls: string }> = {
  approve: { label: 'Approved', cls: 'badge--green' },
  comment: { label: 'Comments', cls: 'badge--cyan' },
  request_changes: { label: 'Changes requested', cls: 'badge--pink' },
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
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Code review" title="Reviews"
      lede="An AI or a colleague left recommendations on your code. Go through them and decide what happens to each one. Nothing here changes your code."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'Reviews' }]"
    />

    <div v-if="!list?.reviews.length" class="panel how">
      <p class="eyebrow eyebrow--green">No reviews yet</p>
      <h2>Ask for a review</h2>
      <p class="muted">Run one from the <NuxtLink :to="`${base}/ai`">AI assistant</NuxtLink>, turn on automatic reviews in the sync settings, or, with CodeSynapse connected over MCP (<code>just mcp</code>), ask your agent:</p>
      <pre class="cmd">Review my current changes in {{ repo }} with CodeSynapse and submit the review.</pre>
    </div>

    <div v-else class="layout">
      <aside class="panel list" aria-label="Reviews">
        <p class="eyebrow">Reviews · {{ list.reviews.length }}</p>
        <button v-for="r in list.reviews" :key="r.id" class="item" :class="{ 'is-active': r.id === selectedId }" @click="router.replace({ query: { id: r.id } })">
          <span class="item-title">{{ r.title }}</span>
          <template v-if="r.kind === 'report'">
            <span class="row tight"><span class="badge badge--violet">report</span><span class="dim small">nothing to decide</span></span>
          </template>
          <template v-else>
            <span class="bar bar--mini" :title="`${decided(r.counts)} of ${r.counts.total} decided`">
              <span v-for="s in segments(r.counts)" :key="s.cls" :class="['seg', s.cls]" :style="{ flexGrow: s.n }" />
              <span class="seg seg--open" :style="{ flexGrow: r.counts.open }" />
            </span>
            <span class="small" :class="r.counts.open ? 'warn' : 'muted'">{{ r.counts.open ? `${r.counts.open} to decide` : r.counts.accepted ? `${r.counts.accepted} to fix` : 'all decided' }}</span>
          </template>
          <span class="dim small">{{ r.author }} · {{ relTime(r.created_at) }}</span>
        </button>
      </aside>

      <section class="detail">
        <p v-if="loadError" class="notice notice--danger" role="alert">{{ loadError }}</p>
        <template v-if="review && counts">
          <!-- Review header -->
          <div class="panel head">
            <div class="panel__header">
              <div>
                <p class="eyebrow">{{ review.author }} · {{ relTime(review.created_at) }}<span v-if="review.commit"> · commit <span class="mono">{{ review.commit.slice(0, 10) }}</span></span></p>
                <h2 class="title">{{ review.title }}</h2>
              </div>
              <div class="row tight">
                <span class="badge big" :class="isReport ? 'badge--violet' : verdictText[review.verdict]?.cls">{{ isReport ? 'Report' : verdictText[review.verdict]?.label ?? review.verdict }}</span>
                <button class="secondary tiny" title="Delete this review" @click="confirmDelete = true">Delete</button>
              </div>
            </div>
            <!-- eslint-disable-next-line vue/no-v-html -- renderMarkdown escapes all HTML first -->
            <div v-if="review.summary" class="md summary" :class="{ clamped: longSummary && !summaryOpen && !isReport }" v-html="md(review.summary)" />
            <button v-if="longSummary && !isReport" class="link small" @click="summaryOpen = !summaryOpen">{{ summaryOpen ? 'Show less' : 'Read the full summary' }}</button>

            <div v-if="!isReport && counts.total" class="progress">
              <div class="row between">
                <strong>{{ counts.open ? `${decided(counts)} of ${plural(counts.total, 'recommendation')} decided` : 'Every recommendation has a decision' }}</strong>
                <span class="legend small">
                  <span v-for="s in segments(counts)" :key="s.cls"><i :class="['sw', s.cls]" />{{ s.n }} {{ s.label }}</span>
                  <span v-if="counts.open"><i class="sw seg--open" />{{ counts.open }} to decide</span>
                </span>
              </div>
              <span class="bar" role="img" :aria-label="`${decided(counts)} of ${counts.total} decided`">
                <span v-for="s in segments(counts)" :key="s.cls" :class="['seg', s.cls]" :style="{ flexGrow: s.n }" />
                <span class="seg seg--open" :style="{ flexGrow: counts.open }" />
              </span>
            </div>
          </div>

          <template v-if="!isReport && counts.total">
            <!-- How deciding works -->
            <section class="panel guide" :class="{ closed: !guideOpen }">
              <button class="guide-toggle" :aria-expanded="guideOpen" @click="toggleGuide">
                <span><span class="eyebrow">How deciding works</span><span v-if="!guideOpen" class="muted small"> · Accept, Reject, Defer, Mark fixed</span></span>
                <span class="muted small">{{ guideOpen ? 'Hide' : 'Show' }}</span>
              </button>
              <div v-if="guideOpen" class="guide-grid">
                <div v-for="d in DECISIONS" :key="d.status" class="guide-item" :class="d.cls">
                  <p class="guide-name"><span class="ico" aria-hidden="true">{{ d.icon }}</span>{{ d.label }}</p>
                  <p><strong>{{ d.what }}</strong></p>
                  <p class="muted small">{{ d.then }}</p>
                </div>
              </div>
              <p v-if="guideOpen" class="dim small">Decisions are saved immediately and can always be undone or reopened. They never change code.</p>
            </section>

            <!-- Views -->
            <nav class="views" aria-label="Filter recommendations">
              <button v-for="v in VIEWS" :key="v.id" class="view" :class="{ active: view === v.id }" @click="view = v.id">
                {{ v.label }}<span class="n">{{ v.n }}</span>
              </button>
            </nav>
            <p v-if="actionError" class="error small">{{ actionError }}</p>
            <div v-if="view === 'accepted' && counts.accepted" class="panel tofix">
              <div class="grow">
                <strong>{{ plural(counts.accepted, 'accepted item') }} to fix</strong>
                <p class="muted small">Fix them one at a time from each card, or all together on one branch with one pull request. You can also ask an agent over MCP: “fix the accepted comments in review {{ review.id }}”.</p>
                <p v-if="fixBlocked" class="warn small">{{ fixBlocked }}</p>
              </div>
              <button v-if="allFixable.length > 1" :disabled="!!fixBlocked" @click="fixTarget = allFixable">Fix all {{ allFixable.length }} on one branch</button>
            </div>

            <div v-if="!byFile.length" class="empty">
              <template v-if="view === 'open'"><strong>Nothing left to decide</strong>{{ counts.accepted ? `${plural(counts.accepted, 'item')} accepted and waiting to be fixed — see To fix.` : 'Every recommendation in this review has a decision.' }}</template>
              <template v-else-if="view === 'accepted'"><strong>Nothing to fix</strong>Accepted recommendations appear here until they are marked fixed.</template>
              <template v-else-if="view === 'deferred'"><strong>Nothing parked</strong>Deferred recommendations wait here.</template>
              <template v-else><strong>Nothing here yet</strong>Fixed and rejected recommendations appear here.</template>
            </div>

            <!-- Recommendations -->
            <section v-for="[file, cs] in byFile" :key="file" class="file">
              <p class="file-name mono">{{ file }} <span class="dim">· {{ plural(cs.length, 'item') }}</span></p>
              <article v-for="c in cs" :key="c.id" class="card" :class="[`sev-${c.severity}`, { closed: CLOSED.includes(c.status) }]">
                <header class="row tight meta">
                  <SevBadge :severity="c.severity" />
                  <span class="tag">{{ c.category }}</span>
                  <a v-if="code[c.id]?.url" :href="code[c.id]!.url" target="_blank" rel="noopener noreferrer" class="mono loc" :title="`Open on ${forgeName(code[c.id]!.url!)}`">{{ loc(c) }} ↗</a>
                  <span v-else class="mono loc">{{ loc(c) }}</span>
                  <NuxtLink v-if="c.symbol_id" :to="explore(c.symbol_id)" class="mono small" title="Open this symbol in the explorer">{{ c.symbol }}</NuxtLink>
                  <NuxtLink v-if="c.rule" :to="`${base}/security`" class="tag sec">{{ ruleTitle(c.rule) ?? c.rule }}</NuxtLink>
                </header>

                <!-- eslint-disable vue/no-v-html -- renderMarkdown escapes all HTML first -->
                <h3 class="md headline" v-html="md(headline(c.message)[0])" />
                <div v-if="headline(c.message)[1]" class="md why" v-html="md(headline(c.message)[1])" />
                <div v-if="c.suggestion" class="fix">
                  <p class="label">Suggested fix</p>
                  <div class="md" v-html="md(c.suggestion)" />
                </div>
                <!-- eslint-enable vue/no-v-html -->
                <ReviewCode :comment="c" :code="code[c.id]" />

                <!-- Undecided: the four choices -->
                <footer v-if="c.status === 'open' || c.status === 'deferred'" class="decide">
                  <template v-if="rejecting !== c.id">
                    <span class="decide-q">{{ c.status === 'deferred' ? 'Deferred. Decide now?' : 'Your decision' }}</span>
                    <template v-for="d in DECISIONS" :key="d.status">
                      <button v-if="!(d.status === 'deferred' && c.status === 'deferred')" class="choice" :class="d.cls" :disabled="busy === c.id" :title="`${d.what} ${d.then}`" @click="d.status === 'rejected' ? startReject(c) : decide(c, d.status)">
                        <span class="ico" aria-hidden="true">{{ d.icon }}</span>
                        <span><strong>{{ d.label }}</strong><small>{{ d.short }}</small></span>
                      </button>
                    </template>
                  </template>
                  <form v-else class="reject-form" @submit.prevent="decide(c, 'rejected', reason)">
                    <label class="small" :for="`why-${c.id}`">Why are you rejecting it? <span class="dim">Optional, but it helps the next reviewer.</span></label>
                    <div class="row tight">
                      <input :id="`why-${c.id}`" v-model="reason" maxlength="1000" placeholder="e.g. false positive: the input is validated in the caller">
                      <button type="submit" :disabled="busy === c.id">Reject</button>
                      <button type="button" class="secondary" @click="rejecting = ''">Cancel</button>
                    </div>
                  </form>
                </footer>

                <!-- Accepted: the fix -->
                <div v-if="c.status === 'accepted' && c.fix" class="fixbox" :class="`fix--${c.fix.state}`">
                  <template v-if="stale(c)">
                    <span class="ico bad" aria-hidden="true">!</span>
                    <span class="grow"><strong>This fix seems stuck</strong><span class="muted small">It started {{ relTime(c.fix.at) }} and never finished. Try again.</span></span>
                  </template>
                  <template v-else-if="c.fix.state === 'queued'">
                    <span class="spin spin--slow" aria-hidden="true" />
                    <span class="grow"><strong>Waiting for its turn…</strong><span class="muted small">CodeSynapse runs one job at a time (sync, indexing, AI). The fix starts by itself when the current job is done. Queued {{ relTime(c.fix.at) }}.</span></span>
                  </template>
                  <template v-else-if="c.fix.state === 'running'">
                    <span class="spin" aria-hidden="true" />
                    <span class="grow"><strong>Writing the fix on a new branch…</strong><span class="muted small">The model rewrites the file, then CodeSynapse commits, pushes and opens a draft pull request. Large files take a few minutes on a local model. Started {{ relTime(c.fix.at) }}.</span></span>
                  </template>
                  <template v-else-if="c.fix.state === 'pushed'">
                    <span class="ico ok" aria-hidden="true">↗</span>
                    <span class="grow">
                      <strong>Fix pushed to <span class="mono">{{ c.fix.branch }}</span></strong>
                      <span class="muted small">Your CI runs on this branch. Review the pull request, merge it, then mark this fixed.<template v-if="c.fix.error"> {{ c.fix.error }}</template></span>
                    </span>
                    <a v-if="c.fix.pr_url" :href="c.fix.pr_url" target="_blank" rel="noopener noreferrer" class="btn">Open pull request ↗</a>
                    <a v-else-if="c.fix.compare_url" :href="c.fix.compare_url" target="_blank" rel="noopener noreferrer" class="btn">Open on GitHub ↗</a>
                  </template>
                  <template v-else>
                    <span class="ico bad" aria-hidden="true">!</span>
                    <span class="grow"><strong>The fix could not be made</strong><span class="small err">{{ c.fix.error }}</span></span>
                  </template>
                </div>

                <!-- Decided: status and the next step -->
                <footer v-if="c.status !== 'open' && c.status !== 'deferred'" class="decided">
                  <span class="badge" :class="STATUS_TEXT[c.status]?.cls">{{ STATUS_TEXT[c.status]?.label ?? c.status }}</span>
                  <span class="muted small">{{ STATUS_TEXT[c.status]?.next }}<template v-if="c.status_by"> · by {{ c.status_by }}</template><template v-if="c.updated_at"> · {{ relTime(c.updated_at) }}</template></span>
                  <span v-if="c.status_note" class="note small">“{{ c.status_note }}”</span>
                  <span class="spacer" />
                  <span v-if="c.status === 'accepted' && fixable(c)" :title="fixBlocked">
                    <button class="tiny" :disabled="!!fixBlocked || busy === c.id" @click="fixTarget = [c]">{{ c.fix?.state === 'failed' || stale(c) ? 'Try the fix again' : 'Create fix branch & PR' }}</button>
                  </span>
                  <button v-if="c.status === 'accepted'" class="secondary tiny" :disabled="busy === c.id || (unfinished(c) && !stale(c))" @click="decide(c, 'resolved')">★ Mark fixed</button>
                  <button class="secondary tiny" :disabled="busy === c.id" @click="decide(c, 'open')">{{ c.status === 'accepted' ? 'Undo' : 'Reopen' }}</button>
                </footer>
              </article>
            </section>
          </template>

          <ConfirmDialog :open="!!fixTarget" :title="fixTarget && fixTarget.length > 1 ? `Fix ${fixTarget.length} items on a new branch?` : 'Fix this on a new branch?'" confirm-label="Create branch & pull request" :busy="fixStarting" @confirm="startFix" @cancel="fixTarget = null; fixError = ''">
            <ol class="steps">
              <li>Your AI model rewrites {{ fixTarget && new Set(fixTarget.map((c) => c.file)).size > 1 ? 'the affected files' : 'the file' }} to address {{ fixTarget && fixTarget.length > 1 ? 'these comments' : 'the comment' }}, starting from commit <span class="mono">{{ review.commit?.slice(0, 10) || 'the synced head' }}</span>.</li>
              <li>CodeSynapse commits it to a new branch <span class="mono">codesynapse/fix-…</span> and pushes it to <span class="mono">{{ repoInfo?.remote }}</span>. Nothing is pushed to <span class="mono">{{ repoInfo?.branch }}</span>.</li>
              <li>A draft pull request into <span class="mono">{{ repoInfo?.branch }}</span> is opened, so your CI/CD pipeline runs on it.</li>
              <li>You review the pull request and merge it, or close it if the fix is wrong.</li>
            </ol>
            <p class="muted small">Only the files named in the comments can change. CI and git configuration files are never touched.</p>
            <p v-if="fixError" class="error small">{{ fixError }}</p>
          </ConfirmDialog>
          <ConfirmDialog :open="confirmDelete" title="Delete this review?" confirm-label="Delete review" danger :busy="deleting" @confirm="removeReview" @cancel="confirmDelete = false">
            <p>“{{ review.title }}” and its {{ plural(counts.total, 'recommendation') }} will be removed. Code is not affected.</p>
          </ConfirmDialog>
        </template>
      </section>
    </div>

    <Transition name="toast">
      <div v-if="toast" class="toast" role="status">
        <span>{{ toast.text }}</span>
        <button class="link" @click="undo">Undo</button>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.how { gap: 10px; }
.cmd { margin: 0; padding: 10px 12px; background: var(--bg); border: 1px solid var(--line); border-radius: var(--radius-xs); font: 13px var(--font-mono); color: var(--accent-ink); white-space: pre-wrap; }
.layout { display: grid; grid-template-columns: 280px minmax(0, 1fr); gap: 20px; align-items: start; }
.list { position: sticky; top: 24px; max-height: calc(100vh - 48px); overflow-y: auto; gap: 4px; }
.item { all: unset; box-sizing: border-box; cursor: pointer; display: grid; gap: 6px; padding: 10px 12px; border-radius: var(--radius-sm); }
.item:hover { background: var(--hover); }
.item.is-active { background: var(--primary); color: var(--on-primary); }
.item.is-active :is(.muted, .dim, .small) { color: inherit; }
.item:focus-visible { box-shadow: var(--focus); }
.item-title { font-weight: 600; color: var(--ink); line-height: 1.35; }
.detail { display: grid; gap: 16px; min-width: 0; }
.head { gap: 12px; }
.title { font-size: 20px; }
.big { font-size: 11px; padding: 5px 12px; }
.tight { gap: 6px; }
.between { justify-content: space-between; flex-wrap: wrap; gap: 8px; }
.spacer { flex: 1; }
.small { font-size: 12.5px; }
.warn { color: var(--warning); }
.link { all: unset; cursor: pointer; color: var(--accent-ink); font-size: 13px; }
.link:hover { text-decoration: underline; }
.link:focus-visible { box-shadow: var(--focus); }

/* Markdown from the model (escaped by renderMarkdown) */
.md :deep(p) { margin: 0 0 6px; line-height: 1.6; }
.md :deep(p:last-child) { margin-bottom: 0; }
.md :deep(code) { font-family: var(--font-mono); font-size: 0.92em; background: var(--bg); border: 1px solid var(--line); padding: 0 5px; border-radius: 4px; word-break: break-word; }
.md :deep(pre) { margin: 4px 0; padding: 10px 12px; background: var(--bg); border: 1px solid var(--line); border-radius: var(--radius-xs); overflow-x: auto; }
.md :deep(pre code) { border: 0; padding: 0; background: none; }
.md :deep(ul), .md :deep(ol) { margin: 0 0 6px; padding-left: 20px; display: grid; gap: 3px; }
.md :deep(h3), .md :deep(h4) { margin: 12px 0 6px; font-size: 15px; }
.summary { color: var(--ink); }
.summary.clamped { max-height: 5.2em; overflow: hidden; mask-image: linear-gradient(#000 55%, transparent); }

/* Progress */
.progress { display: grid; gap: 8px; padding-top: 12px; border-top: 1px dashed var(--line); }
.bar { display: flex; height: 8px; border-radius: var(--pill); overflow: hidden; background: var(--panel-2); gap: 2px; }
.bar--mini { height: 4px; gap: 1px; }
.seg { display: block; min-width: 0; flex-basis: 0; }
.seg--fixed { background: var(--success); }
.seg--accept { background: var(--accent); }
.seg--reject { background: var(--line-strong); }
.seg--defer { background: var(--warning); }
.seg--open { background: transparent; }
.legend { display: flex; gap: 12px; flex-wrap: wrap; color: var(--muted); }
.sw { display: inline-block; width: 8px; height: 8px; border-radius: 2px; margin-right: 5px; vertical-align: baseline; }
.sw.seg--open { border: 1px solid var(--line-strong); }

/* Guide */
.guide { gap: 12px; }
.guide.closed { padding-top: 10px; padding-bottom: 10px; }
.guide-toggle { all: unset; cursor: pointer; display: flex; justify-content: space-between; align-items: center; }
.guide-toggle:focus-visible { box-shadow: var(--focus); }
.guide-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.guide-item { display: grid; gap: 4px; align-content: start; padding: 12px; border-radius: var(--radius-sm); background: var(--panel-2); border: 1px solid var(--line); border-top: 2px solid var(--line-strong); }
.guide-item p { margin: 0; }
.guide-name { font-family: var(--font-mono); font-size: 11px; text-transform: uppercase; letter-spacing: 0.08em; display: flex; gap: 8px; align-items: center; }
.guide-item.accept { border-top-color: var(--accent); }
.guide-item.reject { border-top-color: var(--line-strong); }
.guide-item.defer { border-top-color: var(--warning); }
.guide-item.fixed { border-top-color: var(--success); }

/* Views */
.views { display: flex; gap: 4px; flex-wrap: wrap; border-bottom: 1px solid var(--line); }
.view { all: unset; cursor: pointer; padding: 8px 14px; color: var(--muted); border-bottom: 2px solid transparent; margin-bottom: -1px; display: inline-flex; gap: 8px; align-items: center; font-size: 14px; }
.view:hover { color: var(--ink); }
.view.active { color: var(--ink); border-bottom-color: var(--accent); }
.view:focus-visible { box-shadow: var(--focus); }
.view .n { font: 11px var(--font-mono); padding: 1px 7px; border-radius: var(--pill); background: var(--panel-2); color: var(--muted); }
.view.active .n { background: var(--accent-soft); color: var(--accent-ink); }

/* Cards */
.file { display: grid; gap: 10px; }
.file-name { color: var(--info); font-size: 13px; margin: 6px 0 0; }
.card { display: grid; gap: 10px; padding: 16px 18px; border-radius: var(--radius-sm); background: var(--panel); border: 2px solid var(--line); }
.card.closed { opacity: 0.65; }
.meta { flex-wrap: wrap; }
.loc { color: var(--dim); font-size: 12px; }
a.loc { color: var(--info); text-decoration: none; }
a.loc:hover { text-decoration: underline; }
.headline { margin: 0; font-size: 15.5px; font-weight: 600; line-height: 1.45; }
.headline :deep(p) { margin: 0; }
.why { color: var(--muted); font-size: 14px; }
.fix { display: grid; gap: 6px; padding: 10px 12px; border-radius: var(--radius-xs); background: var(--success-soft); border: 1px solid var(--success); }
.fix .label { color: var(--success); margin: 0; }

.decide { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; padding-top: 12px; border-top: 1px dashed var(--line); }
.decide-q { font: 11px var(--font-mono); text-transform: uppercase; letter-spacing: 0.08em; color: var(--muted); margin-right: 4px; }
.choice { all: unset; box-sizing: border-box; cursor: pointer; display: inline-flex; gap: 8px; align-items: center; padding: 7px 12px 7px 10px; border-radius: var(--radius-xs); border: 1px solid var(--line-strong); background: var(--panel-2); color: var(--ink); }
.choice strong { display: block; font-size: 13px; line-height: 1.2; }
.choice small { display: block; font-size: 11px; color: var(--muted); }
.choice:hover:not(:disabled) { border-color: var(--line-strong); background: var(--hover); }
.choice:focus-visible { box-shadow: var(--focus); }
.choice:disabled { opacity: 0.5; cursor: progress; }
.choice.accept:hover:not(:disabled) { border-color: var(--accent); }
.choice.reject:hover:not(:disabled) { border-color: var(--line-strong); }
.choice.defer:hover:not(:disabled) { border-color: var(--warning); }
.choice.fixed:hover:not(:disabled) { border-color: var(--success); }
.ico { width: 20px; height: 20px; border-radius: 50%; display: inline-grid; place-items: center; font-size: 11px; background: var(--panel-2); flex: none; }
.accept .ico { color: var(--accent-ink); }
.reject .ico { color: var(--muted); }
.defer .ico { color: var(--warning); }
.fixed .ico { color: var(--success); }
.reject-form { display: grid; gap: 6px; width: 100%; }
.reject-form input { flex: 1; min-width: 200px; }
.decided { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; padding-top: 10px; border-top: 1px dashed var(--line); }
.note { color: var(--ink); font-style: italic; }

.fixbox { display: flex; gap: 10px; align-items: center; padding: 10px 12px; border-radius: var(--radius-xs); border: 1px solid var(--line); background: var(--panel-2); }
.fixbox strong { display: block; font-size: 13.5px; }
.fixbox .grow, .tofix .grow { flex: 1; min-width: 0; display: grid; gap: 2px; }
.fix--running, .fix--queued { border-color: var(--info); background: var(--info-soft); }
.spin--slow { animation-duration: 2.4s; border-top-color: var(--muted); }
.fix--pushed { border-color: var(--success); background: var(--success-soft); }
.fix--failed { border-color: var(--danger); background: var(--danger-soft); }
.ico.ok { color: var(--success); }
.ico.bad { color: var(--danger); font-weight: 700; }
.err { color: var(--danger); word-break: break-word; }
.btn { display: inline-flex; align-items: center; padding: 6px 12px; border-radius: var(--radius-xs); background: var(--accent); color: var(--on-accent); font-weight: 600; font-size: 13px; text-decoration: none; white-space: nowrap; }
.btn:hover { background: var(--accent-hover); }
.spin { width: 16px; height: 16px; border-radius: 50%; border: 2px solid var(--line-strong); border-top-color: var(--info); animation: spin 0.9s linear infinite; flex: none; }
@keyframes spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .spin { animation: none; } }
.tofix { flex-direction: row; display: flex; gap: 16px; align-items: center; flex-wrap: wrap; }
.tofix p { margin: 0; }
.steps { margin: 0 0 10px; padding-left: 20px; display: grid; gap: 6px; line-height: 1.5; }
.toast { position: fixed; bottom: 24px; left: 50%; transform: translateX(-50%); z-index: 50; display: flex; gap: 16px; align-items: center; padding: 10px 16px; border-radius: var(--radius-sm); background: var(--panel-2); border: 1px solid var(--line-strong); box-shadow: var(--shadow-pop); font-size: 14px; }
.toast-enter-active, .toast-leave-active { transition: opacity 0.2s, transform 0.2s; }
.toast-enter-from, .toast-leave-to { opacity: 0; transform: translate(-50%, 8px); }

@media (max-width: 1100px) { .guide-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 1000px) { .layout { grid-template-columns: 1fr; } .list { position: static; max-height: none; } }
@media (max-width: 560px) { .guide-grid { grid-template-columns: 1fr; } .choice { flex: 1 1 calc(50% - 8px); } }
</style>
