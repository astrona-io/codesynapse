<script setup lang="ts">
import type { CleanupPlan } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Cleanup plan · ${repo.value}` })
const { data: plan, error } = useApiGet<CleanupPlan>(() => repoPath(repo.value, 'cleanup/plan'))

const GROUPS: Record<string, { label: string; kinds: string[] }> = {
  all: { label: 'Everything', kinds: [] },
  code: { label: 'Delete code', kinds: ['remove-risky-dead-code', 'remove-dead-code', 'remove-dead-file'] },
  deps: { label: 'Dependencies', kinds: ['remove-dependency', 'upgrade-dependency', 'resolve-licence'] },
  dupes: { label: 'Duplicates', kinds: ['merge-duplicates', 'review-edited-copies'] },
  tests: { label: 'Tests', kinds: ['test-hotspot'] },
}
const group = ref('all')
const steps = computed(() => (plan.value?.steps ?? []).filter((s) => group.value === 'all' || GROUPS[group.value]!.kinds.includes(s.kind)))
const KIND: Record<string, string> = {
  'remove-risky-dead-code': 'Delete risky dead code', 'remove-dead-code': 'Delete dead code', 'remove-dead-file': 'Delete a dead file',
  'remove-dependency': 'Remove a dependency', 'upgrade-dependency': 'Upgrade a dependency', 'resolve-licence': 'Licence conflict',
  'merge-duplicates': 'Merge copies', 'review-edited-copies': 'Review edited copies', 'test-hotspot': 'Add tests',
}
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const isCode = (id?: string) => !!id && id.includes('|Function|') || !!id && id.includes('|Method|')
const safetyClass = (s: string) => ({ high: 'badge--green', medium: 'badge--amber', low: 'badge--pink' }[s] ?? '')
const effortLabel = (e: number) => ({ 1: 'small', 2: 'medium', 3: 'large' }[e] ?? '')
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Code health" title="Cleanup plan"
      lede="The unused code, dependencies, duplicates and untested hotspots CodeSynapse found, in the order that pays off most for the least risk. Each step says why, where, and how to check it. Nothing is changed for you."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Code health' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>
    <CodeHealthTabs :repo="repo" />

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="plan">
      <div class="grid grid--stats">
        <StatTile label="Steps" :value="plan.summary.steps" hint="ranked below" />
        <StatTile label="Lines you could remove" :value="`up to ${fmtInt(plan.summary.lines_up_to)}`" hint="steps can overlap" />
        <StatTile label="Advisories fixed" :value="plan.summary.advisories" hint="by removing or upgrading" />
      </div>

      <section class="panel">
        <div class="panel__header">
          <h2>Steps <span class="muted">· {{ steps.length }}</span></h2>
          <div class="row">
            <button v-for="(g, id) in GROUPS" :key="id" class="chip" :class="{ active: group === id }" @click="group = id as string">{{ g.label }}</button>
          </div>
        </div>
        <ol v-if="steps.length" class="steps">
          <li v-for="(s, i) in steps" :key="s.id" class="step">
            <header class="row">
              <span class="rank">{{ i + 1 }}</span>
              <strong>{{ s.title }}</strong>
            </header>
            <div class="row meta">
              <span class="badge">{{ KIND[s.kind] ?? s.kind }}</span>
              <span class="badge" :class="safetyClass(s.safety)" :title="s.safety === 'high' ? 'Likely right without a close look; the build and tests confirm it' : s.safety === 'medium' ? 'Check it before you merge it' : 'Needs a person to decide'">{{ s.safety }} safety</span>
              <span class="badge" title="How much work">{{ effortLabel(s.effort) }} effort</span>
              <span class="muted small">value {{ s.value }} · priority {{ s.priority }}<template v-if="s.lines"> · {{ s.lines }} lines</template><template v-if="s.advisories"> · {{ s.advisories }} advisories</template></span>
            </div>
            <p>{{ s.why }}</p>
            <ul v-if="s.targets.length" class="targets">
              <li v-for="t in s.targets" :key="t.name + t.location">
                <NuxtLink v-if="isCode(t.id)" :to="explore(t.id!)" class="mono">{{ t.name }}</NuxtLink>
                <span v-else class="mono">{{ t.name }}</span>
                <span v-if="t.location" class="mono muted"> {{ t.location }}</span>
              </li>
            </ul>
            <p class="muted small"><strong>Check:</strong> {{ s.verify }} <NuxtLink v-if="s.link" :to="s.link">{{ s.group ? 'How to merge' : 'More detail' }}</NuxtLink></p>
          </li>
        </ol>
        <div v-else class="empty"><strong>Nothing to do here</strong>No steps for this filter.</div>
      </section>

      <section v-if="plan.notes.length" class="panel">
        <p class="eyebrow">Left out on purpose</p>
        <ul><li v-for="n in plan.notes" :key="n">{{ n }}</li></ul>
      </section>

      <section class="panel">
        <p class="eyebrow">How the order is decided</p>
        <p class="muted small">Priority = value × safety factor ÷ effort divisor. Value (0 to 100) is how much the step is worth: called vulnerabilities and risky dead code score highest, copies and tests lower. Safety factors are 1.0 for high, 0.8 for medium and 0.6 for low; effort divisors are 1.0, 1.5 and 2.0 for small, medium and large. Nothing here edits your code, and the plan is only as current as the last Sync.</p>
      </section>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.steps { list-style: none; padding: 0; margin: 0; display: grid; gap: 16px; }
.step { border-top: 1px solid var(--line); padding-top: 14px; display: grid; gap: 8px; }
.step p { margin: 0; }
.rank { display: inline-grid; place-items: center; min-width: 26px; height: 26px; border-radius: 50%; background: var(--accent); color: var(--on-accent); font-size: 13px; }
.meta { flex-wrap: wrap; gap: 8px; align-items: center; }
.targets { margin: 0; padding-left: 18px; display: grid; gap: 2px; }
.small { font-size: 12.5px; }
</style>
