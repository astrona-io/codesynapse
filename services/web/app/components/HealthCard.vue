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
.health { gap: 4px; padding: 18px 20px; border-left: 3px solid var(--line-strong); color: var(--ink); transition: transform 0.15s, border-color 0.15s; }
a.health:hover { transform: translateY(-2px); color: var(--ink); }
.value { font-size: 30px; font-weight: 700; line-height: 1.15; font-variant-numeric: tabular-nums; }
.verdict { font-size: 13.5px; color: var(--muted); }
.action { margin-top: 6px; font: 500 12px var(--font-mono); text-transform: uppercase; letter-spacing: 0.08em; color: var(--orange-ink); }
.tone--good { border-left-color: var(--green); } .tone--good .value { color: var(--green); }
.tone--warn { border-left-color: var(--amber); } .tone--warn .value { color: var(--amber); }
.tone--bad { border-left-color: var(--pink); } .tone--bad .value { color: var(--pink); }
.tone--neutral .value { color: var(--cyan); }
</style>
