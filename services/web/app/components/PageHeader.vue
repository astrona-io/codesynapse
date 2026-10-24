<script setup lang="ts">
// Page opener: breadcrumbs above a quiet header with an optional label,
// title and lede on the left and actions on the right.
defineProps<{ eyebrow?: string; title: string; lede?: string; crumbs?: { label: string; to?: string }[] }>()
</script>

<template>
  <div class="page-head">
    <nav v-if="crumbs?.length" class="crumbs" aria-label="Breadcrumb">
      <template v-for="(c, i) in crumbs" :key="i">
        <span v-if="i" class="sep">/</span>
        <NuxtLink v-if="c.to && i < crumbs.length - 1" :to="c.to">{{ c.label }}</NuxtLink>
        <span v-else class="current">{{ c.label }}</span>
      </template>
    </nav>
    <header class="panel topbar">
      <div class="topbar__text">
        <p v-if="eyebrow" class="eyebrow">{{ eyebrow }}</p>
        <h1>{{ title }}</h1>
        <p v-if="lede" class="muted">{{ lede }}</p>
        <slot name="meta" />
      </div>
      <div v-if="$slots.default" class="topbar__actions"><slot /></div>
    </header>
  </div>
</template>

<style scoped>
.page-head { display: flex; flex-direction: column; gap: 10px; }
</style>
