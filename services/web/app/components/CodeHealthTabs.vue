<script setup lang="ts">
// Code health is four pages under one heading: quality, unused code, duplicates and the plan that ranks them.
const props = defineProps<{ repo: string }>()
const base = computed(() => `/repos/${encodeURIComponent(props.repo)}`)
</script>

<template>
  <nav class="tabs" aria-label="Code health">
    <NuxtLink :to="`${base}/metrics`">Quality</NuxtLink>
    <NuxtLink :to="`${base}/cleanup`">Unused code</NuxtLink>
    <NuxtLink :to="`${base}/duplicates`">Duplicates</NuxtLink>
    <NuxtLink :to="`${base}/cleanup-plan`">Plan</NuxtLink>
  </nav>
</template>
