<script setup lang="ts">
import type { AIProvider, Job, ReviewSummary } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const { data: providers } = useApiGet<{ providers: AIProvider[] }>('ai/providers')
useHead({ title: () => `AI assistant · ${repo.value}` })
const { data: reviews, error: reviewsError, refresh: refreshReviews } = useApiGet<{ reviews: ReviewSummary[] }>(() => repoPath(repo.value, 'reviews'))
const aiResults = computed(() => (reviews.value?.reviews ?? []).filter((r) => r.source === 'ai'))

const TASKS = [
  { id: 'security-triage', title: 'Triage security findings', body: 'The model judges each medium-or-higher finding — likely exploitable, needs review or false positive — and proposes a fix. You accept or reject each one.' },
  { id: 'review', title: 'Review the riskiest code', body: 'Reviews hotspots and untested complex functions, or a diff you paste below, and leaves comments on exact lines with suggested fixes.' },
  { id: 'briefing', title: 'Write a briefing', body: 'A short architecture and risk overview for someone new to the repository, written from graph summaries only — no source is sent.' },
  { id: 'pr-summary', local: true, title: 'Summarise a change', body: 'Paste a git diff below. A local model writes what changed, the riskiest parts and a merge checklist from the graph (callers, tests, findings). Needs a local model.' },
  { id: 'cleanup-briefing', local: true, title: 'Brief the cleanup plan', body: 'Turns the ranked cleanup plan (dead code, duplicates, hotspots, vulnerable packages) into what to do first and what is safe and quick. Needs a local model.' },
]
const LOCAL_ONLY = new Set(['pr-summary', 'cleanup-briefing'])
const tasks = computed(() => TASKS.filter((t) => !LOCAL_ONLY.has(t.id) || providers.value?.providers.some((p) => p.tasks.includes(t.id))))
const routed = (task: string) => providers.value?.providers.find((p) => p.tasks.includes(task))?.name ?? providers.value?.providers[0]?.name ?? ''
const choice = reactive<Record<string, string>>({})
const diff = ref('')
const jobs = reactive<Record<string, Job | null>>({})
const errors = reactive<Record<string, string>>({})
const timers: Record<string, ReturnType<typeof setInterval>> = {}

function stopAll() {
  for (const task of Object.keys(timers)) {
    clearInterval(timers[task])
    delete timers[task]
  }
}
async function run(task: string) {
  errors[task] = ''
  clearInterval(timers[task])
  try {
    const body: Record<string, string> = { provider: choice[task] || '' }
    if ((task === 'review' || task === 'pr-summary') && diff.value.trim()) body.diff = diff.value
    const job = await apiPost<Job>(repoPath(repo.value, `ai/${task}`), body)
    jobs[task] = job
    let misses = 0
    timers[task] = setInterval(async () => {
      try {
        const j = await apiGet<Job>(`jobs/${encodeURIComponent(job.id)}`)
        misses = 0
        jobs[task] = j
        if (j.state !== 'running') {
          clearInterval(timers[task])
          if (j.state === 'failed') errors[task] = j.error ?? 'failed'
          void refreshReviews()
        }
      } catch (e) {
        // A few failed polls are network noise; after that, say so and stop.
        if (++misses >= 3) {
          clearInterval(timers[task])
          errors[task] = `Lost track of the job: ${apiErrorMessage(e)}. The result still appears in Reviews when it finishes.`
        }
      }
    }, 2500)
  } catch (e) {
    errors[task] = apiErrorMessage(e)
  }
}
// Jobs belong to one repository: forget them when another one is opened.
watch(repo, () => {
  stopAll()
  for (const k of Object.keys(jobs)) delete jobs[k]
  for (const k of Object.keys(errors)) delete errors[k]
})
onBeforeUnmount(stopAll)
const elapsed = (j: Job) => Math.round((Date.now() - Date.parse(j.started_at)) / 1000)
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Improve" title="AI assistant"
      lede="Ask a model to triage, review or explain this repository. CodeSynapse sends it only the graph context it needs, so local models stay fast and cloud costs stay low. Results land in Reviews for you to accept or reject."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'AI assistant' }]">
      <NuxtLink to="/ai" class="btn btn--ghost">Model settings</NuxtLink>
    </PageHeader>

    <div v-if="providers && !providers.providers.length" class="empty"><strong>No AI model configured</strong>Set one up on the <NuxtLink to="/ai">AI models</NuxtLink> page — a local MLX or Ollama server is enough.</div>

    <div v-else class="grid cols-3">
      <section v-for="t in tasks" :key="t.id" class="panel task">
        <h2>{{ t.title }}</h2>
        <p class="muted">{{ t.body }}</p>
        <textarea v-if="t.id === 'review' || t.id === 'pr-summary'" v-model="diff" :placeholder="t.id === 'review' ? 'Optional: paste a git diff to review a change instead' : 'Paste a git diff to summarise'" class="diff" :aria-label="t.id === 'review' ? 'Git diff to review (optional)' : 'Git diff to summarise'" />
        <label class="row tight">Model
          <select v-model="choice[t.id]" :aria-label="`Model for ${t.title}`">
            <option value="">{{ routed(t.id) }} (default)</option>
            <option v-for="p in providers?.providers ?? []" :key="p.name" :value="p.name">{{ p.name }} · {{ p.local ? 'local' : 'cloud' }}</option>
          </select>
        </label>
        <div class="row">
          <button :disabled="jobs[t.id]?.state === 'running'" @click="run(t.id)">{{ jobs[t.id]?.state === 'running' ? `Working… ${elapsed(jobs[t.id]!)} s` : 'Run' }}</button>
          <NuxtLink v-if="jobs[t.id]?.state === 'succeeded' && jobs[t.id]?.result" :to="{ path: `${base}/reviews`, query: { id: jobs[t.id]!.result } }" class="btn btn--ghost">Open result →</NuxtLink>
        </div>
        <p v-if="jobs[t.id]?.state === 'running'" class="muted small">Local models can take a few minutes; you can leave this page — the result appears in Reviews.</p>
        <p v-if="errors[t.id]" class="error small" role="alert">{{ errors[t.id] }}</p>
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">History</p><h2>AI results for {{ repo }}</h2>
      <div v-if="aiResults.length" class="table-wrap">
        <table>
          <thead><tr><th>Result</th><th>Model</th><th>Open</th><th>When</th></tr></thead>
          <tbody>
            <tr v-for="r in aiResults" :key="r.id">
              <td><NuxtLink :to="{ path: `${base}/reviews`, query: { id: r.id } }">{{ r.title }}</NuxtLink> <span v-if="r.kind === 'report'" class="badge badge--violet">report</span></td>
              <td class="mono dim">{{ r.author }}</td>
              <td class="num">{{ r.kind === 'report' ? '–' : fmtInt(r.counts.open) }}</td>
              <td class="muted">{{ relTime(r.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else-if="reviewsError" class="notice notice--danger" role="alert">{{ apiErrorMessage(reviewsError) }}</p>
      <p v-else-if="!reviews" class="muted" role="status">Loading…</p>
      <p v-else class="muted">Nothing generated yet.</p>
    </section>
  </div>
</template>

<style scoped>
.task { gap: 10px; }
.diff { min-height: 90px; }
.tight { gap: 8px; }
.tight select { width: auto; flex: 1; }
.small { font-size: 12.5px; }
</style>
