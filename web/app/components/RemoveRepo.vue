<script setup lang="ts">
// "Remove repository" button with a confirmation dialog.
const props = defineProps<{ repo: string; managed?: boolean; small?: boolean }>()
const emit = defineEmits<{ removed: [] }>()
const open = ref(false)
const busy = ref(false)
const deleteFiles = ref(true)
const error = ref('')
async function remove() {
  busy.value = true
  error.value = ''
  try {
    const q = props.managed && deleteFiles.value ? '?files=1' : ''
    await $fetch(apiPath(`repos/${encodeURIComponent(props.repo)}${q}`), { method: 'DELETE' })
    open.value = false
    emit('removed')
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <button class="secondary" :class="{ tiny: small }" @click.prevent.stop="open = true">Remove</button>
  <ConfirmDialog :open="open" :title="`Remove ${repo}?`" confirm-label="Remove repository" :confirm-text="repo" danger :busy="busy" @confirm="remove" @cancel="open = false">
    <p>This deletes the indexed graph, its security and cleanup results, and its auto-sync schedule. You can index it again at any time.</p>
    <label v-if="managed" class="check"><input v-model="deleteFiles" type="checkbox"> Also delete the cloned files from the workspace</label>
    <p v-else>The source folder on disk is not touched.</p>
    <p v-if="error" class="error">{{ error }}</p>
  </ConfirmDialog>
</template>

<style scoped>
.check { display: flex; gap: 8px; align-items: center; color: var(--ink); }
</style>
