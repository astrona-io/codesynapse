<script setup lang="ts">
import type { ProviderCheck } from '~/utils/types'

// One local model server's check: is it there, are the models pulled and
// does it really give the chat model context_window tokens.
const props = defineProps<{ check: ProviderCheck; title?: boolean }>()
const ctx = (n?: number) => (n ? `${fmtInt(n)} tokens` : 'not reported')
const loaded = computed(() => {
  const c = props.check
  if (c.loaded_context) return ctx(c.loaded_context)
  if (c.server !== 'ollama') return 'not reported'
  return c.num_ctx ? `not loaded (Modelfile num_ctx ${fmtInt(c.num_ctx)})` : 'not loaded'
})
</script>

<template>
  <div class="pcheck">
    <p v-if="title" class="pcheck__title">
      <span class="dot" :class="check.ok ? 'dot--ok' : 'dot--bad'" />
      <strong class="mono">{{ check.name }}</strong>
      <span class="muted small">{{ check.server === 'ollama' ? `Ollama ${check.version ?? ''}` : check.reachable ? 'OpenAI-compatible' : 'not answering' }}</span>
      <span v-if="check.gateway" class="badge badge--violet">gateway</span>
    </p>
    <dl class="facts small">
      <dt>Model</dt><dd class="mono wrap">{{ check.model || '(server default)' }} · {{ check.model_found ? 'on the server' : check.reachable ? 'not found' : 'unknown' }}</dd>
      <template v-if="check.embedding_model"><dt>Embeddings</dt><dd class="mono wrap">{{ check.embedding_model }} · {{ check.embedding_found ? 'on the server' : check.reachable ? 'not found' : 'unknown' }}</dd></template>
      <dt>Context</dt><dd>{{ check.context_window ? fmtInt(check.context_window) : 'not set' }} configured · {{ loaded }} loaded<template v-if="check.model_context"> · model takes {{ fmtInt(check.model_context) }}</template></dd>
      <dt>Relied on by</dt><dd><span v-if="check.gateway" class="tag">gateway</span><span v-for="t in check.tasks" :key="t" class="tag">{{ t }}</span><span v-if="!check.gateway && !check.tasks.length" class="muted">nothing routed here</span></dd>
    </dl>
    <ul v-if="check.problems.length || check.warnings.length" class="pcheck__notes">
      <li v-for="p in check.problems" :key="p" class="error">{{ p }}</li>
      <li v-for="w in check.warnings" :key="w" class="muted">{{ w }}</li>
    </ul>
  </div>
</template>

<style scoped>
.pcheck { display: grid; gap: 8px; }
.pcheck__title { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 0; }
.pcheck__notes { margin: 0; padding-left: 18px; display: grid; gap: 4px; font-size: 12.5px; }
.facts { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 4px 14px; margin: 0; }
.facts dt { color: var(--muted); }
.facts dd { margin: 0; min-width: 0; }
.wrap { overflow-wrap: anywhere; }
.small { font-size: 12.5px; }
.tag + .tag { margin-left: 4px; }
</style>
