<script setup lang="ts">
import type { Distribution, MetricsReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: m, error } = useApiGet<MetricsReport>(() => repoPath(repo.value, 'metrics'))
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const bars = (d?: Distribution) => (d?.buckets ?? []).map((b) => ({ label: b.label, value: b.count }))
const pkgSort = ref<'instability' | 'loc' | 'avg_complexity' | 'tested_pct'>('loc')
const packages = computed(() => [...(m.value?.packages ?? [])].sort((a, b) => (b[pkgSort.value] as number) - (a[pkgSort.value] as number)))
const short = (p: string) => p.replace(/^.*?\/internal\//, 'internal/').replace(/^github\.com\/[^/]+\/[^/]+\/?/, '') || p
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Measurements" title="Metrics & benchmark"
      lede="Computed from the full graph at ingest: size, complexity, test reachability, documentation, package coupling, and how many tokens the graph saves an agent."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Metrics' }]"
    >
      <span v-if="m" class="pill"><span class="dot dot--ok" />Computed {{ relTime(m.generated_at) }}</span>
    </PageHeader>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="m">
      <section class="panel panel--glass bench edge--green">
        <div class="panel__header">
          <div><p class="eyebrow eyebrow--green">Token benchmark</p><h2>Context reduced by {{ (m.benchmark.median_reduction_pct ?? 0).toFixed(1) }}% per question <span class="muted">· median</span></h2></div>
          <span class="badge badge--green">{{ m.benchmark.samples }} sampled functions</span>
        </div>
        <div class="grid grid--stats">
          <StatTile label="Graph answers" :value="fmtInt(m.benchmark.total_graph_tokens)" :hint="`p50 ${fmtInt(m.benchmark.graph_tokens.p50)} · p90 ${fmtInt(m.benchmark.graph_tokens.p90)} tokens`" />
          <StatTile label="Reading files" :value="fmtInt(m.benchmark.total_baseline_tokens)" :hint="`p50 ${fmtInt(m.benchmark.baseline_tokens.p50)} · p90 ${fmtInt(m.benchmark.baseline_tokens.p90)} tokens`" />
          <StatTile label="Reduced overall" :value="`${(m.benchmark.total_reduction_pct ?? 0).toFixed(1)}%`" :hint="`${fmtInt(m.benchmark.total_baseline_tokens - m.benchmark.total_graph_tokens)} tokens not spent across the sample`" />
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Symbol</th><th class="num">Files avoided</th><th class="num">Baseline</th><th class="num">Graph</th><th class="num">Reduced by</th></tr></thead>
            <tbody>
              <tr v-for="e in m.benchmark.examples" :key="e.symbol">
                <td class="mono">{{ e.symbol }}</td><td class="num">{{ e.files }}</td>
                <td class="num">{{ fmtInt(e.baseline_tokens) }}</td><td class="num">{{ fmtInt(e.graph_tokens) }}</td>
                <td class="num"><strong class="saving">{{ (e.reduction_pct ?? 0).toFixed(1) }}%</strong> <span class="dim">({{ e.savings_x.toFixed(1) }}×)</span></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="muted small">{{ m.benchmark.method }}</p>
      </section>

      <div class="grid grid--stats">
        <StatTile label="Code" :value="fmtInt(m.code.loc)" :hint="`LOC · ${m.code.packages} packages · ${m.code.files} files`" />
        <StatTile label="Callables" :value="fmtInt(m.code.functions + m.code.methods)" :hint="`${m.code.types} types · ${m.code.interfaces} interfaces`" />
        <StatTile label="Reached by tests" :value="`${m.tests.reachable_pct}%`" :hint="`${m.tests.reachable}/${m.tests.total} · ${m.tests.exported_untested} exported untested`" />
        <StatTile label="Doc coverage" :value="`${m.docs.coverage_pct}%`" :hint="`${m.docs.documented}/${m.docs.exported} exported · ${m.docs.documents} docs`" />
        <StatTile label="Avg complexity" :value="m.code.avg_complexity.toFixed(1)" :hint="`${m.code.complex_over_10} over 10 · ${m.code.complex_over_20} over 20`" />
        <StatTile label="Test / code" :value="m.tests.test_to_code_ratio.toFixed(2)" :hint="`${fmtInt(m.code.test_loc)} test LOC · ${m.tests.test_functions} tests`" />
        <StatTile label="Call edges" :value="fmtInt(m.code.call_edges)" :hint="`${m.code.security_symbols} security-sensitive functions`" />
        <StatTile label="Dead-code candidates" :value="m.dead_code.length" hint="unexported, no static callers" />
      </div>

      <div class="grid cols-2">
        <section v-for="(title, key) in { complexity: 'Cyclomatic complexity', loc: 'Function length (lines)', fan_in: 'Fan-in (callers)', fan_out: 'Fan-out (callees)' }" :key="key" class="panel">
          <p class="eyebrow">Distribution</p>
          <h2>{{ title }}</h2>
          <p class="muted small mono">p50 {{ m.distributions[key]?.p50 }} · p90 {{ m.distributions[key]?.p90 }} · p99 {{ m.distributions[key]?.p99 }} · max {{ m.distributions[key]?.max }} · mean {{ m.distributions[key]?.mean }}</p>
          <BarList :items="bars(m.distributions[key])" />
        </section>
      </div>

      <section class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Architecture</p><h2>Packages <span class="muted">· instability = Ce / (Ca + Ce)</span></h2></div>
          <div class="row">
            <button v-for="k in (['loc', 'instability', 'avg_complexity', 'tested_pct'] as const)" :key="k" class="chip" :class="{ active: pkgSort === k }" @click="pkgSort = k">{{ k.replace('_', ' ') }}</button>
          </div>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Package</th><th class="num">LOC</th><th class="num">Funcs</th><th class="num">Ca</th><th class="num">Ce</th><th>Instability</th><th class="num">Avg cx</th><th class="num">Tested</th><th class="num">Docs</th></tr></thead>
            <tbody>
              <tr v-for="p in packages" :key="p.name">
                <td class="mono">{{ short(p.name) }}</td>
                <td class="num">{{ fmtInt(p.loc) }}</td><td class="num">{{ p.functions }}</td>
                <td class="num">{{ p.afferent }}</td><td class="num">{{ p.efferent }}</td>
                <td><div class="meter"><span :style="{ width: `${p.instability * 100}%` }" /></div><span class="mono dim">{{ p.instability.toFixed(2) }}</span></td>
                <td class="num">{{ p.avg_complexity.toFixed(1) }}</td>
                <td class="num" :class="{ error: p.functions && p.tested_pct < 30 }">{{ p.tested_pct }}%</td>
                <td class="num">{{ p.doc_pct }}%</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <div class="grid cols-2">
        <section class="panel edge--amber">
          <p class="eyebrow">Test gaps</p><h2>Complex code no test reaches</h2>
          <div class="table-wrap"><table><tbody>
            <tr v-for="r in m.tests.untested_complex" :key="r.id"><td><NuxtLink :to="explore(r.id)" class="mono">{{ r.name }}</NuxtLink></td><td class="mono muted">{{ r.location }}</td></tr>
          </tbody></table></div>
          <p v-if="!m.tests.untested_complex.length" class="muted">Every function with complexity ≥ 5 is reached by a test.</p>
        </section>
        <section class="panel edge--violet">
          <p class="eyebrow">Cleanup</p><h2>Dead-code candidates</h2>
          <p class="muted small">Unexported, no static callers. Functions only used as values (callbacks, handlers) also show up here.</p>
          <div class="table-wrap"><table><tbody>
            <tr v-for="r in m.dead_code.slice(0, 25)" :key="r.id"><td><NuxtLink :to="explore(r.id)" class="mono">{{ r.name }}</NuxtLink></td><td class="mono muted">{{ r.location }}</td></tr>
          </tbody></table></div>
        </section>
      </div>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.bench { gap: 18px; }
.saving { color: var(--green); }
.small { font-size: 12.5px; }
.meter { width: 90px; display: inline-block; vertical-align: middle; margin-right: 8px; }
</style>
