<script setup lang="ts">
// Link to a symbol in the explorer.
const props = defineProps<{ repo: string; id: string; name: string }>()
const to = computed(() => ({ path: `/repos/${encodeURIComponent(props.repo)}/explore`, query: { id: props.id } }))
</script>

<template>
  <NuxtLink :to="to" class="mono">{{ name }}</NuxtLink>
</template>
