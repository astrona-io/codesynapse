<script setup lang="ts">
// Auto-sync setting and status for one repository.
interface Schedule { state: 'manual' | 'paused' | 'active'; interval_minutes: number; paused: boolean; auto_review: boolean; auto_triage: boolean; next_run?: string; last_run?: string; last_status?: string; last_error?: string }
const props = defineProps<{ repo: string }>()
const { data: sc, refresh } = useApiGet<Schedule>(() => repoPath(props.repo, 'schedule'))
const { data: ai } = useApiGet<{ providers: unknown[] }>('ai/providers')
const hasAI = computed(() => (ai.value?.providers.length ?? 0) > 0)
const OPTIONS = [
  { v: 0, label: 'Off (manual)' }, { v: 5, label: 'Every 5 min' }, { v: 15, label: 'Every 15 min' }, { v: 30, label: 'Every 30 min' }, { v: 60, label: 'Every hour' },
  { v: 360, label: 'Every 6 hours' }, { v: 1440, label: 'Daily' },
]
const error = ref('')
async function save(interval: number, paused: boolean, autoReview = sc.value?.auto_review ?? false, autoTriage = sc.value?.auto_triage ?? false) {
  error.value = ''
  try {
    await $fetch(apiPath(repoPath(props.repo, 'schedule')), { method: 'PUT', body: { interval_minutes: interval, paused, auto_review: autoReview, auto_triage: autoTriage } })
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
const label = computed(() => OPTIONS.find((o) => o.v === sc.value?.interval_minutes)?.label.toLowerCase() ?? '')
function until(iso?: string) {
  if (!iso) return ''
  const m = Math.round((Date.parse(iso) - Date.now()) / 60000)
  if (m <= 0) return 'any moment'
  if (m < 60) return `in ${m} min`
  if (m < 48 * 60) return `in ${Math.round(m / 60)} h`
  return `in ${Math.round(m / 1440)} days`
}
</script>

<template>
  <div v-if="sc" class="autosync" :class="`state--${sc.state}`">
    <span class="status">
      <span class="dot" :class="{ 'dot--ok': sc.state === 'active', 'dot--warn': sc.state === 'paused' }" />
      <template v-if="sc.state === 'active'">Auto-sync {{ label }} · next {{ until(sc.next_run) }}</template>
      <template v-else-if="sc.state === 'paused'">Auto-sync paused</template>
      <template v-else>Manual sync only</template>
    </span>
    <select :value="sc.interval_minutes" aria-label="Auto-sync interval" @change="save(Number(($event.target as HTMLSelectElement).value), false)">
      <option v-for="o in OPTIONS" :key="o.v" :value="o.v">{{ o.label }}</option>
    </select>
    <button v-if="sc.state !== 'manual'" type="button" class="secondary tiny" @click="save(sc.interval_minutes, !sc.paused)">{{ sc.paused ? 'Resume' : 'Pause' }}</button>
    <span v-if="sc.last_status === 'failed'" class="error small" role="status" :title="sc.last_error">last auto-sync failed</span>
    <span class="sep" />
    <span class="muted">After each sync:</span>
    <label class="toggle" :title="hasAI ? 'Review the commits that arrived with the sync' : 'Configure an AI model first'">
      <input type="checkbox" :checked="sc.auto_review" :disabled="!hasAI" @change="save(sc.interval_minutes, sc.paused, ($event.target as HTMLInputElement).checked, sc.auto_triage)"> AI-review new commits
    </label>
    <label class="toggle" :title="hasAI ? 'Triage security findings that are new since the previous sync' : 'Configure an AI model first'">
      <input type="checkbox" :checked="sc.auto_triage" :disabled="!hasAI" @change="save(sc.interval_minutes, sc.paused, sc.auto_review, ($event.target as HTMLInputElement).checked)"> AI-triage new findings
    </label>
    <NuxtLink v-if="!hasAI" to="/ai" class="small">set up a model</NuxtLink>
    <span v-if="error" class="error small" role="alert">{{ error }}</span>
  </div>
</template>

<style scoped>
.autosync { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; }
.status { display: inline-flex; align-items: center; gap: 8px; color: var(--muted); }
.state--active .status { color: var(--ink); }
select { width: auto; min-height: 34px; padding: 4px 10px; font-size: 13px; }
.small { font-size: 12px; }
.sep { width: 1px; height: 18px; background: var(--line-strong); margin: 0 4px; }
.toggle { display: inline-flex; align-items: center; gap: 6px; color: var(--ink); font-size: 13px; font-weight: 400; }
</style>
