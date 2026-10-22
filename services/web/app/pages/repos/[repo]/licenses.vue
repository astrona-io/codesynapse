<script setup lang="ts">
import type { LicenseCategory, LicenseReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: rep, error } = useApiGet<LicenseReport>(() => repoPath(repo.value, 'licenses'))

const verdict = ref<'all' | 'conflict' | 'review'>('all')
const shippedOnly = ref(false)
const issues = computed(() => (rep.value?.issues ?? []).filter((e) =>
  (verdict.value === 'all' || e.verdict.level === verdict.value) && (!shippedOnly.value || !e.dev)))

const CATEGORY: Record<LicenseCategory, { label: string; cls: string }> = {
  'public-domain': { label: 'public domain', cls: 'badge--green' },
  permissive: { label: 'permissive', cls: 'badge--green' },
  'weak-copyleft': { label: 'weak copyleft', cls: 'badge--amber' },
  'strong-copyleft': { label: 'strong copyleft', cls: 'badge--pink' },
  'network-copyleft': { label: 'network copyleft', cls: 'badge--pink' },
  restricted: { label: 'not open source', cls: 'badge--pink' },
  unknown: { label: 'unknown', cls: 'badge--violet' },
}
const cat = (c: LicenseCategory) => CATEGORY[c] ?? CATEGORY.unknown
const levelCls = (l: string) => ({ conflict: 'sev--critical', review: 'sev--medium', ok: 'sev--info' }[l] ?? '')
const maxCount = computed(() => Math.max(1, ...(rep.value?.by_license ?? []).map((c) => c.count)))
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Compliance" title="Dependency licences"
      lede="Every dependency's licence, checked against this repository's own. Conflicts mean you could not ship the code under your licence as it stands. Each verdict says why and where the licence was read; it is guidance, not legal advice."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Licences' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rep">
      <section class="panel" :class="rep.project.license ? 'edge--green' : 'edge--pink'">
        <p class="eyebrow">This repository</p>
        <h2 v-if="rep.project.license" class="mono">{{ rep.project.license }} <span class="badge" :class="cat(rep.category).cls">{{ cat(rep.category).label }}</span></h2>
        <h2 v-else>No licence found</h2>
        <p v-if="rep.project.license" class="muted small">Read from <span class="mono">{{ rep.project.source }}</span>.</p>
        <p v-else class="muted small">No LICENSE file, package.json or pyproject.toml licence. Without one, all rights are reserved and nobody may reuse the code; verdicts below treat the project as proprietary.</p>
      </section>

      <div class="grid grid--stats">
        <StatTile label="Dependencies" :value="fmtInt(rep.total)" :hint="rep.online ? 'licences from disk and deps.dev' : 'licences from disk only'" />
        <StatTile label="Conflicts" :value="rep.conflict" hint="cannot ship under your licence" />
        <StatTile label="To review" :value="rep.review" hint="weak copyleft, dev-only conflicts, unknown" />
        <StatTile label="No licence found" :value="rep.unknown" :hint="rep.online ? 'not on disk or deps.dev' : 'enable CODESYNAPSE_LICENSE_LOOKUP'" />
      </div>

      <section class="panel edge--orange">
        <div class="panel__header">
          <div><p class="eyebrow">Needs attention</p><h2>Conflicts and reviews <span class="muted">· {{ issues.length }}</span></h2></div>
        </div>
        <div class="row">
          <span class="label">Verdict</span>
          <button v-for="v in (['all', 'conflict', 'review'] as const)" :key="v" class="chip" :class="{ active: verdict === v }" @click="verdict = v">{{ v }}</button>
          <label class="row compact"><input v-model="shippedOnly" type="checkbox"> hide development dependencies</label>
        </div>
        <div v-if="issues.length" class="table-wrap">
          <table>
            <thead><tr><th>Verdict</th><th>Dependency</th><th>Licence</th><th>Why</th></tr></thead>
            <tbody>
              <tr v-for="e in issues" :key="`${e.ecosystem}|${e.name}|${e.version}`">
                <td><span class="sev" :class="levelCls(e.verdict.level)">{{ e.verdict.level }}</span></td>
                <td>
                  <span class="mono">{{ e.name }}</span> <span class="mono muted">{{ e.version }}</span>
                  <div class="muted small">{{ e.ecosystem }} · {{ e.direct ? 'direct' : 'indirect' }}<template v-if="e.dev"> · dev only</template></div>
                </td>
                <td>
                  <span class="mono">{{ e.license || 'unknown' }}</span>
                  <div><span class="badge" :class="cat(e.category).cls">{{ cat(e.category).label }}</span></div>
                  <div v-if="e.source" class="muted small">from <span class="mono">{{ e.source }}</span></div>
                </td>
                <td class="small">{{ e.verdict.reason }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>Nothing to look at</strong>{{ rep.issues.length ? 'for the current filter.' : 'Every dependency licence fits this repository’s licence.' }}</div>
        <p v-if="rep.truncated" class="muted small">{{ rep.truncated }} more not listed; run <code>codesynapse licenses</code> for the full list.</p>
      </section>

      <section class="panel">
        <p class="eyebrow">Inventory</p><h2>Licences in use</h2>
        <ul class="bars">
          <li v-for="c in rep.by_license" :key="c.license">
            <span class="mono name">{{ c.license }}</span>
            <span class="badge" :class="cat(c.category).cls">{{ cat(c.category).label }}</span>
            <span class="bar"><span :style="{ width: `${(c.count / maxCount) * 100}%` }" :class="`fill--${cat(c.category).cls.replace('badge--', '')}`" /></span>
            <span class="num">{{ c.count }}</span>
          </li>
        </ul>
      </section>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.compact { gap: 6px; }
.small { font-size: 12.5px; margin-top: 4px; }
.bars { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
.bars li { display: grid; grid-template-columns: minmax(120px, 1.2fr) auto 2fr 48px; gap: 12px; align-items: center; }
.bars .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bar { height: 6px; border-radius: 3px; background: var(--line); overflow: hidden; }
.bar > span { display: block; height: 100%; }
.fill--green { background: var(--green); }
.fill--amber { background: var(--amber); }
.fill--pink { background: var(--pink); }
.fill--violet { background: var(--violet); }
.num { text-align: right; }
@media (max-width: 640px) {
  .bars li { grid-template-columns: 1fr auto 48px; }
  .bars .bar { grid-column: 1 / -1; }
}
</style>
