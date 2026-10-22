<script setup lang="ts">
// A value to copy: a key shown once, a command, a config snippet.
const props = withDefaults(defineProps<{ value: string; label?: string; multiline?: boolean; secret?: boolean }>(), { label: '', multiline: false, secret: false })
const copied = ref(false)
async function copy() {
  await navigator.clipboard.writeText(props.value)
  copied.value = true
  setTimeout(() => (copied.value = false), 1600)
}
</script>

<template>
  <div class="copy">
    <span v-if="label" class="label">{{ label }}</span>
    <div class="copy__box">
      <pre v-if="multiline" class="copy__value"><code>{{ value }}</code></pre>
      <code v-else class="copy__value copy__value--line" :class="{ secret }">{{ value }}</code>
      <button class="secondary tiny" type="button" @click="copy">{{ copied ? 'Copied' : 'Copy' }}</button>
    </div>
  </div>
</template>

<style scoped>
.copy { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.copy__box { display: flex; align-items: flex-start; gap: 8px; padding: 8px 8px 8px 12px; border: 1px solid var(--line-strong); border-radius: var(--radius-sm); background: var(--panel-2); }
.copy__value { flex: 1; min-width: 0; margin: 0; background: none; padding: 2px 0; font: 12.5px/1.55 var(--font-mono); white-space: pre-wrap; word-break: break-all; }
.copy__value--line { white-space: nowrap; overflow-x: auto; }
.secret { color: var(--accent-ink); }
</style>
