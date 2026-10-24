<script setup lang="ts">
// The "Agent spark" mark: an AI sparkle with a prompt cursor in its heart
// and two nodes in orbit, on the ink tile. Colours are the brand's own and
// stay the same in both themes.
withDefaults(defineProps<{ size?: number }>(), { size: 32 })
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 128 128" fill="none" aria-hidden="true" class="brand-mark">
    <rect width="128" height="128" rx="30" fill="#1B1530" />
    <g transform="translate(18 18) scale(0.92)">
      <path d="M50 8C53 36 64 47 92 50C64 53 53 64 50 92C47 64 36 53 8 50C36 47 47 36 50 8Z" fill="#FF5FA2" />
      <path d="M42 43L49 50L42 57" stroke="#FFFFFF" stroke-width="5" stroke-linecap="round" stroke-linejoin="round" />
      <line x1="52" y1="58" x2="60" y2="58" stroke="#FFFFFF" stroke-width="5" stroke-linecap="round" />
      <circle cx="83" cy="17" r="8" fill="#FFD84D" />
      <circle cx="17" cy="83" r="7" fill="#3DDC97" />
    </g>
  </svg>
</template>

<style scoped>
.brand-mark { flex: none; display: block; }
</style>
