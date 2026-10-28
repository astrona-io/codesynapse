<script setup lang="ts">
import type { FindingTriageList, ImageScan, RuleGuide, SecFinding, SecurityReport, TriageState } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: rep, error, refresh } = useApiGet<SecurityReport>(() => repoPath(repo.value, 'security'))
// Answers 409 without accounts; then nobody sees the triage action.
const { data: triage, refresh: refreshTriage } = useApiGet<FindingTriageList>(() => repoPath(repo.value, 'triage'))
// 404 until CI uploads a scan (codesynapse images upload): the panel stays hidden.
const { data: images } = useApiGet<ImageScan>(() => repoPath(repo.value, 'images'))
const { data: catalog } = useApiGet<{ rules: RuleGuide[] }>('rules')
const guides = computed(() => new Map((catalog.value?.rules ?? []).map((g) => [g.rule, g])))
const ruleFilter = ref('')
const open = ref(new Set<string>())
const toggle = (id: string) => {
  const next = new Set(open.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  open.value = next
}
const findingsEl = ref<HTMLElement | null>(null)
function showRule(rule: string) {
  ruleFilter.value = ruleFilter.value === rule ? '' : rule
  sevFilter.value = 'all'
  nextTick(() => findingsEl.value?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
}
function showOwner(owner: string) {
  const key = owner || NO_OWNER
  ownerFilter.value = ownerFilter.value === key ? '' : key
  sevFilter.value = 'all'
  nextTick(() => findingsEl.value?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
}
const filesByRule = computed(() => {
  const m = new Map<string, Set<string>>()
  for (const f of rep.value?.findings ?? []) {
    if (!m.has(f.rule)) m.set(f.rule, new Set())
    m.get(f.rule)!.add(f.location.split(':')[0]!)
  }
  return m
})
const langMatch = (a?: string, b?: string) => {
  // Languages that share examples: the JS family, and Kotlin with Java.
  const n = (l?: string) => (['javascript', 'typescript', 'vue', 'js', 'ts'].includes(l ?? '') ? 'typescript' : l === 'yml' ? 'yaml' : l === 'kotlin' ? 'java' : l)
  return n(a) === n(b)
}
function examplesFor(f: SecFinding) {
  const g = guides.value.get(f.rule)
  if (!g) return []
  const match = g.examples.filter((e) => langMatch(e.language, f.language))
  return match.length ? match : g.examples.slice(0, 1)
}

const SEVERITIES = ['critical', 'high', 'medium', 'low', 'info']
const sevFilter = ref<string>('all')
const reachFilter = ref<string>('all')
const exposedOnly = ref(false)
// A CODEOWNERS owner, or NO_OWNER for files no rule owns.
const NO_OWNER = '-'
const ownerFilter = ref('')
const ownsFinding = (f: SecFinding) =>
  !ownerFilter.value || (ownerFilter.value === NO_OWNER ? !f.code_owners?.length : !!f.code_owners?.includes(ownerFilter.value))
// ?q= comes from the top bar's search, which opens a finding here.
const q = ref(typeof route.query.q === 'string' ? route.query.q : '')

const findings = computed(() =>
  (rep.value?.findings ?? []).filter((f) =>
    (sevFilter.value === 'all' || f.severity === sevFilter.value) &&
    (!ruleFilter.value || f.rule === ruleFilter.value) &&
    (!exposedOnly.value || f.exposed) &&
    ownsFinding(f) &&
    (!q.value || `${f.rule} ${f.location} ${f.owner?.name ?? ''} ${f.code_owners?.join(' ') ?? ''} ${f.detail}`.toLowerCase().includes(q.value.toLowerCase())),
  ),
)
const vulns = computed(() =>
  (rep.value?.vulnerabilities ?? []).filter((v) =>
    (sevFilter.value === 'all' || v.severity === sevFilter.value) &&
    (reachFilter.value === 'all' || v.reachability === reachFilter.value),
  ),
)
const total = (m: Record<string, number> | undefined) => Object.values(m ?? {}).reduce((a, b) => a + b, 0)
const gradeColor = computed(() => ({ A: 'var(--success)', B: 'var(--info)', C: 'var(--warning)', D: 'var(--accent)', F: 'var(--danger)', 'N/A': 'var(--muted)' }[rep.value?.grade ?? 'A']))
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const reachClass = (r: string) => ({ called: 'badge--pink', imported: 'badge--amber', required: '' }[r] ?? '')

// Triage: an editor records why a finding no longer counts. The API checks
// the role again; the flag only decides whether the action is shown.
const TRIAGE_LABELS: Record<TriageState, string> = { accepted_risk: 'Accepted risk', false_positive: 'False positive', fixed_by: 'Fixed by' }
const canTriage = computed(() => !!triage.value?.can_edit)
const triaging = ref('')
const triageForm = reactive({ state: 'accepted_risk' as TriageState, reason: '', reference: '', expires: '' })
const triageError = ref('')
const triageBusy = ref(false)
function startTriage(f: SecFinding) {
  triaging.value = triaging.value === f.id ? '' : f.id
  Object.assign(triageForm, { state: 'accepted_risk', reason: '', reference: '', expires: '' })
  triageError.value = ''
}
async function saveTriage(f: SecFinding) {
  triageBusy.value = true
  triageError.value = ''
  try {
    await apiPut(repoPath(repo.value, 'triage'), {
      finding_id: f.id, state: triageForm.state, reason: triageForm.reason,
      reference: triageForm.state === 'fixed_by' ? triageForm.reference : undefined,
      expires_at: triageForm.expires ? new Date(`${triageForm.expires}T23:59:59`).toISOString() : undefined,
    })
    triaging.value = ''
    await Promise.all([refresh(), refreshTriage()])
  } catch (err) {
    triageError.value = apiErrorMessage(err)
  } finally {
    triageBusy.value = false
  }
}
async function clearTriage(fingerprint: string) {
  triageError.value = ''
  try {
    await apiDelete(repoPath(repo.value, `triage/${fingerprint}`))
    await Promise.all([refresh(), refreshTriage()])
  } catch (err) {
    triageError.value = apiErrorMessage(err)
  }
}
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Security review" title="Threats & vulnerabilities"
      lede="Code findings from type-checked rules and known CVEs in dependencies, ranked by severity, exposure from entry points and test reachability."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Security' }]"
    >
      <SyncControl :repo="repo" />
      <a class="btn btn--ghost" :href="apiPath(repoPath(repo, 'security')) + '?format=markdown'" target="_blank" rel="noopener">Markdown report</a>
    </PageHeader>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rep">
      <div class="grid grid--stats">
        <div class="panel stat-card grade">
          <p class="eyebrow">Security grade</p>
          <p class="stat__value" :style="{ color: gradeColor }">{{ rep.grade }} <span v-if="rep.grade !== 'N/A'" class="score">{{ rep.score }}/100</span></p>
          <p class="stat__hint">{{ rep.grade === 'N/A' ? 'Nothing here is covered by the rules yet' : 'Weighted by severity, exposure and reachability' }}</p>
        </div>
        <StatTile label="Code findings" :value="total(rep.finding_counts)" :hint="SEVERITIES.filter((s) => rep!.finding_counts[s]).map((s) => `${rep!.finding_counts[s]} ${s}`).join(' · ') || 'none'" />
        <StatTile label="Vulnerabilities" :value="total(rep.vuln_counts)" :hint="SEVERITIES.filter((s) => rep!.vuln_counts[s]).map((s) => `${rep!.vuln_counts[s]} ${s}`).join(' · ') || 'none'" />
        <StatTile label="Dependencies" :value="rep.modules.total" :hint="`${rep.modules.direct} direct · ${rep.modules.vulnerable} vulnerable${rep.modules.stdlib ? ` · Go ${rep.modules.stdlib.replace(/^v/, '')}` : ''}`" />
      </div>

      <section v-if="rep.checked?.length || rep.not_checked?.length || rep.head" class="panel coverage">
        <p class="eyebrow">Coverage</p>
        <div class="grid cols-2">
          <div>
            <p class="label">Checked</p>
            <ul><li v-for="c in rep.checked ?? []" :key="c">{{ c }}</li></ul>
            <p v-if="!rep.checked?.length" class="muted small">Re-sync the repository to see what the rules checked.</p>
          </div>
          <div>
            <p class="label">Not checked: review by hand</p>
            <ul><li v-for="c in rep.not_checked ?? []" :key="c">{{ c }}</li></ul>
            <p v-if="!rep.not_checked?.length" class="muted small">Every file type found is covered.</p>
          </div>
        </div>
        <p v-if="rep.head" class="muted small">Indexed at commit <span class="mono">{{ rep.head.slice(0, 10) }}</span> · {{ relTime(rep.indexed_at) }}. If your checkout moved on, sync before relying on this report.<template v-if="rep.unanalysed_findings"> {{ rep.unanalysed_findings }} more findings count in the grade but are not listed.</template></p>
      </section>

      <section v-if="rep.suppressed?.length" class="panel stack">
        <h2>Approved exceptions and triage · {{ rep.suppressed.length }}</h2>
        <p class="muted">These findings remain stored. This organisation's approved policy or its triage decisions exclude them from this report's active findings and grade.</p>
        <p v-if="triageError && !triaging" class="error">{{ triageError }}</p>
        <article v-for="finding in rep.suppressed" :key="finding.id" class="suppressed">
          <div>
            <strong>{{ finding.title }}</strong>
            <span v-if="finding.triage" class="badge badge--cyan">{{ TRIAGE_LABELS[finding.triage.state] ?? finding.triage.state }}<template v-if="finding.triage.reference"> {{ finding.triage.reference }}</template></span>
            <span v-else class="badge badge--violet">Approved policy</span>
          </div>
          <p class="small mono">{{ finding.location }} · {{ finding.rule }}</p>
          <p>{{ finding.triage ? finding.triage.reason : finding.suppression_reason }}</p>
          <p v-if="finding.triage" class="small muted">
            {{ finding.triage.by || 'Someone' }} · {{ relTime(finding.triage.at) }}<template v-if="finding.triage.expires_at"> · until {{ new Date(finding.triage.expires_at).toLocaleDateString() }}</template>
            <button v-if="canTriage && finding.fingerprint" class="secondary tiny clear" @click="clearTriage(finding.fingerprint)">Clear triage</button>
          </p>
        </article>
      </section>

      <div class="panel filters">
        <div class="row">
          <span class="label">Severity</span>
          <button class="chip" :class="{ active: sevFilter === 'all' }" @click="sevFilter = 'all'">All</button>
          <button v-for="s in SEVERITIES" :key="s" class="chip" :class="{ active: sevFilter === s }" @click="sevFilter = s">{{ s }}</button>
        </div>
        <div class="row">
          <span class="label">Reach</span>
          <button v-for="r in ['all', 'called', 'imported', 'required']" :key="r" class="chip" :class="{ active: reachFilter === r }" @click="reachFilter = r">{{ r }}</button>
          <label class="row compact"><input v-model="exposedOnly" type="checkbox"> exposed findings only</label>
          <input v-model="q" type="search" placeholder="Filter findings…" class="filter-input" aria-label="Filter findings">
        </div>
      </div>

      <section class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Dependencies</p><h2>Vulnerable modules <span class="muted">· {{ vulns.length }}</span></h2></div>
        </div>
        <div v-if="vulns.length" class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>Advisory</th><th>Module</th><th>Version → fix</th><th>Reach</th><th>Summary</th></tr></thead>
            <tbody>
              <tr v-for="v in vulns" :key="`${v.module}${v.id}`">
                <td><SevBadge :severity="v.severity" /></td>
                <td>
                  <a :href="v.url" target="_blank" rel="noopener noreferrer" class="mono">{{ v.id }}</a>
                  <div v-for="c in v.cve" :key="c" class="mono muted">{{ c }}</div>
                  <div v-if="v.cwe?.length" class="mono dim">{{ v.cwe.join(' ') }}</div>
                </td>
                <td class="mono">{{ v.module }}</td>
                <td class="mono"><span class="muted">{{ v.version }}</span> → <span :class="v.fixed ? 'fix' : 'error'">{{ v.fixed || 'no fix' }}</span></td>
                <td>
                  <span class="badge" :class="reachClass(v.reachability)">{{ v.reachability }}</span>
                  <div v-for="s in v.call_sites ?? []" :key="s.id" class="callsite"><NuxtLink :to="explore(s.id)" class="mono">{{ s.name }}</NuxtLink></div>
                </td>
                <td>{{ v.summary }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>No known vulnerabilities</strong>{{ rep.modules.total ? 'for the current filter.' : 'Ingest with vulnerability scanning enabled (CODESYNAPSE_VULN_SCAN=true).' }}</div>
      </section>

      <section v-if="images" class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Container images</p><h2>Base-image vulnerabilities <span class="muted">· {{ images.total }}</span></h2></div>
          <p class="muted">{{ images.images.join(', ') }} · scanned {{ new Date(images.uploaded_at).toLocaleDateString() }}</p>
        </div>
        <div v-if="images.findings.length" class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>Advisory</th><th>Package</th><th>Version → fix</th><th>Image</th></tr></thead>
            <tbody>
              <tr v-for="f in images.findings" :key="`${f.image}${f.package}${f.version}${f.id}`">
                <td><SevBadge :severity="f.severity" /></td>
                <td class="mono">{{ f.id }}</td>
                <td class="mono">{{ f.package }}</td>
                <td class="mono"><span class="muted">{{ f.version }}</span> → <span :class="f.fixed ? 'fix' : 'error'">{{ f.fixed || 'no fix' }}</span></td>
                <td class="mono">{{ f.image }}</td>
              </tr>
            </tbody>
          </table>
          <p v-if="images.truncated" class="muted">Showing the worst {{ images.findings.length }} of {{ images.total }}.</p>
        </div>
        <div v-else class="empty"><strong>No known vulnerabilities in the scanned images</strong></div>
      </section>

      <section v-if="rep.owners?.length" class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">CODEOWNERS</p><h2>Findings by owner</h2></div>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Owner</th><th>By severity</th><th class="num">Findings</th></tr></thead>
            <tbody>
              <tr v-for="o in rep.owners" :key="o.owner" class="clickable" :class="{ selected: ownerFilter === (o.owner || NO_OWNER) }" tabindex="0"
                  @click="showOwner(o.owner)" @keydown.enter="showOwner(o.owner)">
                <td><a href="#" class="mono" @click.prevent>{{ o.owner || 'no owner' }}</a></td>
                <td><span v-for="s in SEVERITIES.filter((s) => o.counts[s])" :key="s" class="owner-sev"><SevBadge :severity="s" /> {{ o.counts[s] }}</span></td>
                <td class="num">{{ o.findings }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section ref="findingsEl" class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Source code & config</p><h2>Findings <span class="muted">· {{ findings.length }}</span></h2></div>
          <button v-if="ruleFilter" class="chip active" @click="ruleFilter = ''">rule: {{ ruleFilter }} ✕</button>
          <button v-if="ownerFilter" class="chip active" @click="ownerFilter = ''">owner: {{ ownerFilter === NO_OWNER ? 'none' : ownerFilter }} ✕</button>
        </div>
        <div v-if="findings.length" class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>Rule</th><th>Where</th><th>Exposed</th><th>Tested</th><th>Detail</th><th /></tr></thead>
            <tbody>
              <template v-for="f in findings" :key="f.id">
              <tr>
                <td><SevBadge :severity="f.severity" /></td>
                <td><span class="mono">{{ f.rule }}</span><div class="mono dim">{{ f.cwe?.join(' ') }}</div></td>
                <td>
                  <NuxtLink v-if="f.owner && f.owner.kind !== 'File'" :to="explore(f.owner.id)" class="mono">{{ f.owner.name }}</NuxtLink>
                  <div class="mono muted">{{ f.location }}</div>
                  <div v-if="f.code_owners?.length" class="mono dim">{{ f.code_owners.join(' ') }}</div>
                </td>
                <td><span class="badge" :class="f.exposed ? 'badge--pink' : ''">{{ f.exposed ? 'yes' : 'no' }}</span></td>
                <td><span class="badge" :class="f.tested ? 'badge--green' : 'badge--amber'">{{ f.tested ? 'yes' : 'no' }}</span></td>
                <td class="detail">{{ f.detail }}<div v-if="f.ignore_request" class="small muted" title="A codesynapse:ignore comment asks to suppress this finding. It stays active until the organisation approves inline ignores for this repository.">Ignore requested in code: {{ f.ignore_request }}</div></td>
                <td class="actions">
                  <button class="secondary tiny" :aria-expanded="open.has(f.id)" @click="toggle(f.id)">{{ open.has(f.id) ? 'Hide fix' : 'How to fix' }}</button>
                  <button v-if="canTriage" class="secondary tiny" :aria-expanded="triaging === f.id" @click="startTriage(f)">Triage</button>
                </td>
              </tr>
              <tr v-if="triaging === f.id" class="fix-row">
                <td colspan="7">
                  <form class="triage" @submit.prevent="saveTriage(f)">
                    <label>Decision
                      <select v-model="triageForm.state">
                        <option v-for="st in triage?.states ?? []" :key="st" :value="st">{{ TRIAGE_LABELS[st] ?? st }}</option>
                      </select>
                    </label>
                    <label v-if="triageForm.state === 'fixed_by'">Commit or pull request
                      <input v-model="triageForm.reference" required placeholder="#412 or a commit SHA" maxlength="64">
                    </label>
                    <label>Expires (optional)<input v-model="triageForm.expires" type="date"></label>
                    <label class="wide">Why
                      <input v-model="triageForm.reason" required maxlength="1000" placeholder="What makes this safe, or what fixed it">
                    </label>
                    <p v-if="triageError" class="error wide">{{ triageError }}</p>
                    <div class="row wide">
                      <button type="submit" :disabled="triageBusy">Save decision</button>
                      <button type="button" class="secondary" @click="triaging = ''">Cancel</button>
                    </div>
                  </form>
                </td>
              </tr>
              <tr v-if="open.has(f.id)" class="fix-row">
                <td colspan="7">
                  <div class="fix">
                    <p v-if="guides.get(f.rule)?.why"><strong>Why it matters.</strong> {{ guides.get(f.rule)!.why }}</p>
                    <p><strong>Fix.</strong> {{ guides.get(f.rule)?.fix }}</p>
                    <div v-for="(ex, i) in examplesFor(f)" :key="i" class="pair">
                      <div><p class="label bad">Vulnerable · {{ ex.language }}</p><pre class="code bad-code">{{ ex.vulnerable }}</pre></div>
                      <div><p class="label good">Fixed · {{ ex.language }}</p><pre class="code good-code">{{ ex.fixed }}</pre></div>
                    </div>
                  </div>
                </td>
              </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>No findings</strong>for the current filter.</div>
      </section>

      <div class="grid cols-2">
        <section class="panel">
          <p class="eyebrow">Rules</p><h2>What fired</h2>
          <div class="table-wrap">
            <table>
              <tbody>
                <tr v-for="r in rep.rules" :key="r.rule" class="clickable" :class="{ selected: ruleFilter === r.rule }" tabindex="0" @click="showRule(r.rule)" @keydown.enter="showRule(r.rule)">
                  <td><SevBadge :severity="r.severity" /></td>
                  <td><a href="#" @click.prevent>{{ r.title }}</a><div class="mono dim">{{ r.rule }} · {{ filesByRule.get(r.rule)?.size ?? 0 }} file(s)</div></td>
                  <td class="num">{{ r.count }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="!rep.rules.length" class="muted">No rule fired.</p>
        </section>
        <section class="panel">
          <p class="eyebrow">Attack surface</p><h2>Functions calling sensitive APIs</h2>
          <div class="table-wrap">
            <table>
              <tbody>
                <tr v-for="s in rep.sensitive_functions ?? []" :key="s.id">
                  <td><NuxtLink :to="explore(s.id)" class="mono">{{ s.name }}</NuxtLink><div class="mono muted">{{ s.location }}</div></td>
                  <td><span v-for="t in (s.summary ?? '').split(', ')" :key="t" class="tag sec">{{ t }}</span></td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.grade .score { font-size: 15px; color: var(--muted); font-weight: 500; }
.coverage ul { margin: 0; padding-left: 18px; display: grid; gap: 4px; font-size: 13px; }
.coverage .label { display: block; margin-bottom: 6px; }
.small { font-size: 12.5px; }
.filters { gap: 10px; padding: 16px 20px; }
.filter-input { max-width: 280px; margin-left: auto; }
.compact { gap: 6px; }
.fix { color: var(--success); }
.callsite { margin-top: 4px; font-size: 12px; }
.owner-sev { display: inline-flex; align-items: center; gap: 4px; margin-right: 10px; white-space: nowrap; }
.detail { max-width: 460px; color: var(--muted); font-size: 13px; }
.clickable { cursor: pointer; }
.clickable.selected td { background: var(--accent-soft); }
.fix-row td { background: var(--panel-2); }
.fix { display: grid; gap: 10px; padding: 6px 4px 10px; max-width: 1100px; }
.pair { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 360px), 1fr)); gap: 12px; }
.code { margin: 4px 0 0; padding: 10px 12px; border-radius: var(--radius-xs); font: 12.5px/1.5 var(--font-mono); white-space: pre-wrap; word-break: break-word; background: var(--bg); border: 1px solid var(--line); }
.bad-code { border: 2px solid var(--danger); }
.good-code { border: 2px solid var(--success); }
.label.bad { color: var(--danger); }
.label.good { color: var(--success); }
.actions { display: flex; gap: 6px; flex-wrap: wrap; }
.suppressed .badge { margin-left: 8px; }
.suppressed .clear { margin-left: 10px; }
.triage { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 220px), 1fr)); gap: 10px 14px; padding: 6px 4px 10px; max-width: 1100px; }
.triage label { display: grid; gap: 4px; font-size: 13px; }
.triage .wide { grid-column: 1 / -1; }
</style>
