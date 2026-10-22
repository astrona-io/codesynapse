<script setup lang="ts">
import type { Job, Source, WorkspaceOverview } from '~/utils/types'

interface Settings { git_token_configured: boolean; git_token_host: string; git_token_hosts?: string[]; clone_enabled: boolean; allow_private_git: boolean; vuln_scan: boolean }

const { data: ov, refresh: refreshOverview } = useApiGet<WorkspaceOverview>('overview')
const { data: sources } = useApiGet<{ sources: Source[] }>('sources')
const { data: jobs, refresh: refreshJobs } = useApiGet<{ jobs: Job[] }>('jobs')
const { data: settings } = useApiGet<Settings>('settings')

// 4 columns x 3 rows per page; the grid drops columns on narrow screens.
const PAGE_SIZE = 12
const page = ref(1)
const pageCount = computed(() => Math.max(1, Math.ceil((ov.value?.repos.length ?? 0) / PAGE_SIZE)))
watch(pageCount, (n) => { if (page.value > n) page.value = n })
const visibleRepos = computed(() => (ov.value?.repos ?? []).slice((page.value - 1) * PAGE_SIZE, page.value * PAGE_SIZE))

const indexed = computed(() => new Set((ov.value?.repos ?? []).map((r) => r.name)))
const running = computed(() => jobs.value?.jobs.some((j) => j.state === 'running') ?? false)
let timer: ReturnType<typeof setInterval> | undefined
watch(running, (now, before) => {
  if (now && !timer) timer = setInterval(() => refreshJobs(), 2000)
  if (!now && timer) { clearInterval(timer); timer = undefined }
  if (before && !now) refreshOverview()
}, { immediate: true })
onBeforeUnmount(() => timer && clearInterval(timer))

const error = ref('')
async function ingest(source: string) {
  error.value = ''
  try {
    await apiPost<Job>('ingest', { source })
    await refreshJobs()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}

const reg = reactive({ url: '', branch: '', name: '' })
const regError = ref('')
const regOk = ref('')
const urlHost = computed(() => { try { return new URL(reg.url.trim()).hostname.toLowerCase() } catch { return '' } })
const hostHasToken = computed(() => !!urlHost.value && (settings.value?.git_token_hosts ?? []).includes(urlHost.value))
async function register() {
  regError.value = ''
  regOk.value = ''
  try {
    const job = await apiPost<Job>('repos', { url: reg.url.trim(), branch: reg.branch.trim(), name: reg.name.trim() })
    regOk.value = `Cloning ${job.repo}… it appears below when indexing finishes.`
    reg.url = ''; reg.branch = ''; reg.name = ''
    await refreshJobs()
  } catch (e) {
    regError.value = apiErrorMessage(e)
  }
}

const gradeColor: Record<string, string> = { A: 'var(--green)', B: 'var(--cyan)', C: 'var(--amber)', D: 'var(--orange)', F: 'var(--pink)' }
const syncText = (s?: { state: string; interval_minutes: number }) => {
  if (!s || s.state === 'manual') return 'Manual sync'
  const every = ({ 15: '15 min', 30: '30 min', 60: 'hour', 360: '6 hours', 1440: 'day' } as Record<number, string>)[s.interval_minutes] ?? `${s.interval_minutes} min`
  return s.state === 'paused' ? `Auto-sync paused (every ${every})` : `Auto-sync every ${every}`
}
const jobLabel = (j: Job) => ({ ingest: 'Indexing', clone: 'Cloning', sync: 'Syncing' }[j.kind ?? 'ingest'] ?? 'Indexing')
const stateText = (j: Job) => (j.state === 'running' ? 'in progress' : j.state === 'succeeded' ? `done · ${fmtInt(j.nodes)} nodes` : 'failed')
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Workspace" title="Repositories" lede="Each repository becomes a graph of its code, config, docs, history and dependencies. Open one to see its health and what to do next.">
      <span class="pill"><span class="dot" :class="running ? 'dot--warn' : 'dot--ok'" />{{ running ? 'Indexing…' : `${plural(ov?.repos.length ?? 0, 'repository', 'repositories')} indexed` }}</span>
    </PageHeader>

    <section v-if="ov?.repos.length" class="cards">
      <NuxtLink v-for="r in visibleRepos" :key="r.name" :to="`/repos/${encodeURIComponent(r.name)}`" class="panel repo-card">
        <div class="repo-top">
          <div class="repo-name">
            <strong>{{ r.name }}</strong>
            <span class="mono dim">{{ r.branch || 'no branch' }} · {{ r.managed ? 'cloned' : 'mounted' }}</span>
          </div>
          <span class="grade" :style="{ color: gradeColor[r.grade], borderColor: gradeColor[r.grade] }" :title="`Security grade ${r.grade}: ${GRADE_TEXT[r.grade]}`">{{ r.grade }}</span>
        </div>
        <FormatChips :formats="r.formats" :helm-charts="r.helm_charts" :k8s="r.k8s_resources" compact />
        <p class="repo-verdict" :class="{ bad: (r.findings.critical ?? 0) + (r.findings.high ?? 0) }">
          <template v-if="(r.findings.critical ?? 0) + (r.findings.high ?? 0)">{{ plural((r.findings.critical ?? 0) + (r.findings.high ?? 0), 'critical/high issue') }} to fix</template>
          <template v-else>{{ GRADE_TEXT[r.grade] }}</template>
          <span v-if="r.dead_functions" class="muted"> · {{ plural(r.dead_functions, 'unused function') }}</span>
        </p>
        <div class="repo-foot">
          <p class="muted small">
            Indexed {{ relTime(r.ingested_at) }}<span v-if="r.stale" class="stale"> · out of date</span>
            <br>
            <span :class="`sync sync--${r.schedule?.state ?? 'manual'}`">{{ syncText(r.schedule) }}</span>
          </p>
          <span class="row tight-actions">
            <SwitchToRemote v-if="!r.managed" :repo="r.name" :remote="r.remote" :branch="r.branch" small @switched="refreshOverview" />
            <RemoveRepo :repo="r.name" :managed="r.managed" small @removed="refreshOverview" />
          </span>
        </div>
      </NuxtLink>
    </section>
    <nav v-if="pageCount > 1" class="pager" aria-label="Repository pages">
      <button class="secondary tiny" :disabled="page <= 1" @click="page--">Previous</button>
      <span class="muted small">Page {{ page }} of {{ pageCount }} · {{ plural(ov?.repos.length ?? 0, 'repository', 'repositories') }}</span>
      <button class="secondary tiny" :disabled="page >= pageCount" @click="page++">Next</button>
    </nav>
    <div v-else-if="!ov?.repos.length" class="empty"><strong>No repositories yet</strong>Clone one by URL or index a mounted source below — it takes about a minute.</div>

    <div class="grid cols-2">
      <section class="panel edge--cyan">
        <p class="eyebrow">Add a repository</p>
        <h2>Clone from GitHub, GitLab or any HTTPS git host</h2>
        <form class="clone" @submit.prevent="register">
          <label>Repository URL
            <input v-model="reg.url" type="url" required placeholder="https://github.com/org/repo.git" autocomplete="off">
          </label>
          <div class="row">
            <label class="grow">Branch <span class="dim">(optional)</span><input v-model="reg.branch" placeholder="default branch"></label>
            <label class="grow">Name <span class="dim">(optional)</span><input v-model="reg.name" placeholder="owner/name from the URL"></label>
          </div>
          <div class="row">
            <button type="submit" :disabled="!reg.url || running || settings?.clone_enabled === false">Clone & index</button>
            <span class="muted small">Shallow clone over HTTPS; nothing is pushed.</span>
          </div>
        </form>
        <p v-if="regOk" class="ok">{{ regOk }}</p>
        <p v-if="regError" class="error">{{ regError }}</p>

        <div class="token" :class="settings?.git_token_configured ? 'token--ok' : 'token--missing'">
          <template v-if="settings?.git_token_configured">
            <strong>Private repositories on {{ (settings.git_token_hosts?.length ? settings.git_token_hosts : [settings.git_token_host]).join(', ') }}: enabled</strong>
            <p class="muted">Tokens are configured on the server. Each is sent only to its own host and never shown here. Add another host with <code>just git-token &lt;host&gt;</code>.</p>
          </template>
          <template v-else>
            <strong>Private repositories need a token</strong>
            <p class="muted">Public repositories work right away. For private ones, create a
              <a href="https://github.com/settings/personal-access-tokens/new" target="_blank" rel="noopener noreferrer">fine-grained GitHub token</a>
              with read-only <em>Contents</em> access to the repositories, then run this in the project folder:</p>
            <pre class="cmd">just git-token</pre>
            <p class="muted small">It asks for the token without showing it, stores it in <code>.env</code> and restarts the API. For GitLab, Forgejo or another host: <code>just git-token codeberg.org</code>.</p>
          </template>
          <p v-if="urlHost && !hostHasToken" class="muted small">No token is set for {{ urlHost }}: if this repository is private, cloning will fail. Run <code>just git-token {{ urlHost }}</code>.</p>
        </div>
      </section>

      <section class="panel">
        <p class="eyebrow">Mounted sources</p>
        <h2>Folders available to index</h2>
        <p class="muted small">Everything under <code>./sources</code> in the project folder. Copy a checkout there with <code>just ingest ~/src/repo</code>.</p>
        <p v-if="error" class="error">{{ error }}</p>
        <ul v-if="sources?.sources.length" class="sources">
          <li v-for="s in sources.sources" :key="s.name" class="source">
            <div class="source-head">
              <strong class="mono">{{ s.name }}</strong>
              <span v-if="indexed.has(s.name)" class="badge badge--green">indexed</span>
              <span v-if="s.is_git" class="badge badge--violet">git</span>
              <button class="secondary tiny" :disabled="running" @click="ingest(s.name)">{{ indexed.has(s.name) ? 'Re-index' : 'Index' }}</button>
            </div>
            <FormatChips :formats="s.content" compact />
            <p v-if="s.truncated" class="muted small">Large folder: counts are from the first 20,000 files.</p>
          </li>
        </ul>
        <div v-else class="empty"><strong>No mounted sources</strong>Put repositories in <code>./sources</code> to index them from disk.</div>
      </section>
    </div>

    <section v-if="jobs?.jobs.length" class="panel">
      <p class="eyebrow">Activity</p><h2>Recent indexing</h2>
      <ul class="jobs">
        <li v-for="j in jobs.jobs.slice(0, 8)" :key="j.id" class="job">
          <span class="dot" :class="{ 'dot--ok': j.state === 'succeeded', 'dot--bad': j.state === 'failed', 'dot--warn': j.state === 'running' }" />
          <span>{{ jobLabel(j) }} <strong class="mono">{{ j.repo }}</strong><span v-if="j.branch" class="dim"> ({{ j.branch }})</span></span>
          <span class="muted">{{ stateText(j) }}</span>
          <span class="muted small right">{{ relTime(j.started_at) }}</span>
          <p v-if="j.error" class="error small job-error">{{ j.error }}</p>
        </li>
      </ul>
    </section>
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 16px; grid-template-columns: repeat(4, minmax(0, 1fr)); }
@media (max-width: 1500px) { .cards { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 1100px) { .cards { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 640px) { .cards { grid-template-columns: minmax(0, 1fr); } }
.pager { display: flex; justify-content: center; align-items: center; gap: 14px; }
.repo-card { gap: 10px; color: var(--ink); transition: transform 0.15s, border-color 0.15s; }
.repo-card:hover { transform: translateY(-2px); border-color: var(--line-bright); color: var(--ink); }
.repo-top { display: flex; justify-content: space-between; align-items: flex-start; gap: 10px; }
.repo-name { display: grid; gap: 2px; min-width: 0; }
.repo-name strong { font-size: 17px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.grade { display: grid; place-items: center; width: 40px; height: 40px; border-radius: 12px; border: 1px solid; font: 700 20px var(--font-sans); flex: none; }
.repo-verdict { font-size: 14px; }
.repo-verdict.bad { color: var(--pink); }
.stale { color: var(--amber); }
.repo-foot { display: flex; justify-content: space-between; align-items: flex-end; gap: 10px; }
.sync--active { color: var(--green); }
.sync--paused { color: var(--amber); }
.small { font-size: 12.5px; }
.clone { display: grid; gap: 10px; }
.clone label { display: grid; gap: 4px; }
.grow { flex: 1 1 180px; }
.ok { color: var(--green); }
.token { margin-top: 8px; padding: 14px 16px; border-radius: var(--radius-sm); border: 1px dashed var(--line-strong); background: var(--panel-faint); display: grid; gap: 6px; }
.token--ok { border-style: solid; border-color: rgba(57, 255, 136, 0.35); }
.token--ok strong { color: var(--green); }
.token--missing strong { color: var(--amber); }
.cmd { margin: 0; padding: 8px 12px; background: var(--bg); border: 1px solid var(--line); border-radius: var(--radius-xs); font: 13px var(--font-mono); color: var(--orange-ink); }
.sources { list-style: none; margin: 0; padding: 0; display: grid; }
.source { display: grid; gap: 6px; padding: 12px 0; border-bottom: 1px dashed var(--line); }
.source:last-child { border-bottom: 0; }
.source-head { display: flex; align-items: center; gap: 8px; }
.source-head button { margin-left: auto; }
.jobs { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
.job { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding: 6px 0; border-bottom: 1px dashed var(--line); }
.job:last-child { border-bottom: 0; }
.right { margin-left: auto; }
.job-error { flex-basis: 100%; margin-left: 17px; }
.tight-actions { gap: 6px; flex-wrap: nowrap; }
</style>
