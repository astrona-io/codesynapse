<script setup lang="ts">
import type { IngestionOverview, IngestFile, IngestRun, SeriesRange } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Ingestion reports · ${repo.value}` })
const before = ref('')
const { data, error, refresh } = useApiGet<IngestionOverview>(() => repoPath(repo.value, `ingestion?before=${encodeURIComponent(before.value)}`))
const selected = ref('')
const offset = ref(0)
const { data: details, error: detailErr, refresh: refreshDetails } = useApiGet<{ run: IngestRun; files: IngestFile[]; next_offset: number }>(() => repoPath(repo.value, `ingestion/${encodeURIComponent(selected.value || '-') }?offset=${offset.value}`), { immediate: false })
const detailError = computed(() => detailErr.value ? apiErrorMessage(detailErr.value) : '')
const range = ref<SeriesRange>('24h')
const tick = ref(0)
const unavailable = ref(false)
const report = computed(() => details.value?.run.report ?? data.value?.latest_attempt?.report)
const metricLag = computed(() => {
  const last = data.value?.latest_attempt?.last_exported_at
  return !last || Date.now() - Date.parse(last) > 90000
})
const counts = computed<[string, number][]>(() => report.value ? [
  ['Encountered', report.value.total_files_scanned], ['Indexed', report.value.files_indexed],
  ['Written', report.value.files_written], ['Reused', report.value.files_reused],
  ['Skipped', report.value.files_skipped], ['Failed', report.value.files_failed],
  ['Unprocessed', report.value.files_unprocessed],
] : [])
async function loadRun(id: string, next = 0) {
  selected.value = id
  offset.value = next
  await refreshDetails()
}
async function reload() { await refresh(); tick.value++; if (selected.value) await loadRun(selected.value, offset.value) }
watch(repo, () => { before.value = ''; selected.value = ''; details.value = undefined; unavailable.value = false })
watch(range, () => { unavailable.value = false })
// Reports carry ISO times; show them in the reader's zone and name it.
const when = (iso?: string | null) => (iso ? new Date(iso).toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }) : '')
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Repository" title="Ingestion reports" lede="Exact file outcomes for each run, with exclusions, discovery limits and the evidence behind context estimates." :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Ingestion reports' }]">
      <button @click="reload">Refresh</button>
    </PageHeader>
    <p v-if="error" class="notice notice--danger" role="alert">{{ apiErrorMessage(error) }}</p>
    <p v-if="detailError" class="notice notice--danger" role="alert">{{ detailError }}</p>
    <p v-else-if="!data" class="muted" role="status">Loading reports…</p>
    <p v-if="data && !data.latest_attempt" class="empty">No ingestion report yet. Reports are recorded for new ingestion runs.</p>
    <template v-if="data?.latest_attempt">
      <section class="panel stack">
        <h2>Latest attempt and last successful run</h2>
        <p>Latest: <button class="btn--ghost mono" @click="loadRun(data.latest_attempt.report.run_id)">{{ data.latest_attempt.report.run_id }}</button> · {{ data.latest_attempt.report.status }} · {{ when(data.latest_attempt.report.started_at) }}</p>
        <p v-if="data.last_successful">Last successful: <button class="btn--ghost mono" @click="loadRun(data.last_successful.report.run_id)">{{ data.last_successful.report.run_id }}</button> · {{ when(data.last_successful.report.finished_at) }}</p>
        <p v-else class="muted">No confirmed successful run is recorded.</p>
        <NuxtLink :to="`/repos/${encodeURIComponent(repo)}/policy`">View indexing policy and annotations</NuxtLink>
      </section>
      <section v-if="report" class="panel stack">
        <h2>Run {{ report.run_id }}</h2>
        <p>{{ report.status }} · Graph commit: {{ report.graph_commit }} · Source revision: <code>{{ report.revision || 'unversioned' }}</code></p>
        <p class="small muted">Policy fingerprint: <code>{{ report.policy_digest || 'unavailable' }}</code> · Extractors: {{ report.extractor_version }}</p>
        <dl class="counts"><div v-for="[label, count] in counts" :key="label"><dt>{{ label }}</dt><dd>{{ fmtInt(count) }}</dd></div></dl>
        <p v-if="!report.finished_at" class="notice">This run is pending. Indexed counts require a confirmed graph commit.</p>
        <p :class="report.discovery_complete ? 'muted' : 'notice notice--warn'">Discovery {{ report.discovery_complete ? 'completed within its declared scope' : 'is incomplete' }}. {{ fmtInt(report.pruned_directories) }} directories were pruned; their descendants are not counted.</p>
        <h3>Whole-file skip reasons</h3>
        <ul v-if="Object.keys(report.skip_breakdown).length"><li v-for="(count, reason) in report.skip_breakdown" :key="reason">{{ reason }}: {{ fmtInt(count) }}</li></ul>
        <p v-else class="muted">No whole-file skips recorded.</p>
        <p class="small muted">Kubernetes-only exclusions appear in file stages when ordinary YAML metadata still contributes. Unsupported files remain unprocessed.</p>
        <h3>Estimated context tokens excluded</h3>
        <p>{{ report.estimated_tokens_saved_on_ingest === null ? 'Unknown' : fmtInt(report.estimated_tokens_saved_on_ingest) }}<span v-if="report.estimate.partial"> · partial estimate</span></p>
        <p class="muted">{{ report.estimate.explanation }} This is a hypothetical raw-text context estimate, not billed tokens or cloud savings.</p>
        <details><summary>Estimate provenance</summary><p>{{ report.estimate.baseline }}</p><p>{{ report.estimate.name }} v{{ report.estimate.version }} · {{ report.estimate.bytes_per_token }} bytes/token · {{ fmtInt(report.estimate.observed_metadata_bytes) }} metadata bytes · {{ fmtInt(report.estimate.covered_files) }} covered files · {{ fmtInt(report.estimate.unknown_files) }} unknown files.</p></details>
        <button v-if="!details" @click="loadRun(report.run_id)">Inspect file outcomes</button>
      </section>
      <section v-if="details" class="panel stack">
        <h2>File outcomes</h2>
        <div class="table-wrap"><table><thead><tr><th>File</th><th>Outcome</th><th>Reason</th><th>Stages</th></tr></thead><tbody><tr v-for="file in details.files" :key="file.path"><td class="mono">{{ file.path }}</td><td>{{ file.outcome }}</td><td>{{ file.skip_reason || file.diagnostic || '—' }}</td><td><span v-for="stage in file.stages" :key="stage.stage">{{ stage.stage }}: {{ stage.outcome }} {{ stage.diagnostic }}<br></span></td></tr></tbody></table></div>
        <div class="row"><button v-if="offset" @click="loadRun(selected)">First files</button><button v-if="details.next_offset" @click="loadRun(selected, details.next_offset)">Next files</button></div>
      </section>
      <section class="panel stack">
        <h2>Run history</h2>
        <div v-for="run in data.history" :key="run.report.run_id" class="row"><button class="btn--ghost mono" @click="loadRun(run.report.run_id)">{{ run.report.run_id }}</button><span>{{ run.report.status }} · {{ when(run.report.started_at) }}</span></div>
        <div class="row"><button v-if="before" @click="before = ''">Latest runs</button><button v-if="data.next" @click="before = data.next">Older runs</button></div>
        <p class="small muted">{{ data.annotation_notice }}</p>
      </section>
      <section class="panel stack">
        <h2>Processing across runs</h2>
        <p v-if="!data.latest_attempt.metric_coverage" class="notice notice--warn">Metric admission reached its limit. The reports above retain the exact counts.</p>
        <p v-else-if="metricLag || unavailable" class="notice notice--warn">Telemetry is unavailable or more than 90 seconds behind. Use the exact reports above.</p>
        <template v-if="data.latest_attempt.metric_coverage">
          <label>Period <select v-model="range"><option value="1h">1 hour</option><option value="24h">24 hours</option><option value="7d">7 days</option><option value="30d">30 days</option></select></label>
          <SeriesCard panel="ingest_files" :repo="repo" :range="range" :tick="tick" @unavailable="unavailable = true" />
          <SeriesCard panel="ingest_skips" :repo="repo" :range="range" :tick="tick" @unavailable="unavailable = true" />
          <SeriesCard panel="annotation_jobs" :range="range" :tick="tick" @unavailable="unavailable = true" />
        </template>
      </section>
    </template>
  </div>
</template>

<style scoped>
.counts { display: flex; flex-wrap: wrap; gap: 24px; }
.counts dd { margin: 4px 0 0; font-size: 1.4rem; font-family: var(--font-mono); }
.counts dt { color: var(--muted); }
code, td { overflow-wrap: anywhere; }
</style>
