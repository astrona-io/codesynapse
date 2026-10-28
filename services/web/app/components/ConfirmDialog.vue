<script setup lang="ts">
// Accessible confirmation dialog for destructive actions. With `confirmText`
// the user must type it (e.g. the repository name) before confirming.
const props = defineProps<{ open: boolean; title: string; confirmLabel?: string; confirmText?: string; danger?: boolean; busy?: boolean }>()
const emit = defineEmits<{ confirm: []; cancel: [] }>()
const titleId = useId()
const typed = ref('')
const dialog = ref<HTMLDialogElement | null>(null)
watch(() => props.open, (open) => {
  typed.value = ''
  if (open) dialog.value?.showModal()
  else dialog.value?.close()
})
const canConfirm = computed(() => !props.busy && (!props.confirmText || typed.value.trim() === props.confirmText))
</script>

<template>
  <!-- Teleported so dialogs inside links (repository cards) do not trigger them. -->
  <Teleport to="#teleports">
  <dialog ref="dialog" class="confirm" :aria-labelledby="titleId" @cancel.prevent="emit('cancel')" @close="emit('cancel')">
    <form method="dialog" class="body" @submit.prevent="canConfirm && emit('confirm')">
      <h2 :id="titleId">{{ title }}</h2>
      <div class="content"><slot /></div>
      <label v-if="confirmText">Type <strong class="mono">{{ confirmText }}</strong> to confirm
        <input v-model="typed" autocomplete="off" :placeholder="confirmText" autofocus>
      </label>
      <div class="actions">
        <button type="button" class="secondary" @click="emit('cancel')">Cancel</button>
        <button type="submit" :class="{ danger }" :disabled="!canConfirm">{{ busy ? 'Working…' : (confirmLabel ?? 'Confirm') }}</button>
      </div>
    </form>
  </dialog>
  </Teleport>
</template>

<style scoped>
.confirm { border: 2px solid var(--line-strong); border-radius: var(--radius); background: var(--panel); color: var(--ink); padding: 0; width: min(480px, calc(100vw - 32px)); box-shadow: var(--shadow-pop); }
.confirm::backdrop { background: var(--backdrop); backdrop-filter: blur(4px); }
.confirm strong.mono { overflow-wrap: anywhere; }
.body { display: grid; gap: 14px; padding: 22px 24px; }
.content { color: var(--muted); font-size: 14px; display: grid; gap: 8px; }
label { display: grid; gap: 6px; }
.actions { display: flex; justify-content: flex-end; gap: 10px; }
</style>
