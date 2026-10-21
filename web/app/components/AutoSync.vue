<script setup lang="ts">
// Auto-sync setting and status for one repository.
interface Schedule { state: 'manual' | 'paused' | 'active'; interval_minutes: number; paused: boolean; next_run?: string; last_run?: string; last_status?: string; last_error?: string }
const props = defineProps<{ repo: string }>()
const { data: sc, refresh } = useApiGet<Schedule>(() => repoPath(props.repo, 'schedule'))
const OPTIONS = [
  { v: 0, label: 'Off (manual)' }, { v: 15, label: 'Every 15 min' }, { v: 60, label: 'Every hour' },
  { v: 360, label: 'Every 6 hours' }, { v: 1440, label: 'Daily' },
]
const error = ref('')
async function save(interval: number, paused: boolean) {
  error.value = ''
  try {
    await $fetch(apiPath(repoPath(props.repo, 'schedule')), { method: 'PUT', body: { interval_minutes: interval, paused } })
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
    <button v-if="sc.state !== 'manual'" class="secondary tiny" @click="save(sc.interval_minutes, !sc.paused)">{{ sc.paused ? 'Resume' : 'Pause' }}</button>
    <span v-if="sc.last_status === 'failed'" class="error small" :title="sc.last_error">last auto-sync failed</span>
    <span v-if="error" class="error small">{{ error }}</span>
  </div>
</template>

<style scoped>
.autosync { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 13px; }
.status { display: inline-flex; align-items: center; gap: 8px; color: var(--muted); }
.state--active .status { color: var(--ink); }
select { width: auto; min-height: 34px; padding: 4px 10px; font-size: 13px; }
.small { font-size: 12px; }
</style>
