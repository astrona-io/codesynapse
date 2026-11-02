<script setup lang="ts">
import type { GatewayTurn } from '~/utils/types'

// How the cloud agent's input grows over a conversation, one row per turn
// bucket (messages in the request). Left: who answered, as one stacked bar
// (self-hosted, cloud). Right: the cloud input per answer, the median as a
// bar and the 90th percentile as a tick, all rows on one scale so growth
// reads top to bottom. Every value is also in the text beside its bar.
const props = defineProps<{ turns: GatewayTurn[] }>()
const top = computed(() => Math.max(1, ...props.turns.map((t) => t.cloud_input_p90 ?? t.cloud_input_p50 ?? 0)))
const pct = (v: number | null | undefined) => `${Math.min(100, Math.max(0, ((v ?? 0) / top.value) * 100))}%`
const share = (t: GatewayTurn, n: number) => (t.requests > 0 ? `${(n / t.requests) * 100}%` : '0%')
</script>

<template>
  <div v-if="turns.length" class="turns">
    <div class="legend muted">
      <span><i class="sw self" />CodeSynapse model (self-hosted)</span>
      <span><i class="sw cloud" />cloud</span>
      <span><i class="sw tick" />cloud input, 90th percentile</span>
    </div>
    <div class="row head muted">
      <span>Messages in the request</span><span>Answered by</span><span>Cloud input per answer (median)</span>
    </div>
    <div v-for="t in turns" :key="t.turn" class="row">
      <span class="turn mono">{{ t.turn }}</span>
      <div>
        <div class="track" :title="`${fmtCompact(t.local)} self-hosted · ${fmtCompact(t.cloud)} cloud`">
          <span class="self" :style="{ width: share(t, t.local) }" /><span class="cloud" :style="{ width: share(t, t.cloud) }" />
        </div>
        <div class="val">{{ fmtCompact(t.requests) }} requests · {{ t.local_share_pct }}% self-hosted</div>
      </div>
      <div>
        <div class="track input">
          <span v-if="t.cloud_input_p50 != null" class="cloud" :style="{ width: pct(t.cloud_input_p50) }" />
          <i v-if="t.cloud_input_p90 != null" class="p90" :style="{ left: pct(t.cloud_input_p90) }" />
        </div>
        <div class="val">
          <template v-if="t.cloud_input_p50 != null">{{ fmtCompact(t.cloud_input_p50) }} tokens · p90 {{ t.cloud_input_p90 != null ? fmtCompact(t.cloud_input_p90) : '–' }}<template v-if="t.cached_share_pct != null"> · {{ t.cached_share_pct }}% from cache</template></template>
          <template v-else>no cloud usage reported</template>
        </div>
      </div>
    </div>
  </div>
  <p v-else class="muted small">No conversation request went through the gateway in this window.</p>
</template>

<style scoped>
.turns { display: grid; gap: 10px; font-size: 13px; }
.legend { display: flex; flex-wrap: wrap; gap: 14px; font-size: 12px; }
.sw { display: inline-block; width: 10px; height: 10px; border-radius: 3px; margin-right: 6px; vertical-align: -1px; }
.sw.self, .self { background: var(--s-1); }
.sw.cloud, .cloud { background: var(--s-2); }
.sw.tick { width: 2px; background: var(--ink); border-radius: 0; }
.row { display: grid; grid-template-columns: 80px 1fr 1.4fr; gap: 14px; align-items: start; }
.row.head { font-size: 12px; }
.turn { padding-top: 1px; }
.track { position: relative; display: flex; height: 8px; border-radius: 4px; background: var(--panel-2); overflow: hidden; }
.track.input { overflow: visible; }
.track > span { display: block; height: 100%; }
.track.input > span { border-radius: 4px; min-width: 2px; }
.p90 { position: absolute; top: -3px; width: 2px; height: 14px; background: var(--ink); transform: translateX(-1px); }
.val { margin-top: 3px; font-size: 12px; font-variant-numeric: tabular-nums; color: var(--ink); }
.small { font-size: 12.5px; }
@media (max-width: 640px) {
  .row { grid-template-columns: 1fr; gap: 4px; }
  .row.head { display: none; }
}
</style>
