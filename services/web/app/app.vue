<script setup lang="ts">
const { attr } = useTheme()
useHead({ htmlAttrs: { 'data-theme': attr } })
</script>

<template>
  <NuxtLayout><NuxtPage /></NuxtLayout>
</template>
