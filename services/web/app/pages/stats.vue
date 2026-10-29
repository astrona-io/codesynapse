<script setup lang="ts">
import { STAT_WINDOWS, useStatistics, windowChip } from '~/composables/useStatistics'
useHead({ title: 'Statistics · CodeSynapse' })
const route = useRoute()
if (route.path === '/stats') await navigateTo('/stats/agents', { replace: true })
const { days, pending } = useStatistics()
// Live activity (minutes to days) and the model queue (weeks) have windows
// of their own.
const showWindow = computed(() => !route.path.endsWith('/activity') && !route.path.endsWith('/queue'))
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Workspace" title="Statistics"
      lede="How agents use CodeSynapse, what context offloading saves, and where the workspace stands on security, licences, update automation and reviews. Estimates say how they are estimated. Nothing here is configured: settings live under Settings."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: 'Statistics' }]"
    />
    <nav class="tabs" aria-label="Statistics">
      <NuxtLink to="/stats/agents">Agents</NuxtLink>
      <NuxtLink to="/stats/models">Models &amp; savings</NuxtLink>
      <NuxtLink to="/stats/queue">Model queue</NuxtLink>
      <NuxtLink to="/stats/memory">Agent memory</NuxtLink>
      <NuxtLink to="/stats/activity">Live activity</NuxtLink>
      <NuxtLink to="/stats/code">Code &amp; security</NuxtLink>
      <NuxtLink to="/stats/dependencies">Dependencies &amp; licences</NuxtLink>
      <NuxtLink to="/stats/reviews">Reviews &amp; AI</NuxtLink>
      <NuxtLink to="/stats/sync">Sync health</NuxtLink>
    </nav>
    <div v-if="showWindow" class="row filters">
      <span class="label" id="stat-window">Window</span>
      <div class="segmented" role="group" aria-labelledby="stat-window">
        <button v-for="d in STAT_WINDOWS" :key="d" type="button" :aria-pressed="days === d" @click="days = d">{{ windowChip(d) }}</button>
      </div>
      <span v-if="pending" class="muted small">refreshing…</span>
    </div>
    <NuxtPage />
  </div>
</template>

<style scoped>
.filters { gap: 8px; }
.small { font-size: 12.5px; }
</style>
