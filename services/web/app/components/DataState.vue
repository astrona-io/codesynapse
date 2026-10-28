<script setup lang="ts">
// Loading, error and empty states said the same way everywhere. The error
// wins, then loading (only while there is nothing to show yet), then empty;
// otherwise the default slot renders. compact is for a line inside a card or
// chart instead of a whole panel. Status changes are announced politely,
// errors assertively.
const props = withDefaults(defineProps<{
  pending?: boolean
  error?: unknown
  empty?: boolean
  title?: string
  text?: string
  loading?: string
  compact?: boolean
}>(), { pending: false, error: undefined, empty: false, title: 'Nothing here yet', text: '', loading: 'Loading…', compact: false })

const message = computed(() => {
  const e = props.error
  if (!e) return ''
  return typeof e === 'string' ? e : apiErrorMessage(e)
})
const state = computed(() => (message.value ? 'error' : props.pending && props.empty ? 'loading' : props.empty ? 'empty' : 'ready'))
</script>

<template>
  <p v-if="state === 'error'" :class="compact ? 'error small' : 'notice notice--danger'" role="alert">{{ message }}</p>
  <p v-else-if="state === 'loading'" class="ds-loading" :class="{ compact }" role="status">
    <span class="ds-spinner" aria-hidden="true" />{{ loading }}
  </p>
  <template v-else-if="state === 'empty'">
    <p v-if="compact" class="muted small"><slot name="empty">{{ text || title }}</slot></p>
    <div v-else class="empty"><strong>{{ title }}</strong><slot name="empty">{{ text }}</slot></div>
  </template>
  <slot v-else />
</template>

<style scoped>
.ds-loading { display: flex; align-items: center; justify-content: center; gap: 10px; padding: 24px; color: var(--muted); font-size: 13.5px; }
.ds-loading.compact { justify-content: flex-start; padding: 4px 0; font-size: 12.5px; }
.ds-spinner { flex: none; width: 14px; height: 14px; border-radius: 50%; border: 2px solid var(--line); border-top-color: var(--muted); animation: ds-spin 0.9s linear infinite; }
@keyframes ds-spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .ds-spinner { animation: none; border-top-color: var(--line); } }
.small { font-size: 12.5px; }
</style>
