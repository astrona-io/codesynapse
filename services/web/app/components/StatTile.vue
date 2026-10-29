<script setup lang="ts">
// hint is a short line with the figure's data; tip holds the longer
// explanation (method, caveats, what is left out) behind an info icon.
defineProps<{ label: string; value: string | number; hint?: string; tip?: string }>()
</script>

<template>
  <div class="panel stat-card">
    <p class="eyebrow">{{ label }}<InfoTip v-if="tip" :text="tip" :label="`About ${label}`" /></p>
    <p class="stat__value">{{ value }}</p>
    <p v-if="hint" class="stat__hint">{{ hint }}</p>
  </div>
</template>
