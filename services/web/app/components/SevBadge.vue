<script setup lang="ts">
defineProps<{ severity: string }>()
</script>

<template>
  <span class="sev" :class="`sev--${severity}`">{{ severity }}</span>
</template>
