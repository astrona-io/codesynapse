<script setup lang="ts">
import type { AttentionItem, AttentionLevel } from '~/utils/types'

// A ranked list of things to fix. The level is spelled out next to its
// colour, so meaning never rests on colour alone.
const props = withDefaults(defineProps<{ items: AttentionItem[]; showRepo?: boolean; limit?: number }>(), { showRepo: true, limit: 0 })
const LABEL: Record<AttentionLevel, string> = { critical: 'Critical', high: 'High', medium: 'Medium', low: 'Low' }
const expanded = ref(false)
const shown = computed(() => (props.limit && !expanded.value ? props.items.slice(0, props.limit) : props.items))
</script>

<template>
  <ol class="attention">
    <li v-for="(it, i) in shown" :key="i" class="item" :class="`item--${it.level}`">
      <span class="level" :class="`level--${it.level}`">{{ LABEL[it.level] }}</span>
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
  <button v-if="limit && items.length > limit" class="link small more" @click="expanded = !expanded">
    {{ expanded ? 'Show fewer' : `Show all ${items.length}` }}
  </button>
</template>

<style scoped>
.attention { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; }
.item { display: grid; grid-template-columns: 76px minmax(0, 1fr) auto; gap: 14px; align-items: center; padding: 12px 4px; border-bottom: 1px solid var(--line); }
.item:last-child { border-bottom: 0; }
.level { justify-self: start; font: 600 11.5px/1.5 var(--font-sans); padding: 2px 8px; border-radius: var(--pill); }
.level--critical { background: var(--danger); color: #fff; }
.level--high { background: var(--danger-soft); color: var(--danger); }
.level--medium { background: var(--warning-soft); color: var(--warning); }
.level--low { background: var(--panel-2); color: var(--muted); }
.title { display: flex; flex-wrap: wrap; gap: 4px 10px; align-items: baseline; font-weight: 550; }
.repo { font-size: 12.5px; }
.action { min-height: 32px; padding: 5px 12px; font-size: 13px; white-space: nowrap; }
.more { margin-top: 8px; }
@media (max-width: 720px) {
  .item { grid-template-columns: 1fr; gap: 6px; }
  .action { justify-self: start; }
}
</style>
