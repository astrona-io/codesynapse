<script setup lang="ts">
import type { AttentionItem, AttentionLevel } from '~/utils/types'

// A ranked list of things to fix. The level is spelled out next to its
// colour, so meaning never rests on colour alone.
const props = withDefaults(defineProps<{ items: AttentionItem[]; showRepo?: boolean; limit?: number }>(), { showRepo: true, limit: 0 })
const LABEL: Record<AttentionLevel, string> = { critical: 'Critical', high: 'High', medium: 'Medium', low: 'Low' }
// The shared severity styles; low reads as the quiet "info" tint here.
const SEV: Record<AttentionLevel, string> = { critical: 'critical', high: 'high', medium: 'medium', low: 'info' }
const expanded = ref(false)
const shown = computed(() => (props.limit && !expanded.value ? props.items.slice(0, props.limit) : props.items))
</script>

<template>
  <ol class="attention">
    <li v-for="(it, i) in shown" :key="i" class="item" :class="`item--${it.level}`">
      <span class="sev level" :class="`sev--${SEV[it.level]}`">{{ LABEL[it.level] }}</span>
      <div class="body">
        <p class="title">
          <NuxtLink v-if="showRepo && it.repo" :to="`/repos/${encodeURIComponent(it.repo)}`" class="repo mono">{{ it.repo }}</NuxtLink>
          <span>{{ it.title }}</span>
        </p>
        <p class="muted small">{{ it.detail }}</p>
      </div>
      <NuxtLink :to="it.link" class="btn btn--ghost action">{{ it.action }}</NuxtLink>
    </li>
  </ol>
  <button v-if="limit && items.length > limit" type="button" class="link small more" :aria-expanded="expanded" @click="expanded = !expanded">
    {{ expanded ? 'Show fewer' : `Show all ${items.length}` }}
  </button>
</template>

<style scoped>
.attention { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; }
.item { display: grid; grid-template-columns: 76px minmax(0, 1fr) auto; gap: 14px; align-items: center; padding: 12px 4px; border-bottom: 1px solid var(--line); }
.item:last-child { border-bottom: 0; }
.level { justify-self: start; }
.title { display: flex; flex-wrap: wrap; gap: 4px 10px; align-items: baseline; font-weight: 550; }
.repo { font-size: 12.5px; overflow-wrap: anywhere; }
.action { min-height: 32px; padding: 5px 12px; font-size: 13px; white-space: nowrap; }
.more { margin-top: 8px; }
@media (max-width: 720px) {
  .item { grid-template-columns: 1fr; gap: 6px; }
  .action { justify-self: start; }
}
</style>
