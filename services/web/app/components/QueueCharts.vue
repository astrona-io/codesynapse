<script setup lang="ts">
import type { SeriesRange } from '~/utils/types'

// The four model queue charts, for every provider or for one (provider, a
// name the queue answer marked configured; the API checks it again). Mounted
// once the queue answer is in, so a link to a model's tab asks for that
// model's charts only, never for every provider first: each platform view
// is written to the audit log.
const props = defineProps<{ scope: 'org' | 'platform'; org?: string; provider?: string; range: SeriesRange }>()

const PANELS = ['queue_load', 'queue_wait', 'queue_saturation', 'queue_start']
const { panels: charts, pending } = useSeriesBatch(PANELS, () => props.range, undefined,
  props.scope === 'platform'
    ? { base: 'admin/stats/series/batch', org: () => props.org, provider: () => props.provider }
    : { provider: () => props.provider })
const hidden = ref<string[]>([])
function unavailable(panel: string) {
  if (!hidden.value.includes(panel)) hidden.value.push(panel)
}
</script>

<template>
  <div class="grid cols-2">
    <template v-for="panel in PANELS" :key="panel">
      <SeriesCard v-if="!hidden.includes(panel)" :panel="panel" :range="range" :batched="charts[panel] ?? null" :batch-pending="pending" :y-max="panel === 'queue_saturation' ? 100 : undefined" @unavailable="unavailable(panel)" />
    </template>
  </div>
</template>
