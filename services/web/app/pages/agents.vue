<script setup lang="ts">
import type { ApiKey } from '~/utils/types'

useHead({ title: 'Agents · CodeSynapse' })
const session = useSession()
const { data: settings } = useApiGet<{ mcp_url?: string }>('settings')
const mcpUrl = computed(() => settings.value?.mcp_url || 'http://localhost:8090/mcp')

// Step 1: a key for this agent (accounts mode). Single-tenant installs use
// the installation's MCP token instead.
const name = ref('')
const key = ref('')
const keyId = ref('')
const error = ref('')
async function createKey() {
  error.value = ''
  try {
    const res = await apiPost<{ key: string; api_key: ApiKey }>('me/keys', { org: session.current.value?.org.slug ?? session.memberships.value[0]?.org.slug, name: name.value || 'agent', ttl_days: 0 })
    key.value = res.key
    keyId.value = res.api_key.id
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
const token = computed(() => key.value || (session.accounts.value ? 'csk_…' : '$CODESYNAPSE_MCP_TOKEN'))

// Step 2: the setup for each agent.
const agent = ref<'claude' | 'codex' | 'cursor' | 'other'>('claude')
const agents = [
  { id: 'claude', label: 'Claude Code' },
  { id: 'codex', label: 'Codex' },
  { id: 'cursor', label: 'Cursor' },
  { id: 'other', label: 'Other (JSON)' },
] as const
const snippet = computed(() => {
  switch (agent.value) {
    case 'claude':
      return `claude mcp add --transport http codesynapse ${mcpUrl.value} \\\n  --header "Authorization: Bearer ${token.value}"`
    case 'codex':
      return `export CODESYNAPSE_MCP_TOKEN=${token.value}\ncodex mcp add codesynapse --url ${mcpUrl.value} --bearer-token-env-var CODESYNAPSE_MCP_TOKEN`
    default:
      return JSON.stringify({ mcpServers: { codesynapse: { url: mcpUrl.value, headers: { Authorization: `Bearer ${token.value}` } } } }, null, 2)
  }
})
const where = computed(() => ({
  claude: 'Run it in a terminal. It registers CodeSynapse for your user; add --scope project to share it through the repository’s .mcp.json (without the key: use an environment variable there).',
  codex: 'Run it in a terminal; Codex reads the key from the environment, so put the export in your shell profile.',
  cursor: 'Paste it into ~/.cursor/mcp.json (all projects) or .cursor/mcp.json in a repository.',
  other: 'Most MCP clients accept this shape: a streamable HTTP server with a bearer token header.',
}[agent.value]))

// Step 3: the key's first use shows the agent is connected.
const { data: keys, refresh: refreshKeys } = useApiGet<{ keys: ApiKey[] }>('me/keys', { immediate: session.accounts.value })
const connected = computed(() => keys.value?.keys.find((k) => k.id === keyId.value)?.last_used_at)
let poll: ReturnType<typeof setInterval> | undefined
watch(keyId, (id) => {
  clearInterval(poll)
  if (id) poll = setInterval(() => { if (!connected.value) refreshKeys() }, 5000)
})
onBeforeUnmount(() => clearInterval(poll))
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Agents" title="Connect an AI agent" lede="Give Claude Code, Codex, Cursor or any MCP client a map of your code. Agents ask CodeSynapse for the few lines they need instead of reading whole files: fewer tokens, better answers, and a security review on every change." />

    <section class="panel step">
      <p class="step__n">1</p>
      <div class="stack--tight">
        <h2>Create a key for this agent</h2>
        <template v-if="session.accounts.value">
          <p class="muted">The key acts as you in <strong>{{ session.current.value?.org.name ?? 'your organisation' }}</strong>: the agent sees that organisation's repositories, and its memory and statistics stay there. You can revoke it any time under <NuxtLink to="/settings/keys">Settings → API keys</NuxtLink>.</p>
          <form v-if="!key" class="row" @submit.prevent="createKey">
            <div class="field grow"><label for="aname">Name it after the machine or agent</label><input id="aname" v-model="name" placeholder="e.g. laptop Claude Code" maxlength="80"></div>
            <button type="submit" class="align-end">Create key</button>
          </form>
          <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
          <div v-if="key" class="stack--tight">
            <p class="notice notice--ok">Created. The key is filled into the setup below; it is shown only on this page, so finish step 2 before you leave.</p>
            <CopyField :value="key" secret label="Your key" />
          </div>
        </template>
        <p v-else class="muted">This installation runs without accounts: agents use the installation's MCP token (<code>CODESYNAPSE_MCP_TOKEN</code> in <code>.env</code>). Ask whoever runs CodeSynapse for it, or run <code>just mcp</code> in the CodeSynapse checkout to set up every agent on this machine.</p>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">2</p>
      <div class="stack--tight">
        <h2>Add CodeSynapse to your agent</h2>
        <div class="row" role="tablist">
          <button v-for="a in agents" :key="a.id" class="chip" :class="{ active: agent === a.id }" role="tab" :aria-selected="agent === a.id" @click="agent = a.id">{{ a.label }}</button>
        </div>
        <CopyField :value="snippet" multiline />
        <p class="muted small">{{ where }}</p>
        <p class="muted small">MCP address: <code>{{ mcpUrl }}</code>. Several agents on one machine? <code>CODESYNAPSE_MCP_TOKEN=csk_… just mcp</code> in the CodeSynapse checkout registers all of them.</p>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">3</p>
      <div class="stack--tight">
        <h2>Check that it works</h2>
        <p class="muted">Start a new session in your agent and ask something about one of your repositories, for example “Use CodeSynapse to show the callers of the main handler”.</p>
        <p v-if="!session.accounts.value || !keyId" class="muted small">With a key from step 1, this page notices the agent's first call.</p>
        <p v-else-if="connected" class="notice notice--ok" role="status">Connected: the agent first used its key {{ relTime(connected) }}.</p>
        <p v-else class="notice" role="status"><span class="dot dot--warn" /> Waiting for the agent's first call…</p>
        <details>
          <summary class="muted small">Make agents use it automatically</summary>
          <div class="stack--tight small">
            <p>CodeSynapse sends its own instructions to agents when they connect. To make them use it first on every task, add this to the repository's <code>AGENTS.md</code> or <code>CLAUDE.md</code>:</p>
            <CopyField multiline value="Use the CodeSynapse MCP tools before reading files: minimal_context with the task first, get_symbol_context before editing a symbol, and get_pr_context on your diff before finishing." />
            <p class="muted">Claude Code can also recall and offload memory automatically: <code>codesynapse mcp hooks</code> installs the hooks. See <NuxtLink to="/help#memory">agent memory</NuxtLink>.</p>
          </div>
        </details>
      </div>
    </section>
  </div>
</template>

<style scoped>
.step { flex-direction: row; gap: 16px; align-items: flex-start; }
.step__n { flex: none; width: 30px; height: 30px; display: grid; place-items: center; border-radius: 50%; background: var(--accent-soft); color: var(--accent-ink); font-weight: 650; }
.step > div { flex: 1; min-width: 0; }
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
</style>
