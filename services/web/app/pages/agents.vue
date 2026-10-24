<script setup lang="ts">
import type { AgentStatus, ApiKey } from '~/utils/types'

useHead({ title: 'Agents · CodeSynapse' })
const session = useSession()
const { data: settings } = useApiGet<{ mcp_url?: string; api_url?: string }>('settings')
const mcpUrl = computed(() => settings.value?.mcp_url || 'http://localhost:8090/mcp')
// Where the bridge registers each chat. Unknown on a remote install until the operator sets CODESYNAPSE_PUBLIC_API_URL.
const apiUrl = computed(() => settings.value?.api_url || (/^https?:\/\/(localhost|127\.0\.0\.1)[:/]/.test(mcpUrl.value) ? 'http://127.0.0.1:8080' : 'https://<your-api-host>'))
const viaBridge = computed(() => session.accounts.value)

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

// Step 2: install the bridge. Step 3: register it with each agent.
const agent = ref<'claude' | 'codex' | 'cursor' | 'other'>('claude')
const agents = [
  { id: 'claude', label: 'Claude Code' },
  { id: 'codex', label: 'Codex' },
  { id: 'cursor', label: 'Cursor' },
  { id: 'other', label: 'Other (JSON)' },
] as const
const install = 'brew install astrona-io/tap/codesynapse'
const urls = computed(() => `--url ${mcpUrl.value} --api-url ${apiUrl.value}`)
const snippet = computed(() => {
  if (viaBridge.value) {
    switch (agent.value) {
      case 'claude':
        return `CODESYNAPSE_MCP_TOKEN=${token.value} codesynapse mcp install --only claude ${urls.value}`
      case 'codex':
        return `CODESYNAPSE_MCP_TOKEN=${token.value} codesynapse mcp install --only codex ${urls.value}`
      case 'cursor':
        return `CODESYNAPSE_MCP_TOKEN=${token.value} codesynapse mcp install --only cursor ${urls.value}`
      default:
        return JSON.stringify({ mcpServers: { codesynapse: { url: mcpUrl.value, headers: { Authorization: `Bearer ${token.value}`, 'X-CodeSynapse-Session': '${CODESYNAPSE_SESSION_ID}', 'X-CodeSynapse-Parent-Session': '${CODESYNAPSE_PARENT_SESSION_ID}' } } } }, null, 2)
    }
  }
  switch (agent.value) {
    case 'claude':
      return `claude mcp add --transport http codesynapse ${mcpUrl.value} \\\n  --header "Authorization: Bearer ${token.value}"`
    case 'codex':
      return `export CODESYNAPSE_MCP_TOKEN=${token.value}\ncodex mcp add codesynapse --url ${mcpUrl.value} --bearer-token-env-var CODESYNAPSE_MCP_TOKEN`
    default:
      return JSON.stringify({ mcpServers: { codesynapse: { url: mcpUrl.value, headers: { Authorization: `Bearer ${token.value}` } } } }, null, 2)
  }
})
const where = computed(() => viaBridge.value
  ? {
      claude: 'Run it in a terminal after installing the bridge. It stores the key in a private file (never in Claude Code’s settings) and registers the bridge, which Claude Code starts once per chat. Then open a new chat and run /mcp.',
      codex: 'Run it in a terminal. Codex connects over HTTP without the bridge, so a launcher must register the chat and supply CODESYNAPSE_SESSION_ID and CODESYNAPSE_PARENT_SESSION_ID before it starts.',
      cursor: 'Run it in a terminal. Cursor connects over HTTP without the bridge, so a launcher must register the chat and supply the two session variables before it starts.',
      other: 'Clients other than Claude Code have no bridge yet. They must register each chat (POST /api/v1/execution/sessions) and send both session headers on every request; without them the server refuses the call.',
    }[agent.value]
  : {
      claude: 'Run it in a terminal. It registers CodeSynapse for your user; add --scope project to share it through the repository’s .mcp.json (without the key: use an environment variable there).',
      codex: 'Run it in a terminal; Codex reads the key from the environment, so put the export in your shell profile.',
      cursor: 'Paste it into ~/.cursor/mcp.json (all projects) or .cursor/mcp.json in a repository.',
      other: 'Most MCP clients accept this shape: a streamable HTTP server with a bearer token header.',
    }[agent.value])

// The last step: what the page can see happen. The key's first use says an
// agent connected; the usage counters (flushed every 30 seconds) say it made
// a call and, from the plugin's hooks, that the plugin runs.
const pluginInstall = 'claude plugin marketplace add astrona-io/codesynapse\nclaude plugin install codesynapse@codesynapse'
const pluginCleanup = 'claude mcp remove codesynapse -s user'
const stepPlugin = 4
const stepCheck = computed(() => (viaBridge.value ? 5 : 3))
const { data: keys, refresh: refreshKeys } = useApiGet<{ keys: ApiKey[] }>('me/keys', { immediate: session.accounts.value })
const { data: status, refresh: refreshStatus } = useApiGet<AgentStatus>('agents/status')
// What was already there when the page opened: only newer activity counts.
const base = ref<AgentStatus | null>(null)
watch(status, (v) => { if (v && !base.value) base.value = { ...v } }, { immediate: true })
const connected = computed(() => keys.value?.keys.find((k) => k.id === keyId.value)?.last_used_at)
const firstCall = computed(() => !!status.value && !!base.value && status.value.calls > base.value.calls)
const pluginSeen = computed(() => !!status.value && !!base.value && status.value.workflow > base.value.workflow)
const checks = computed(() => [
  { id: 'key', label: 'Key created', done: !!keyId.value || !session.accounts.value, hint: 'Step 1.' },
  { id: 'connected', label: 'Agent connected', done: !!connected.value || firstCall.value, hint: connected.value ? `The key was first used ${relTime(connected.value)}.` : 'Start a new chat in your agent; the bridge connects when it starts.' },
  { id: 'call', label: 'First tool call', done: firstCall.value, hint: firstCall.value ? 'A CodeSynapse tool answered a call.' : 'Ask something about a repository. Calls are counted within a minute.' },
  ...(viaBridge.value ? [{ id: 'plugin', label: 'Plugin running', done: pluginSeen.value, hint: pluginSeen.value ? 'Its hooks reported how a session began.' : 'Optional. Appears after a new chat with the plugin installed, once it has looked at code.' }] : []),
])
let poll: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  poll = setInterval(() => {
    if (document.visibilityState !== 'visible') return
    if (keyId.value && !connected.value) refreshKeys()
    if (!firstCall.value || (viaBridge.value && !pluginSeen.value)) refreshStatus()
  }, 5000)
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
          <p class="muted">The key acts as you in <strong>{{ session.current.value?.org.name ?? 'your organisation' }}</strong>: the agent sees repositories you can access there, and its memory and statistics stay there. You can revoke it any time under <NuxtLink to="/settings/keys">Settings → API keys</NuxtLink>.</p>
          <form v-if="!key" class="row" @submit.prevent="createKey">
            <div class="field grow"><label for="aname">Name it after the machine or agent</label><input id="aname" v-model="name" placeholder="e.g. laptop Claude Code" maxlength="80"></div>
            <button type="submit" class="align-end">Create key</button>
          </form>
          <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
          <div v-if="key" class="stack--tight">
            <p class="notice notice--ok">Created. The key is filled into the setup below; it is shown only on this page, so finish the steps below before you leave.</p>
            <CopyField :value="key" secret label="Your key" />
          </div>
        </template>
        <p v-else class="muted">This installation runs without accounts: agents use the installation's MCP token (<code>CODESYNAPSE_MCP_TOKEN</code> in <code>.env</code>). Ask whoever runs CodeSynapse for it, or run <code>just mcp</code> in the CodeSynapse checkout to set up every agent on this machine.</p>
      </div>
    </section>

    <section v-if="viaBridge" class="panel step">
      <p class="step__n">2</p>
      <div class="stack--tight">
        <h2>Install the CodeSynapse bridge</h2>
        <p class="muted">The bridge is a small program on your machine that sits between your agent and CodeSynapse. It is what makes <strong>every request go through your system</strong>: it signs each call with your key, registers each chat as its own session, and forwards the call to your server, where it is checked against your permissions and written to the audit log. Pointing an agent straight at the MCP address is not enough: Claude Code cannot send the session identity itself, so the server refuses it.</p>
        <CopyField :value="install" label="Homebrew (macOS and Linux)" />
        <p class="muted small">No Homebrew? Download a release binary or use <code>go install</code>: see the Claude Code bridge guide in the documentation. Every machine that runs Claude Code needs the <code>codesynapse</code> binary.</p>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">{{ viaBridge ? 3 : 2 }}</p>
      <div class="stack--tight">
        <h2>{{ viaBridge ? 'Register the bridge with your agent' : 'Add CodeSynapse to your agent' }}</h2>
        <div class="row" role="tablist">
          <button v-for="a in agents" :key="a.id" class="chip" :class="{ active: agent === a.id }" role="tab" :aria-selected="agent === a.id" @click="agent = a.id">{{ a.label }}</button>
        </div>
        <CopyField :value="snippet" multiline />
        <p class="muted small">{{ where }}</p>
        <p class="muted small">MCP address: <code>{{ mcpUrl }}</code><template v-if="viaBridge"> · API address: <code>{{ apiUrl }}</code></template>. Several agents on one machine? Leave out <code>--only</code> and it offers every agent it finds.</p>
        <p v-if="viaBridge && apiUrl.includes('<')" class="notice notice--warn" role="status">This installation does not know its public API address. Replace <code>{{ apiUrl }}</code> with the address your API is reached at, or ask the operator to set <code>CODESYNAPSE_PUBLIC_API_URL</code>.</p>
        <details v-if="viaBridge">
          <summary class="muted small">Every call reaches the server (and an opt-in for fresh answers)</summary>
          <div class="stack--tight small">
            <p>By default the bridge sends every call to your system: it is signed with your key, checked against your permissions, written to the audit log and counted here. The server's graph reflects the last sync, so it does not know edits you made since. If you prefer fast answers that include uncommitted edits, you can opt in to answering ten core tools from your working tree on your machine; those answers never reach the server, so they are not audited or counted. Set this where Claude Code starts, for example in your shell profile:</p>
            <CopyField value="export CODESYNAPSE_BRIDGE_WORKTREE=true" />
            <p class="muted">Remote servers must use <code>https</code>; the bridge refuses plain <code>http</code> to anything but this machine.</p>
          </div>
        </details>
      </div>
    </section>

    <section v-if="viaBridge && agent === 'claude'" class="panel step">
      <p class="step__n">{{ stepPlugin }}</p>
      <div class="stack--tight">
        <h2>Install the Claude Code plugin <span class="muted">(recommended)</span></h2>
        <p class="muted">The plugin makes agents use CodeSynapse without anyone pasting a prompt: every session starts with a short ruleset (served by this installation), large files are read by range, and a check runs before the agent finishes. It also reports, as counts only, whether sessions begin with CodeSynapse; see <NuxtLink to="/stats/agents">Statistics → Agents</NuxtLink>. Run this once in a terminal:</p>
        <CopyField :value="pluginInstall" multiline label="Shell" />
        <p class="muted small">The plugin starts its own bridge, so remove the one you registered in step 3 to avoid two servers and double hooks. Your key stays in its file and is reused.</p>
        <CopyField :value="pluginCleanup" label="Shell" />
        <p class="muted small">Update the <code>codesynapse</code> program first (<code>brew upgrade codesynapse</code>): the plugin's hooks call commands older versions do not have.</p>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">{{ stepCheck }}</p>
      <div class="stack--tight">
        <h2>Check that it works</h2>
        <p class="muted">Start a <strong>new</strong> session in your agent (chats that were already open keep their old setup) and ask something about one of your repositories, for example “Use CodeSynapse to show the callers of the main handler”.</p>
        <ul class="checks" aria-live="polite">
          <li v-for="c in checks" :key="c.id" :class="{ done: c.done }">
            <span class="checks__mark" aria-hidden="true">{{ c.done ? '✓' : '' }}</span>
            <span><strong>{{ c.label }}</strong> <span class="muted small">{{ c.hint }}</span></span>
          </li>
        </ul>
        <details>
          <summary class="muted small">Make agents use it automatically</summary>
          <div class="stack--tight small">
            <p>CodeSynapse sends its own instructions to agents when they connect. The Claude Code plugin goes further: it puts a short ruleset in every session and reports on Statistics whether agents then start with CodeSynapse. Without the plugin, add this to the repository's <code>AGENTS.md</code> or <code>CLAUDE.md</code>, or to <code>~/.claude/CLAUDE.md</code> for every project:</p>
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
.checks { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.checks li { display: flex; gap: 12px; align-items: flex-start; padding: 10px 12px; border: 2px solid var(--line); border-radius: var(--radius-sm); }
.checks li.done { background: var(--success-soft); }
.checks__mark { flex: none; width: 22px; height: 22px; border-radius: var(--pill); border: 2px solid var(--line-strong); display: grid; place-items: center; font-weight: 800; font-size: 13px; }
.checks li.done .checks__mark { border-color: var(--success); color: var(--success); }
</style>
