<script setup lang="ts">
import type { AgentStatus, ApiKey, WorkspaceStats } from '~/utils/types'

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
type AgentID = 'claude' | 'codex' | 'cursor' | 'other'
const selected = ref<AgentID[]>(['claude'])
const agents = [
  { id: 'claude', label: 'Claude Code' },
  { id: 'codex', label: 'Codex' },
  { id: 'cursor', label: 'Cursor' },
  { id: 'other', label: 'Other (JSON)' },
] as const
const selectedAgents = computed(() => agents.filter(a => selected.value.includes(a.id)))
function toggleAgent(id: AgentID) {
  selected.value = [id] // one agent at a time for now
}
const install = 'brew install astrona-io/tap/codesynapse'
const urls = computed(() => `--url ${shellQuote(mcpUrl.value)} --api-url ${shellQuote(apiUrl.value)}`)
function snippet(agent: AgentID) {
  if (viaBridge.value) {
    switch (agent) {
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
  switch (agent) {
    case 'claude':
      return `claude mcp add --transport http codesynapse ${mcpUrl.value} \\\n  --header "Authorization: Bearer ${token.value}"`
    case 'codex':
      return `export CODESYNAPSE_MCP_TOKEN=${token.value}\ncodex mcp add codesynapse --url ${mcpUrl.value} --bearer-token-env-var CODESYNAPSE_MCP_TOKEN`
    default:
      return JSON.stringify({ mcpServers: { codesynapse: { url: mcpUrl.value, headers: { Authorization: `Bearer ${token.value}` } } } }, null, 2)
  }
}
const where = (agent: AgentID) => viaBridge.value
  ? {
      claude: 'Run it in a terminal after installing the bridge. It stores the key in a private file (never in Claude Code’s settings) and registers the bridge, which Claude Code starts once per chat. Then open a new chat and run /mcp.',
      codex: 'The installer registers the local bridge and all lifecycle hooks. Restart Codex, open /hooks and trust the definitions. Each chat is registered automatically.',
      cursor: 'The installer registers the local bridge and all lifecycle hooks. Restart Cursor desktop and keep the MCP hooks enabled in Customize → Hooks. Start Cursor CLI chats with codesynapse cursor.',
      other: 'Clients other than Claude Code have no bridge yet. They must register each chat (POST /api/v1/execution/sessions) and send both session headers on every request; without them the server refuses the call.',
    }[agent]
  : {
      claude: 'Run it in a terminal. It registers CodeSynapse for your user; add --scope project to share it through the repository’s .mcp.json (without the key: use an environment variable there).',
      codex: 'Run it in a terminal; Codex reads the key from the environment, so put the export in your shell profile.',
      cursor: 'Paste it into ~/.cursor/mcp.json (all projects) or .cursor/mcp.json in a repository.',
      other: 'Most MCP clients accept this shape: a streamable HTTP server with a bearer token header.',
    }[agent]

// The last step: what the page can see happen. The key's first use says an
// agent connected; the usage counters (flushed every 30 seconds) say it made
// a call and, from the plugin's hooks, that the plugin runs.
const pluginInstall = 'claude plugin marketplace add astrona-io/codesynapse\nclaude plugin install codesynapse@codesynapse'
const pluginCleanup = 'claude mcp remove codesynapse -s user'
// The same ruleset hooked sessions start with (internal/agentrules), so a
// pasted AGENTS.md matches it, including an operator's own wording.
const { data: rules } = useApiGet<{ text: string }>('agent-rules?for=setup')
const agentRules = computed(() => rules.value?.text || 'Use the CodeSynapse MCP tools before reading files: minimal_context with the task first, get_symbol_context before editing a symbol, and get_pr_context on your diff before finishing.')
const shellQuote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'"
const showPluginStatus = computed(() => viaBridge.value && selected.value.includes('claude'))
const setupAgents = computed(() => selectedAgents.value.filter(a => a.id === 'codex' || a.id === 'cursor' || (a.id === 'claude' && viaBridge.value)))
const stepPlugin = (agent: AgentID) => (viaBridge.value ? 4 : 3) + setupAgents.value.findIndex(a => a.id === agent)
const stepGateway = computed(() => (viaBridge.value ? 4 : 3) + setupAgents.value.length)
const stepCheck = computed(() => stepGateway.value + (session.accounts.value ? 1 : 0))

// Optional: send the agent's model requests through this installation, which
// queues them and answers small ones from a local model. Needs the operator to
// run the API with CODESYNAPSE_GATEWAY=true. The gateway key is the key of step 1.
const gatewayKey = computed(() => key.value || 'csk_…')
const claudeGateway = computed(() => JSON.stringify({ env: { ANTHROPIC_BASE_URL: apiUrl.value, ANTHROPIC_CUSTOM_HEADERS: `X-CodeSynapse-Key: ${gatewayKey.value}` } }, null, 2))
const openaiGateway = computed(() => `OPENAI_BASE_URL=${apiUrl.value}/v1\nOPENAI_API_KEY=${gatewayKey.value}`)
const gatewayTest = computed(() => `curl -s ${shellQuote(apiUrl.value + '/v1/models')} \\\n  -H ${shellQuote('Authorization: Bearer ' + gatewayKey.value)}`)
// The key is also used by MCP calls, so this says "the key was used since this
// page opened", not strictly "a gateway request".
const openedAt = Date.now()
const gatewaySeen = computed(() => {
  const t = keys.value?.keys.find((k) => k.id === keyId.value)?.last_used_at
  return !!t && Date.parse(t) >= openedAt - 1000
})

// Last step: a cap on each chat's context. The agent compacts at the cap;
// with the memory hooks the older turns are offloaded first. The setup
// command sets 150k (agentcfg.DefaultContextCap) unless one is already set.
const contextCaps = [100_000, 150_000, 200_000]
const contextCap = ref(150_000)
const capLabel = (n: number) => `${n / 1000}k`
const claudeCap = computed(() => JSON.stringify({ autoCompactWindow: contextCap.value }, null, 2))
const codexCap = computed(() => `model_auto_compact_token_limit = ${contextCap.value}`)
const capAgents = computed(() => selected.value.filter(a => a === 'claude' || a === 'codex'))
const capCommand = computed(() => `codesynapse mcp install --yes --only ${capAgents.value.join(',')} ${urls.value} --context-cap ${capLabel(contextCap.value)}`)
const { data: keys, refresh: refreshKeys } = useApiGet<{ keys: ApiKey[] }>('me/keys', { immediate: session.accounts.value })
const { data: status, refresh: refreshStatus } = useApiGet<AgentStatus>('agents/status')
// The what-if below uses the recall cost measured on this workspace.
const { data: memStats } = useApiGet<WorkspaceStats>('stats?days=30')
const memModel = computed(() => memStats.value?.memory.model)
// What was already there when the page opened: only newer activity counts.
const base = ref<AgentStatus | null>(null)
watch(status, (v) => { if (v && !base.value) base.value = { ...v } }, { immediate: true })
const connected = computed(() => keys.value?.keys.find((k) => k.id === keyId.value)?.last_used_at)
const firstCall = computed(() => !!status.value && !!base.value && status.value.calls > base.value.calls)
const pluginSeen = computed(() => !!status.value && !!base.value && status.value.workflow > base.value.workflow)
const checks = computed(() => [
  { id: 'key', label: 'Key created', done: !!keyId.value || !session.accounts.value, hint: 'Step 1.' },
  { id: 'connected', label: 'Agent connected', done: !!connected.value || firstCall.value, hint: connected.value ? `The key was first used ${relTime(connected.value)}.` : 'Start a new chat after completing your agent’s connection and session setup.' },
  { id: 'call', label: 'First tool call', done: firstCall.value, hint: firstCall.value ? 'A CodeSynapse tool answered a call.' : 'Ask something about a repository. Calls are counted within a minute.' },
  ...(session.accounts.value && keyId.value ? [{ id: 'gateway', label: 'Gateway key used', done: gatewaySeen.value, hint: gatewaySeen.value ? 'The key was used since you opened this page.' : 'Optional. Run the test in the gateway step; the key counts as used within a minute.' }] : []),
  ...(showPluginStatus.value ? [{ id: 'plugin', label: 'Plugin running', done: pluginSeen.value, hint: pluginSeen.value ? 'Its hooks reported how a session began.' : 'Optional. Appears after a new chat with the plugin installed, once it has looked at code.' }] : []),
])
let poll: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  poll = setInterval(() => {
    if (document.visibilityState !== 'visible') return
    if (keyId.value && (!connected.value || !gatewaySeen.value)) refreshKeys()
    if (!firstCall.value || (showPluginStatus.value && !pluginSeen.value)) refreshStatus()
  }, 5000)
})
onBeforeUnmount(() => clearInterval(poll))
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Agents" title="Connect your AI agents" lede="Give Claude Code, Codex, Cursor or any MCP client a map of your code. Agents ask CodeSynapse for the few lines they need instead of reading whole files: fewer tokens, better answers, and a security review on every change." />

    <section class="panel step">
      <p class="step__n">1</p>
      <div class="stack--tight">
        <h2>Create a key for your agents</h2>
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
        <p class="muted">The bridge connects Claude Code, Codex and Cursor to CodeSynapse. It registers each chat automatically, signs its tool calls with your key and sends them to your server for permission checks and audit logging.</p>
        <CopyField :value="install" label="Homebrew (macOS and Linux)" />
        <p class="muted small">No Homebrew? Download a release binary or use <code>go install</code>. Install the <code>codesynapse</code> binary on every machine where you use these assistants.</p>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">{{ viaBridge ? 3 : 2 }}</p>
      <div class="stack--tight">
        <h2>Add CodeSynapse to your agents</h2>
        <p id="agent-selection-hint" class="muted">Choose the agent you want to connect. Set up one agent at a time; come back to connect the next.</p>
        <div class="row" role="group" aria-label="Agents to connect" aria-describedby="agent-selection-hint">
          <button v-for="a in agents" :key="a.id" type="button" class="chip" :class="{ active: selected.includes(a.id) }" :aria-pressed="selected.includes(a.id)" @click="toggleAgent(a.id)">{{ a.label }}</button>
        </div>
        <p v-if="!selectedAgents.length" class="muted" role="status">Choose an agent to see its setup instructions.</p>
        <div v-for="a in selectedAgents" :key="a.id" class="stack--tight agent-setup">
          <h3>{{ a.label }}</h3>
          <CopyField :value="snippet(a.id)" multiline :label="`${a.label} connection`" />
          <p class="muted small">{{ where(a.id) }}</p>
        </div>
        <p class="muted small">MCP address: <code>{{ mcpUrl }}</code><template v-if="viaBridge"> · API address: <code>{{ apiUrl }}</code></template>.</p>
        <p v-if="viaBridge && apiUrl.includes('<')" class="notice notice--warn" role="status">This installation does not know its public API address. Replace <code>{{ apiUrl }}</code> with the address your API is reached at, or ask the operator to set <code>CODESYNAPSE_PUBLIC_API_URL</code>.</p>
        <details v-if="showPluginStatus">
          <summary class="muted small">Every call reaches the server (and an opt-in for fresh answers)</summary>
          <div class="stack--tight small">
            <p>By default the bridge sends every call to your system: it is signed with your key, checked against your permissions, written to the audit log and counted here. The server's graph reflects the last sync, so it does not know edits you made since. If you prefer fast answers that include uncommitted edits, you can opt in to answering ten core tools from your working tree on your machine; those answers never reach the server, so they are not audited or counted. Set this where Claude Code starts, for example in your shell profile:</p>
            <CopyField value="export CODESYNAPSE_BRIDGE_WORKTREE=true" />
            <p class="muted">Remote servers must use <code>https</code>; the bridge refuses plain <code>http</code> to anything but this machine.</p>
          </div>
        </details>
      </div>
    </section>

    <section v-if="showPluginStatus" class="panel step">
      <p class="step__n">{{ stepPlugin('claude') }}</p>
      <div class="stack--tight">
        <h2>Install the Claude Code plugin <span class="muted">(recommended)</span></h2>
        <p class="muted">The plugin makes agents use CodeSynapse without anyone pasting a prompt: every session starts with a short ruleset (served by this installation), large files are read by range, and a check runs before the agent finishes. It also reports, as counts only, whether sessions begin with CodeSynapse; see <NuxtLink to="/stats/agents">Statistics → Agents</NuxtLink>. Run this once in a terminal:</p>
        <CopyField :value="pluginInstall" multiline label="Shell" />
        <p class="muted small">The plugin starts its own bridge, so remove the one you registered in step 3 to avoid two servers and double hooks. Your key stays in its file and is reused.</p>
        <CopyField :value="pluginCleanup" label="Shell" />
        <p class="muted small">Update the <code>codesynapse</code> program first (<code>brew upgrade codesynapse</code>): the plugin's hooks call commands older versions do not have.</p>
      </div>
    </section>

    <section v-if="selected.includes('codex')" class="panel step">
      <p class="step__n">{{ stepPlugin('codex') }}</p>
      <div class="stack--tight">
        <h2>Use CodeSynapse automatically in Codex</h2>
        <p class="muted">Codex supports plugins that bundle skills, MCP connections and lifecycle hooks. Connecting MCP makes tools available; a hook can run on every prompt to supply context or steer the agent toward CodeSynapse.</p>
        <p class="muted">The setup command above installs the Codex bridge and all 12 lifecycle events for your user. Each chat gets its own identity automatically.</p>
        <p class="muted small">Restart Codex, open <code>/hooks</code>, then review and trust the definitions. Hooks load rules on every prompt and save session metadata. View accepted observations in <NuxtLink to="/stats/activity">Statistics → Live activity</NuxtLink>.</p>
        <p class="muted small">Use a current Codex release that supplies thread metadata on MCP calls. If it is missing, the bridge explains that Codex needs updating.</p>
        <h3>Add the graph-first rules</h3>
        <p class="muted">Add this to your repository’s <code>AGENTS.md</code>, or <code>~/.codex/AGENTS.md</code> for all projects. The rules guide Codex once its MCP connection and runtime identity are working; they do not install hooks.</p>
        <CopyField :value="agentRules" multiline label="AGENTS.md" />
        <details>
          <summary class="muted small">Plugins and hooks for every prompt</summary>
          <div class="stack--tight small">
            <p>The native adapter loads rules at session start, on prompts and after compaction; observes tool use, permission requests, subagents, interruptions and session completion; and reports retrieval counts. It stores event types, timings and sizes, without prompts, source, commands or response text.</p>
            <p>Plugins containing hooks require manual installation in Codex desktop. Review and trust the hook definitions before they can run; plugins with lifecycle hooks are not eligible for the public plugin directory.</p>
            <p>See OpenAI’s <a href="https://developers.openai.com/plugins/build/plugins" target="_blank" rel="noopener noreferrer">plugin packaging guide</a> and <a href="https://learn.chatgpt.com/docs/hooks" target="_blank" rel="noopener noreferrer">hook documentation</a>.</p>
          </div>
        </details>
      </div>
    </section>

    <section v-if="selected.includes('cursor')" class="panel step">
      <p class="step__n">{{ stepPlugin('cursor') }}</p>
      <div class="stack--tight">
        <h2>Use CodeSynapse automatically in Cursor</h2>
        <p class="muted">Cursor supports plugins that bundle rules, skills, MCP connections and hooks. A <code>beforeSubmitPrompt</code> hook can run on every prompt; connecting MCP alone makes the tools available without guaranteeing their use.</p>
        <p class="muted">The setup command above installs the Cursor bridge and lifecycle hooks for your user. Each MCP invocation is associated with its chat automatically.</p>
        <p class="muted small">Restart Cursor and inspect Customize → Hooks. Session hooks load rules and save metadata, including compaction context usage when reported. View accepted observations in <NuxtLink to="/stats/activity">Statistics → Live activity</NuxtLink>.</p>
        <p class="muted small">Use a current local Cursor release with MCP call IDs in its hooks, and keep the installed hooks enabled. If the required correlation is unavailable, the bridge refuses the call and explains how to update the setup.</p>
        <p class="muted">For Cursor CLI, start each chat with <code>codesynapse cursor</code>, or resume with <code>codesynapse cursor --resume &lt;existing-chat-id&gt;</code>. Desktop uses its normal chat interface. Relaunch the CLI to switch chats.</p>
        <h3>Add the graph-first rules</h3>
        <p class="muted">Add this to your repository’s <code>AGENTS.md</code> or an always-applied Cursor rule. It guides Cursor once its MCP connection and runtime identity are working; it does not install hooks.</p>
        <CopyField :value="agentRules" multiline label="AGENTS.md or Cursor rule" />
        <details>
          <summary class="muted small">Plugins and hooks for every prompt</summary>
          <div class="stack--tight small">
            <p>The native adapter observes all 21 Cursor event types. Events need a runtime chat identity to be saved; workspace startup and Tab events without one remain outside session activity. Generic and specialized hooks may observe the same operation.</p>
            <p>See Cursor’s <a href="https://cursor.com/docs/hooks" target="_blank" rel="noopener noreferrer">hook documentation</a>. Hooks leave permission decisions with Cursor and never auto-approve requests.</p>
          </div>
        </details>
      </div>
    </section>

    <section v-if="session.accounts.value" class="panel step">
      <p class="step__n">{{ stepGateway }}</p>
      <div class="stack--tight">
        <h2>Send model requests through CodeSynapse <span class="muted">(optional)</span></h2>
        <p class="muted">With the gateway on, every request your agent makes to its model goes to CodeSynapse first. It waits its turn in a queue, small requests are answered by a local model, and the rest is passed on to the cloud with your own credential, which is never stored. Each request is checked against your key and audited. The operator must run the API with <code>CODESYNAPSE_GATEWAY=true</code>; until then these requests are refused.</p>
        <template v-if="selected.includes('claude')">
          <h3>Claude Code</h3>
          <p class="muted small">Merge this into <code>~/.claude/settings.json</code>. Claude Code keeps using your own Anthropic sign-in; only the address and the header change.</p>
          <CopyField :value="claudeGateway" multiline label="settings.json" />
        </template>
        <template v-else>
          <h3>Local agents (OpenAI-compatible)</h3>
          <p class="muted small">Set these where the agent starts. These requests are always answered by the local model, never the cloud.</p>
          <CopyField :value="openaiGateway" multiline label="Environment" />
        </template>
        <p v-if="!key" class="muted small">Create the key in step 1 first: it is filled into these snippets.</p>
        <h3>Test it</h3>
        <p class="muted small">Lists the local model. A 401 means the key is wrong; a 503 means no local model has <code>gateway: true</code> yet.</p>
        <CopyField :value="gatewayTest" multiline label="Shell" />
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">{{ stepCheck }}</p>
      <div class="stack--tight">
        <h2>Check that it works</h2>
        <p class="muted">After setup, launch CLI chats with <code>codesynapse claude</code>, <code>codesynapse codex</code>, or <code>codesynapse cursor</code>. Open desktop apps normally.</p>
        <p class="muted">Start a <strong>new</strong> session in each selected agent (chats that were already open keep their old setup) and ask something about one of your repositories, for example “Use CodeSynapse to show the callers of the main handler”. These checks show activity across your agents; they do not verify each connection separately.</p>
        <ul class="checks" aria-live="polite">
          <li v-for="c in checks" :key="c.id" :class="{ done: c.done }">
            <span class="checks__mark" aria-hidden="true">{{ c.done ? '✓' : '' }}</span>
            <span><strong>{{ c.label }}</strong> <span class="muted small">{{ c.hint }}</span></span>
          </li>
        </ul>
        <details v-if="selected.includes('claude') || selected.includes('other')">
          <summary class="muted small">Make agents use it automatically</summary>
          <div class="stack--tight small">
            <p>CodeSynapse sends its own instructions to agents when they connect. The Claude Code plugin goes further: it puts a short ruleset in every session and reports on Statistics whether agents then start with CodeSynapse. Without the plugin, add this to the repository's <code>AGENTS.md</code> or <code>CLAUDE.md</code>, or to <code>~/.claude/CLAUDE.md</code> for every project:</p>
            <CopyField :value="agentRules" multiline />
            <p class="muted">Claude Code can also recall and offload memory automatically: <code>codesynapse mcp hooks</code> installs the hooks. See <NuxtLink to="/help#memory">agent memory</NuxtLink>.</p>
          </div>
        </details>
      </div>
    </section>

    <section class="panel step">
      <p class="step__n">{{ stepCheck + 1 }}</p>
      <div class="stack--tight">
        <h2>Keep each chat small</h2>
        <p class="muted">An agent keeps the whole conversation in context until it compacts it, and every prompt pays for all of it again. Models with a million-token window compact only near the end of it. With the Claude Code plugin or <code>codesynapse mcp hooks</code>, the older turns go to <NuxtLink to="/help#memory">agent memory</NuxtLink> before each compaction and come back when a prompt needs them, so a small limit loses little.</p>
        <template v-if="capAgents.length">
          <p class="muted">The setup command above already makes Claude Code and Codex compact at <strong>150k tokens</strong>, and keeps a limit you set yourself. To pick another, choose it and run the command:</p>
          <div class="row" role="group" aria-label="Context limit">
            <button v-for="n in contextCaps" :key="n" type="button" class="chip" :class="{ active: contextCap === n }" :aria-pressed="contextCap === n" @click="contextCap = n">{{ capLabel(n) }} tokens</button>
          </div>
          <CopyField :value="capCommand" multiline label="Shell" />
          <p class="muted small"><code>--context-cap 0</code> leaves the agents' own settings alone. Codex summarises older turns itself; its hooks do not offload them to agent memory yet.</p>
          <details>
            <summary class="muted small">Set it by hand</summary>
            <div class="stack--tight small">
              <template v-if="selected.includes('claude')">
                <p>Claude Code: add this to <code>~/.claude/settings.json</code>, or to a project's <code>.claude/settings.json</code> (100k to 1M). For one chat only, type <code>/autocompact {{ capLabel(contextCap) }}</code>. Remove <code>CLAUDE_AUTOCOMPACT_PCT_OVERRIDE</code> from the same file: it is a percentage of this limit, so 30 would compact at {{ capLabel(contextCap * 0.3) }}.</p>
                <CopyField :value="claudeCap" multiline label="settings.json" />
              </template>
              <template v-if="selected.includes('codex')">
                <p>Codex: add this to <code>~/.codex/config.toml</code>, above the first <code>[section]</code>.</p>
                <CopyField :value="codexCap" label="config.toml" />
              </template>
            </div>
          </details>
        </template>
        <p v-else class="muted small">Claude Code and Codex have a setting for this. Choose one of them above to see it.</p>
        <details v-if="memModel" class="what-if">
          <summary>Why a smaller limit saves tokens: try it</summary>
          <p class="muted small">A what-if for one long session: every prompt re-sends the whole conversation, so its size over all turns is what you pay for. Change the numbers to match how you work; nothing here is saved or configured. The recall cost is measured on this workspace once agents have recalled facts.</p>
          <OffloadModel :tokens-per-recall="memModel.tokens_per_recall" :recalls-per-session="memModel.recalls_per_session" :tokens-per-fact="memModel.tokens_per_fact" :measured="memModel.measured" />
        </details>
      </div>
    </section>
  </div>
</template>

<style scoped>
.what-if { margin-top: 16px; }
.what-if summary { cursor: pointer; font-weight: 600; }
.step { flex-direction: row; gap: 16px; align-items: flex-start; }
.step__n { flex: none; width: 30px; height: 30px; display: grid; place-items: center; border-radius: 50%; background: var(--accent-soft); color: var(--accent-ink); font-weight: 650; }
.step > div { flex: 1; min-width: 0; }
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.agent-setup { margin-top: 12px; }
.checks { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.checks li { display: flex; gap: 12px; align-items: flex-start; padding: 10px 12px; border: 2px solid var(--line); border-radius: var(--radius-sm); }
.checks li.done { background: var(--success-soft); }
.checks__mark { flex: none; width: 22px; height: 22px; border-radius: var(--pill); border: 2px solid var(--line-strong); display: grid; place-items: center; font-weight: 800; font-size: 13px; }
.checks li.done .checks__mark { border-color: var(--success); color: var(--success); }
</style>
