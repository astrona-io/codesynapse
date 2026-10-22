<script setup lang="ts">
import type { AdminHealth, AdminOrg, HealthCheck, SeriesRange } from '~/utils/types'

const { data: h, error, refresh, pending } = useApiGet<AdminHealth>('admin/health')
const { data: orgs } = useApiGet<{ orgs: AdminOrg[] }>('admin/orgs')
const checks = computed(() => (h.value ? [
  { label: 'Graph database', c: h.value.graph },
  { label: 'Accounts database', c: h.value.accounts },
  { label: 'Live statistics', c: h.value.metrics },
  { label: 'Audit log chain', c: h.value.audit },
] : []))
// Live statistics that were never set up are off, not broken.
const off = (c: HealthCheck) => !c.ok && c.detail.startsWith('not configured')
const range = ref<SeriesRange>('24h')
const assignTo = reactive<Record<string, string>>({})
watch(() => h.value?.unowned, (gs) => gs?.forEach((g) => { assignTo[g] ??= '' }), { immediate: true })
const msg = ref('')
async function assign(graph: string) {
  msg.value = ''
  try {
    await apiPost(`accounts/orgs/${assignTo[graph]}/repos`, { graph })
    msg.value = `${graph} now belongs to ${assignTo[graph]}.`
    await refresh()
  } catch (e) {
    msg.value = apiErrorMessage(e)
  }
}
</script>

<template>
  <div class="stack">
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-if="h">
      <section class="panel">
        <div class="panel__header"><h2>System health</h2><button class="secondary tiny" :disabled="pending" @click="refresh()">{{ pending ? 'Checking…' : 'Check again' }}</button></div>
        <div class="grid cols-4">
          <div v-for="x in checks" :key="x.label" class="check" :class="{ 'check--bad': !x.c.ok && !off(x.c) }">
            <p class="label">{{ x.label }}</p>
            <p class="state"><span class="dot" :class="x.c.ok ? 'dot--ok' : off(x.c) ? '' : 'dot--bad'" /> {{ x.c.ok ? 'OK' : off(x.c) ? 'Off' : 'Problem' }}</p>
            <p class="muted small">{{ x.c.detail }}</p>
            <p class="dim small">{{ Math.round(x.c.seconds * 1000) }} ms</p>
          </div>
        </div>
        <p class="muted small">Version {{ h.version || 'dev' }} · checked {{ relTime(h.checked) }} · indexing jobs: {{ h.jobs.running }} running, {{ h.jobs.failed }} failed of {{ h.jobs.total }} recent</p>
      </section>
      <div class="grid cols-4">
        <StatTile label="People" :value="String(h.counts.users)" :hint="`${h.counts.admins} platform admin(s) · ${h.counts.suspended} suspended`" />
        <StatTile label="Organisations" :value="String(h.counts.orgs)" :hint="`${(orgs?.orgs ?? []).reduce((t, o) => t + o.lost_access, 0)} repositories without access`" />
        <StatTile label="Indexed repositories" :value="String(h.counts.graphs)" :hint="`${h.unowned.length} not assigned to an organisation`" />
        <StatTile label="Audit entries verified" :value="fmtCompact(h.counts.audit_events_checked)" :hint="h.audit.ok ? 'hash chain intact' : 'chain broken: see the audit log'" />
      </div>
      <section v-if="h.unowned.length" class="panel edge--amber">
        <h2>Repositories without an organisation <span class="muted">{{ h.unowned.length }}</span></h2>
        <p class="muted small">Indexed before accounts existed, or by the installation's own token. Only platform admins see them until you assign each to an organisation.</p>
        <p v-if="msg" class="notice" role="status">{{ msg }}</p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Repository</th><th>Assign to</th><th /></tr></thead>
            <tbody>
              <tr v-for="g in h.unowned.slice(0, 50)" :key="g">
                <td class="mono">{{ g }}</td>
                <td><select v-model="assignTo[g]" class="inline" :aria-label="`Organisation for ${g}`"><option value="" disabled>Choose…</option><option v-for="o in orgs?.orgs ?? []" :key="o.id" :value="o.slug">{{ o.name }}</option></select></td>
                <td class="num"><button class="secondary tiny" :disabled="!assignTo[g]" @click="assign(g)">Assign</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="h.unowned.length > 50" class="muted small">Showing 50 of {{ h.unowned.length }}.</p>
      </section>
      <section class="panel">
        <div class="panel__header">
          <h2>Usage across organisations</h2>
          <div class="row"><button v-for="r in (['24h', '7d', '30d'] as SeriesRange[])" :key="r" class="chip" :class="{ active: range === r }" @click="range = r">{{ r === '24h' ? '24 hours' : r === '7d' ? '7 days' : '30 days' }}</button></div>
        </div>
        <p v-if="!h.metrics.ok" class="muted">{{ off(h.metrics) ? 'Live statistics are off. Set CODESYNAPSE_METRICS_URL to VictoriaMetrics to see usage per organisation by the minute and hour.' : 'Live statistics are not answering right now; see System health above.' }}</p>
        <div v-else class="grid cols-2">
          <SeriesCard panel="tenants" :range="range" :tick="0" />
          <SeriesCard panel="platform_llm_cost" :range="range" :tick="0" />
          <SeriesCard panel="platform_tool_latency" :range="range" :tick="0" />
          <SeriesCard panel="platform_db_latency" :range="range" :tick="0" />
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.check { display: flex; flex-direction: column; gap: 4px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-sm); }
.check--bad { border-color: var(--danger); background: var(--danger-soft); }
.state { display: flex; align-items: center; gap: 8px; font-weight: 600; }
.inline { width: auto; min-height: 30px; padding: 3px 8px; }
</style>
