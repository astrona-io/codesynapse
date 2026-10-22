<script setup lang="ts">
// Code health is two pages under one heading: quality and unused code.
const props = defineProps<{ repo: string }>()
const base = computed(() => `/repos/${encodeURIComponent(props.repo)}`)
</script>

<template>
  <nav class="tabs" aria-label="Code health">
    <NuxtLink :to="`${base}/metrics`">Quality</NuxtLink>
    <NuxtLink :to="`${base}/cleanup`">Unused code</NuxtLink>
  </nav>
</template>
