<script setup lang="ts">
import type { FeatureHealth, FeatureHealthStats, FeatureTiming, FeatureWindow } from '~/utils/types'

// Feature health: how long the optional features (the self-hosted model's
// work and the plugin's hooks) and the tools took, per feature and client,
// from the hourly usage buckets. The window is the page's own.
const WINDOWS: { id: FeatureWindow; label: string }[] = [
  { id: '1h', label: 'last hour' }, { id: '24h', label: '24 hours' }, { id: '7d', label: '7 days' },
]
const route = useRoute()
const router = useRouter()
const isWindow = (v: unknown): v is FeatureWindow => WINDOWS.some((w) => w.id === v)
const win = ref<FeatureWindow>(isWindow(route.query.window) ? route.query.window : '24h')
watch(win, (w) => router.replace({ query: { ...route.query, window: w } }))
const { data, pending, error } = useApiGet<FeatureHealthStats>(() => `stats/features?window=${win.value}`)

const KIND_LABEL: Record<FeatureHealth['kind'], string> = { model: 'Self-hosted model', hook: 'Plugin hooks', tool: 'Tools' }
const groups = computed(() => (['model', 'hook', 'tool'] as const)
  .map((kind) => ({ kind, label: KIND_LABEL[kind], features: (data.value?.features ?? []).filter((f) => f.kind === kind) }))
  .filter((g) => g.features.length > 0))
const open = ref<string[]>([])
const toggle = (f: string) => { open.value = open.value.includes(f) ? open.value.filter((x) => x !== f) : [...open.value, f] }

const dur = (ms: number) => (ms < 1000 ? `${ms} ms` : ms < 10_000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms / 1000)} s`)
const share = (n: number, of: number) => (of > 0 ? `${Math.round((n / of) * 100)}%` : '–')
// Tone only for the share that failed: a tint, never a stripe.
const tone = (t: FeatureTiming) => (t.timeouts > 0 || t.errors * 10 >= t.calls ? 'badge--pink' : t.errors > 0 ? 'badge--amber' : 'badge--green')
</script>

<template>
  <section class="panel">
    <p class="eyebrow">Feature health</p><h2>How long features take</h2>
    <p class="muted small">Calls, errors, timeouts and durations per feature and client: the self-hosted model's work (pre-brief, source extracts, gateway answers, prompt shortening), the plugin's hooks and the tools. Only counts and durations are kept, never prompts, paths, people or sessions. p50 and p95 are estimated from a latency histogram; slowest p95 first.</p>
    <div class="row filters">
      <span class="label" id="feature-window">Window</span>
      <button v-for="w in WINDOWS" :key="w.id" type="button" class="chip" :class="{ active: win === w.id }" :aria-pressed="win === w.id" @click="win = w.id">{{ w.label }}</button>
      <span v-if="pending" class="muted small">refreshing…</span>
    </div>
    <p v-if="error" class="muted small">{{ apiErrorMessage(error) }}</p>
    <div v-else-if="data && groups.length === 0" class="empty"><strong>Nothing timed in this window</strong>Feature timings appear once agents call tools or the plugin's hooks run.</div>
    <div v-else class="stack">
      <section v-for="g in groups" :key="g.kind" class="feature-group">
        <h3>{{ g.label }}</h3>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Feature</th><th class="num">Calls</th><th class="num">Errors</th><th class="num">Timeouts</th>
                <th class="num">Min</th><th class="num">Avg</th><th class="num">p50</th><th class="num">p95</th><th class="num">Max</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="f in g.features" :key="f.feature">
                <tr>
                  <td>
                    <button v-if="f.clients.length > 1" type="button" class="link" :aria-expanded="open.includes(f.feature)" @click="toggle(f.feature)">
                      <code>{{ f.feature }}</code> <span class="muted small">{{ f.clients.length }} clients</span>
                    </button>
                    <template v-else><code>{{ f.feature }}</code> <span v-if="f.clients[0]" class="muted small">{{ f.clients[0].client }}</span></template>
                  </td>
                  <td class="num">{{ fmtCompact(f.total.calls) }}</td>
                  <td class="num"><span class="badge" :class="tone(f.total)">{{ share(f.total.errors, f.total.calls) }}</span></td>
                  <td class="num">{{ fmtCompact(f.total.timeouts) }}</td>
                  <td class="num">{{ dur(f.total.min_ms) }}</td>
                  <td class="num">{{ dur(f.total.avg_ms) }}</td>
                  <td class="num">{{ dur(f.total.p50_ms) }}</td>
                  <td class="num"><strong>{{ dur(f.total.p95_ms) }}</strong></td>
                  <td class="num">{{ dur(f.total.max_ms) }}</td>
                </tr>
                <template v-if="f.clients.length > 1 && open.includes(f.feature)">
                  <tr v-for="c in f.clients" :key="f.feature + c.client" class="client-row">
                    <td><span class="muted">{{ c.client }}</span></td>
                    <td class="num">{{ fmtCompact(c.calls) }}</td>
                    <td class="num">{{ share(c.errors, c.calls) }}</td>
                    <td class="num">{{ fmtCompact(c.timeouts) }}</td>
                    <td class="num">{{ dur(c.min_ms) }}</td>
                    <td class="num">{{ dur(c.avg_ms) }}</td>
                    <td class="num">{{ dur(c.p50_ms) }}</td>
                    <td class="num">{{ dur(c.p95_ms) }}</td>
                    <td class="num">{{ dur(c.max_ms) }}</td>
                  </tr>
                </template>
              </template>
            </tbody>
          </table>
        </div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.filters { gap: 8px; }
.small { font-size: 12.5px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
.feature-group + .feature-group { border-top: 1px solid var(--line); padding-top: 12px; }
.client-row td { background: var(--panel-2); }
button.link { background: none; border: 0; padding: 0; min-height: 0; color: var(--ink); cursor: pointer; font: inherit; text-align: left; }
button.link:hover code { text-decoration: underline; }
</style>
