<script setup lang="ts">
import type { CleanupReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Unused code · ${repo.value}` })
const { data: rep, error } = useApiGet<CleanupReport>(() => repoPath(repo.value, 'cleanup'))
const category = ref('all')
const minConf = ref('low')
const securityOnly = ref(false)
const rank: Record<string, number> = { low: 0, medium: 1, high: 2 }
const items = computed(() => (rep.value?.items ?? []).filter((i) =>
  (category.value === 'all' || i.category === category.value) && rank[i.confidence]! >= rank[minConf.value]! && (!securityOnly.value || i.security?.length)))
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const confClass = (c: string) => ({ high: 'badge--green', medium: 'badge--amber', low: '' }[c] ?? '')
const steps = [
  'Start with security-relevant items: dead code that calls exec, SQL, crypto or a vulnerable symbol is pure risk.',
  'Remove high-confidence items first (unexported functions); the compiler confirms the rest still links.',
  'For exported or low-confidence items, check reflection, plugins, go:linkname and external consumers before deleting.',
  'Delete unused go.mod requirements with go mod tidy, then re-run the security review to confirm the CVEs are gone.',
  'Test-only code: delete it with its tests, or move it into _test.go files so it is not shipped.',
  'Run the tests, press Sync, and check that the counts went down and nothing became unreachable unexpectedly.',
]
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Code health" title="Unused code"
      lede="Code, dependencies and config nothing uses. Dead code is still compiled, shipped, reviewed and patched — and can be the only thing pulling in a vulnerable dependency. Remove it."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Code health' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>
    <CodeHealthTabs :repo="repo" />

    <p v-if="error" class="notice notice--danger" role="alert">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rep">
      <div class="grid grid--stats">
        <StatTile label="Unused functions" :value="fmtInt(rep.summary.unreachable + rep.summary.test_only)" :hint="`${fmtInt(rep.summary.unreachable)} unreachable · ${fmtInt(rep.summary.test_only)} test-only · of ${fmtInt(rep.summary.functions)}`" />
        <StatTile label="Removable lines" :value="fmtInt(rep.summary.dead_loc)" :hint="`${rep.summary.dead_pct}% of function code · ${plural(rep.summary.dead_files, 'whole file')}`" />
        <StatTile label="Unused dependencies" :value="fmtInt(rep.summary.unused_modules)" :hint="`${plural(rep.summary.cves_removable, 'CVE')} removed with them`" />
        <StatTile label="Security-relevant" :value="fmtInt(rep.summary.security_relevant)" hint="dead code with findings or sensitive calls" />
      </div>

      <section class="panel">
        <p class="eyebrow eyebrow--green">How to clean up safely</p>
        <ol class="steps"><li v-for="s in steps" :key="s">{{ s }}</li></ol>
      </section>

      <section v-if="rep.modules.length" class="panel">
        <p class="eyebrow">Supply chain</p><h2>Dependencies nothing imports</h2>
        <div class="table-wrap"><table><tbody>
          <tr v-for="m in rep.modules" :key="`${m.ecosystem ?? 'Go'}:${m.path}`">
            <td class="mono">{{ m.path }}<span v-if="m.ecosystem" class="muted"> ({{ m.ecosystem }})</span></td><td class="mono muted">{{ m.version }}</td>
            <td><span v-for="v in m.vulnerabilities ?? []" :key="v" class="tag sec mono">{{ v }}</span><span v-if="!m.vulnerabilities?.length" class="muted">no known CVEs</span></td>
          </tr>
        </tbody></table></div>
      </section>

      <section class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Code</p><h2>Unused functions <span class="muted">· {{ items.length }}</span></h2></div>
        </div>
        <div class="row">
          <span class="label">Category</span>
          <button v-for="c in ['all', 'unreachable', 'test-only']" :key="c" class="chip" :class="{ active: category === c }" :aria-pressed="category === c" @click="category = c">{{ c }}</button>
          <span class="label">Confidence ≥</span>
          <button v-for="c in ['low', 'medium', 'high']" :key="c" class="chip" :class="{ active: minConf === c }" :aria-pressed="minConf === c" @click="minConf = c">{{ c }}</button>
          <label class="row compact"><input v-model="securityOnly" type="checkbox"> security-relevant only</label>
        </div>
        <div v-if="items.length" class="table-wrap">
          <table>
            <thead><tr><th>Symbol</th><th>Category</th><th>Confidence</th><th class="num">LOC</th><th>Why remove</th></tr></thead>
            <tbody>
              <tr v-for="i in items" :key="i.id">
                <td><NuxtLink :to="explore(i.id)" class="mono">{{ i.name }}</NuxtLink><div class="mono muted">{{ i.location }}</div></td>
                <td><span class="badge" :class="i.category === 'unreachable' ? 'badge--pink' : 'badge--violet'">{{ i.category }}</span></td>
                <td><span class="badge" :class="confClass(i.confidence)">{{ i.confidence }}</span></td>
                <td class="num">{{ fmtInt(i.loc) }}</td>
                <td>
                  <span v-for="s in i.security ?? []" :key="s" class="tag sec">{{ s }}</span>
                  <div class="muted small">{{ i.reason }}</div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>Nothing to remove</strong>for the current filter.</div>
      </section>

      <div class="grid cols-2">
        <section class="panel">
          <p class="eyebrow">Files</p><h2>Files with nothing live</h2>
          <div class="table-wrap"><table><tbody>
            <tr v-for="f in rep.files" :key="f.file"><td class="mono">{{ f.file }}</td><td class="num">{{ plural(f.symbols, 'symbol') }}</td><td class="num">{{ plural(f.loc, 'line') }}</td></tr>
          </tbody></table></div>
          <p v-if="!rep.files.length" class="muted">Every file has live code.</p>
        </section>
        <section class="panel">
          <p class="eyebrow">Config</p><h2>Helm values & template defines nobody uses</h2>
          <div class="table-wrap"><table><tbody>
            <tr v-for="v in rep.unused_values" :key="v.id"><td><span class="badge badge--cyan">value</span></td><td class="mono">{{ v.name }}</td><td class="mono muted">{{ v.location }}</td></tr>
            <tr v-for="d in rep.unused_template_defines" :key="d.id"><td><span class="badge badge--violet">define</span></td><td class="mono">{{ d.name }}</td><td class="mono muted">{{ d.location }}</td></tr>
          </tbody></table></div>
          <p v-if="!rep.unused_values.length && !rep.unused_template_defines.length" class="muted">No unused Helm values or template defines.</p>
          <p v-if="rep.test_support_packages.length" class="muted small">Test-support packages (only tests use them, not counted): {{ rep.test_support_packages.join(', ') }}</p>
        </section>
      </div>
    </template>
    <p v-else class="muted" role="status">Loading…</p>
  </div>
</template>

<style scoped>
.steps { margin: 0; padding-left: 20px; display: grid; gap: 6px; }
.compact { gap: 6px; }
.small { font-size: 12.5px; margin-top: 4px; }
</style>
