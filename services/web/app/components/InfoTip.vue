<script setup lang="ts">
// Small "?" that explains a term on hover or keyboard focus.
const props = defineProps<{ term?: string; text?: string }>()
const body = computed(() => props.text ?? (props.term ? GLOSSARY[props.term] : '') ?? '')
</script>

<template>
  <span class="tip" tabindex="0" :aria-label="body" role="note">?<span class="bubble">{{ body }}</span></span>
</template>

<style scoped>
.tip { position: relative; display: inline-grid; place-items: center; width: 16px; height: 16px; margin-left: 5px; border-radius: 50%; border: 1px solid var(--line-strong); color: var(--muted); font: 600 10px var(--font-mono); cursor: help; vertical-align: middle; text-transform: none; letter-spacing: 0; }
.bubble { display: none; position: absolute; z-index: 20; bottom: calc(100% + 8px); left: 50%; transform: translateX(-50%); width: max-content; max-width: 280px; padding: 8px 10px; border-radius: var(--radius-xs); background: var(--panel-2); border: 1px solid var(--line-strong); color: var(--ink); font: 400 12.5px/1.45 var(--font-sans); box-shadow: 0 12px 30px rgba(0, 0, 0, 0.4); white-space: normal; text-align: left; }
.tip:hover .bubble, .tip:focus .bubble { display: block; }
.tip:focus-visible { box-shadow: var(--focus); }
</style>
