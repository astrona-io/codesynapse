<script setup lang="ts">
// A headline number with a one-line verdict and where to go next.
defineProps<{ label: string; value: string | number; verdict: string; tone?: 'good' | 'warn' | 'bad' | 'neutral'; to?: string; action?: string; tip?: string }>()
</script>

<template>
  <component :is="to ? 'NuxtLink' : 'div'" :to="to" class="panel health" :class="`tone--${tone ?? 'neutral'}`">
    <p class="eyebrow">{{ label }}<InfoTip v-if="tip" :term="tip" /></p>
    <p class="value">{{ value }}</p>
    <p class="verdict">{{ verdict }}</p>
    <p v-if="to && action" class="action">{{ action }} →</p>
  </component>
</template>

<style scoped>
.health { gap: 4px; padding: 20px 22px; background: var(--panel-2); border-color: transparent; color: var(--ink); transition: transform 0.15s, border-color 0.15s; }
a.health:hover { transform: translateY(-2px); border-color: var(--line-strong); color: var(--ink); text-decoration: none; }
.value { font: 800 36px/1.1 var(--font-display); letter-spacing: -0.03em; font-variant-numeric: tabular-nums; }
.verdict { font-size: 13.5px; color: var(--muted); }
.action { margin-top: 6px; font: 600 13px var(--font-sans); color: var(--accent-ink); }
.tone--good .value { color: var(--success); }
.tone--warn .value { color: var(--warning); }
.tone--bad .value { color: var(--danger); }
.tone--neutral .value { color: var(--info); }
</style>
