import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { stripTypeScriptTypes } from 'node:module'
import { test } from 'node:test'
import { createSSRApp, computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { parse, compileScript } from 'vue/compiler-sfc'
import { localSaved, totalSaved } from '../utils/savings.ts'
import { windowLabel } from '../composables/useStatistics.ts'

// Nuxt auto-imports these; useId only has to be stable within one render.
let nextId = 0
const useId = () => `v-${nextId++}`

async function compileComponent(t, file) {
  const source = await readFile(new URL(file, import.meta.url), 'utf8')
  const { descriptor } = parse(source)
  let script = compileScript(descriptor, { id: file, inlineTemplate: true }).content
  script = stripTypeScriptTypes(script).replace(/from ["']vue["']/g, `from ${JSON.stringify(import.meta.resolve('vue'))}`)
  script = script.replace(/import \{ useStatistics \} from ['"]~\/composables\/useStatistics['"]/g, 'const useStatistics = globalThis.useStatistics')
  // A data: URL is imported once per text: keep each render's module, and
  // the stubs it captured, its own.
  script += `\n// ${t.fullName}`
  const { default: component } = await import(`data:text/javascript;base64,${Buffer.from(script).toString('base64')}`)
  return component
}

async function renderComponent(t, file, globals, props = {}) {
  for (const [key, value] of Object.entries({ computed, ref, watch, useId, ...globals })) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, key)
    Object.defineProperty(globalThis, key, { value, configurable: true })
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, key, previous)
      else delete globalThis[key]
    })
  }
  const component = await compileComponent(t, file)
  const setup = component.setup
  component.setup = async (p, ctx) => {
    const render = setup(p, ctx)
    await new Promise((resolve) => setImmediate(resolve))
    return render
  }
  const app = createSSRApp(component, props)
  Object.assign(app.config.globalProperties, globals)
  app.component('PageHeader', { template: '<header />' })
  app.component('GraphCanvas', { template: '<div />' })
  app.component('StatTile', { props: ['label', 'value', 'hint', 'tip'], template: '<div>{{ label }} {{ value }} {{ hint }} <i>{{ tip }}</i></div>' })
  app.component('ColumnChart', { template: '<div />' })
  app.component('RankList', { template: '<div />' })
  app.component('InfoTip', { props: ['text', 'term', 'label'], template: '<i class="info">{{ text }}</i>' })
  app.component('NuxtLink', { template: '<a><slot /></a>' })
  app.component('DataState', await compileComponent(t, './DataState.vue'))
  return renderToString(app)
}

test('empty audit responses render an empty state when events is null', async (t) => {
  const html = await renderComponent(t, './AuditLog.vue', {
    useApiGet: () => ({ data: ref({ events: null }), error: ref(null), pending: ref(false) }),
    apiPath: (p) => p,
  }, { base: 'admin/audit' })
  assert.match(html, /No entries/)
})

test('a failed graph does not hide a valid symbol card or crash on null relation lists', async (t) => {
  const html = await renderComponent(t, '../pages/repos/[repo]/explore.vue', {
    useRoute: () => ({ params: { repo: 'acme/app' }, query: { id: 'symbol-1' } }),
    useRouter: () => ({ push() {} }),
    repoPath: (repo, rest) => `repos/${encodeURIComponent(repo)}/${rest}`,
    apiGet: async (path) => {
      if (path.includes('/graph?')) throw new Error('Graph timed out')
      return { symbol: { id: 'symbol-1', name: 'Handler', kind: 'Function' }, callers: null, callees: null, tests: null }
    },
    apiErrorMessage: (e) => e.message,
    shortName: (n) => n.name,
    location: () => 'app.go:10',
  })
  assert.match(html, /Handler/)
  assert.match(html, /Call neighbourhood unavailable: Graph timed out/)
  assert.match(html, /This symbol is not reached by any test/)
})

test('statistics renders missing token and edit fields from an older API', async (t) => {
  const html = await renderComponent(t, '../pages/stats/agents.vue', {
    useStatistics: () => ({
      days: ref(30), error: ref(null), pending: ref(false),
      s: ref({ agents: { sessions: 0, calls: 0, errors: 0, response_tokens: 0, avoided_tokens: 0, tools: [], clients: [], repos: [], daily: [], workflow: { first_codesynapse: 0, first_native: 0 } } }),
    }),
    fmtCompact: (n) => String(n),
    useModelUsage: () => ({ ready: ref(false), usageOf: () => null }),
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
  })
  assert.match(html, /Model usage unavailable/)
  assert.match(html, /Small edits and output/)
  assert.doesNotMatch(html, /Local models/, 'no AI providers in an older API, no local-model section')
})

test('statistics show the server share of agent work, or why it is unknown', async (t) => {
  const base = { sessions: 1, calls: 10, errors: 0, response_tokens: 1000, avoided_tokens: 3000, context_total_tokens: 4000, context_saved_pct: 75,
    tokens: { input: 2000000, output: 50000, cached_input: 1900000, cache_write: 20000, turns: 10, cache_known_turns: 10, total: 2050000, input_pct: 98, output_pct: 2 },
    tools: [], clients: [], repos: [], daily: [], workflow: { first_codesynapse: 0, first_native: 0 } }
  const render = (st, agents) => renderComponent(st, '../pages/stats/agents.vue', {
    useStatistics: () => ({ days: ref(30), error: ref(null), pending: ref(false), s: ref({ agents }) }),
    fmtCompact: (n) => String(n),
    fmtInt: (n) => String(n),
    useModelUsage: () => ({ ready: ref(false), usageOf: () => null }),
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
  })
  await t.test('known', async (st) => {
    const html = await render(st, { ...base, work_tokens: 150000, share_avoided_tokens: 3000, share_avoided_pct: 2, share_avoided_known: true,
      share_avoided_basis: 'work_tokens', share_upper_pct: 2.5, share_raw_pct: 0.1, avoided_repeat_ranges: 4 })
    assert.match(html, /Work tokens 150000 /)
    assert.match(html, /Share of agent work avoided 2% /)
    assert.match(html, /Upper figure: .*2\.5%/, 'the upper figure is labelled and apart')
    assert.doesNotMatch(html, /Share of agent work avoided 2\.5%/)
    assert.match(html, /Smaller than reading the files 75% /)
  })
  await t.test('unknown', async (st) => {
    const html = await render(st, { ...base, work_tokens: 150000, share_avoided_pct: null, share_avoided_known: false, share_avoided_basis: 'cache_fields_missing', share_upper_pct: null })
    assert.match(html, /Share of agent work avoided unknown +<i>some turns did not report their prompt-cache fields/)
    assert.doesNotMatch(html, /Upper figure/)
  })
})

test('statistics show local model work from the metrics store', async (t) => {
  const usage = { 'local-ollama': { requests: 40, failures: 1, input: 9000, output: 1000 }, claude: { requests: 10, failures: 0, input: 500, output: 100 } }
  const html = await renderComponent(t, '../pages/stats/agents.vue', {
    useStatistics: () => ({
      days: ref(30), error: ref(null), pending: ref(false),
      s: ref({
        agents: { sessions: 0, calls: 0, errors: 0, response_tokens: 0, avoided_tokens: 0, tools: [], clients: [], repos: [], daily: [], workflow: { first_codesynapse: 0, first_native: 0 } },
        ai: [
          { provider: 'local-ollama', model: 'qwen3.8:27b', local: true, usage: { requests: 0, failures: 0, input_tokens: 0, output_tokens: 0 } },
          { provider: 'claude', model: 'claude-opus-5-5', local: false, usage: { requests: 0, failures: 0, input_tokens: 0, output_tokens: 0 } },
        ],
      }),
    }),
    fmtCompact: (n) => String(n),
    useModelUsage: () => ({ ready: ref(true), usageOf: (name) => usage[name] }),
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
  })
  assert.match(html, /AI work that stayed on your network/)
  assert.match(html, /qwen3\.8:27b/)
  assert.match(html, /Local requests 40 /, 'requests from the metrics store, not the API process')
  assert.match(html, /Tokens processed locally 10000 /, 'input + output tokens')
  assert.match(html, /Share of AI requests 80% /, '40 of 50 requests stayed local')
})

test('models and savings split the token pool without counting ask_local twice', async (t) => {
  const models = {
    range: '30d',
    reference_price: { provider: 'claude', model: 'claude-opus-5-5', input_per_mtok: 10, output_per_mtok: 20 },
    spent_dollars: 0.25, compression_saved_dollars: 0.5, compression_enabled: true,
    gateway: { requests: 10, local: 6, cloud: 3, rejected: 1, failed: 1, local_share_pct: 60, bytes_in: 2000, bytes_out: 900, local_input_tokens: 10000, local_output_tokens: 2500, local_unmetered: 0, saved_dollars: 0.25 },
    models: [
      { provider: 'local-ollama', model: 'qwen3.8:27b', local: true, configured: true, context_window: 262144, requests: 40, failures: 0, input_tokens: 60000, output_tokens: 5000, avg_prompt_tokens: 1500,
        read_tokens_per_second: 264, write_tokens_per_second: 32.8, context_p50: 0.02, context_p95: 0.2,
        tasks: [{ task: 'local-answer', requests: 30, failures: 0, input_tokens: 50000, output_tokens: 3000 }, { task: 'review', requests: 10, failures: 0, input_tokens: 10000, output_tokens: 2000 }] },
      { provider: 'claude', model: 'claude-opus-5-5', local: false, configured: true, requests: 2, failures: 0, input_tokens: 4000, output_tokens: 1000, avg_prompt_tokens: 2000,
        read_tokens_per_second: null, write_tokens_per_second: null, context_p50: null, context_p95: null, tasks: [{ task: 'briefing', requests: 2, failures: 0, input_tokens: 4000, output_tokens: 1000 }] },
    ],
  }
  const html = await renderComponent(t, '../pages/stats/models.vue', {
    useStatistics: () => ({
      days: ref(30), error: ref(null), pending: ref(false),
      s: ref({ agents: { tokens: { total: 100000 }, work_tokens: 100000, avoided_tokens: 50000, local_avoided_tokens: 20000,
        saved: { tokens: 30000, price: 'input', per_mtok: 10, dollars: 0.3, resend_tokens: 0, upper_dollars: null, unaware_tokens: 0, unaware_graph_tokens: 0, unaware_dollars: 0, total_dollars: 0.3 } } }),
    }),
    useApiGet: (url) => ({ data: ref(url().startsWith('stats/savings') ? null : models), error: ref(null), pending: ref(false) }),
    localSaved, totalSaved, windowLabel,
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
    fmtCompact: (n) => String(n),
    fmtInt: (n) => String(n),
    apiErrorMessage: (e) => String(e),
  }, {})
  assert.match(html, /Kept off cloud models 48% /, '30k never read + 65k local of a 200k pool')
  assert.match(html, /Tokens never read 50000 /)
  assert.match(html, /Handled by local models 65000 60000 read · 5000 written/)
  assert.match(html, /Total saved \(est\.\) \$1\.75 at claude-opus-5-5 prices <i>estimated<\/i>/, '60k × $10 + 5k × $20 per million, $0.30 for graph answers from the server, plus $0.50 from shorter prompts and $0.25 from gateway answers')
  assert.match(html, /Saved by graph answers \$0\.30 30000 tokens never read <i>30000 tokens never read, each once at the input price/)
  assert.match(html, /Saved on cached input none reported +<i>the local runtime reported no prompt-cache hits/)
  assert.match(html, /Spent on cloud models \(est\.\) \$0\.25 /)
  assert.match(html, /Saved by shorter prompts \$0\.50 +<i>CodeSynapse&#39;s own AI tasks/)
  assert.match(html, /gateway traffic is passed on unchanged/)
  assert.match(html, /Gateway requests 10 6 local · 3 cloud · 1 refused · 1 failed/)
  assert.match(html, /Answered locally 60% /)
  assert.match(html, /Saved by local answers \$0\.25 /)
})

test('shorter prompts tile says why it is off instead of showing $0', async (t) => {
  const models = {
    range: '30d',
    reference_price: { provider: 'claude', model: 'claude-opus-5-5', input_per_mtok: 10, output_per_mtok: 20 },
    spent_dollars: 0, compression_saved_dollars: 0, compression_enabled: false, compression_off: 'no_local_model',
    models: [],
  }
  const html = await renderComponent(t, '../pages/stats/models.vue', {
    useStatistics: () => ({ days: ref(30), error: ref(null), pending: ref(false), s: ref({ agents: { tokens: { total: 0 } } }) }),
    useApiGet: (url) => ({ data: ref(url().startsWith('stats/savings') ? null : models), error: ref(null), pending: ref(false) }),
    localSaved, totalSaved, windowLabel,
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
    fmtCompact: (n) => String(n),
    fmtInt: (n) => String(n),
    apiErrorMessage: (e) => String(e),
  }, {})
  assert.match(html, /Saved by shorter prompts off +<i>no local model to shorten with; covers CodeSynapse&#39;s own AI tasks, not gateway traffic/)
  assert.doesNotMatch(html, /Saved by shorter prompts \$0\.00/)
})

test('graph answers of agents without repeat reports are valued, and local cache hits are shown', async (t) => {
  const models = {
    range: '30d',
    reference_price: { provider: 'built-in', model: 'Claude Opus 5.5 (list price estimate)', input_per_mtok: 4, output_per_mtok: 20, cached_input_per_mtok: 0.2, cache_write_per_mtok: 5, caches_by_default: true, estimate: true, source: 'https://platform.claude.com/docs/en/about-claude/pricing', checked: '2026-10-29' },
    spent_dollars: 0, compression_saved_dollars: 0, compression_enabled: true,
    models: [{ provider: 'local', model: 'q', local: true, configured: true, requests: 1, failures: 0, input_tokens: 1000000, output_tokens: 0, cached_tokens: 500000, avg_prompt_tokens: 0,
      read_tokens_per_second: null, write_tokens_per_second: null, context_p50: null, context_p95: null, tasks: [] }],
  }
  const html = await renderComponent(t, '../pages/stats/models.vue', {
    useStatistics: () => ({
      days: ref(30), error: ref(null), pending: ref(false),
      s: ref({ agents: { tokens: { total: 0 }, avoided_tokens: 2500000, avoided_unaware_tokens: 2500000,
        saved: { tokens: 0, price: 'cache_write', per_mtok: 5, dollars: 0, resend_tokens: 0, upper_dollars: 0, unaware_tokens: 2500000, unaware_graph_tokens: 2500000, unaware_per_mtok: 4, unaware_dollars: 10, total_dollars: 10 } } }),
    }),
    useApiGet: (url) => ({ data: ref(url().startsWith('stats/savings') ? null : models), error: ref(null), pending: ref(false) }),
    localSaved, totalSaved, windowLabel,
    seriesRangeFor: () => '30d',
    seriesRangeLabel: () => 'Last 30 days',
    fmtCompact: (n) => String(n),
    fmtInt: (n) => String(n),
    apiErrorMessage: (e) => String(e),
  }, {})
  assert.match(html, /Saved by graph answers \$10\.00 2500000 tokens never read <i>2500000 tokens never read by agents that cannot tell repeated answers apart \(\$10\.00\), at the input price; left out of the share/)
  assert.match(html, /Saved on cached input \$0\.10 500000 tokens from the local prompt cache <i>500000 tokens the local models&#39; prompt cache served/)
  assert.match(html, /Total saved \(est\.\) \$12\.10 /, '500k × $4 + 500k × $0.20 + $10 for graph answers')
  assert.match(html, /built-in estimate(<!--\[-->)? from <a [^>]*>platform\.claude\.com\/docs\/en\/about-claude\/pricing<\/a>(<!--\[-->)?, checked 2026-10-29/)
})

test('savings chart totals its columns, values money with the tiles\' formula and skips future buckets', async (t) => {
  const { onMounted } = await import('vue')
  const day = 86400
  const start = Math.floor(Date.now() / 1000 / day) * day - day
  const bucket = (s, never, input, cached, output, graph, prompts, gateway) => ({ start: s, end: s + day, never_read_tokens: never, graph_saved_dollars: graph,
    local_input_tokens: input, local_cached_tokens: cached, local_output_tokens: output, compression_saved_dollars: prompts, gateway_saved_dollars: gateway })
  const series = {
    days: 2, step: 'day', metrics: true, unplaced_never_read_tokens: 0, unplaced_graph_saved_dollars: 0,
    buckets: [bucket(start, 1000000, 1000000, 500000, 100000, 4, 0.5, null), bucket(start + day, 0, 0, 0, 0, 0, 0, 0), bucket(start + 3 * day, 7, 7, 0, 7, 7, 7, 7)],
  }
  const price = { provider: 'p', model: 'm', input_per_mtok: 4, output_per_mtok: 20, cached_input_per_mtok: 0.2 }
  const html = await renderComponent(t, './SavingsChart.vue', {
    onMounted, onBeforeUnmount, localSaved, totalSaved,
    fmtCompact: (n) => String(n), fmtInt: (n) => String(n),
  }, { series, price })
  assert.match(html, /Never read \(graph answers\) <strong>1000000<\/strong>/, 'the future bucket is left out of the totals')
  assert.match(html, /Local models <strong>1100000<\/strong>/)
  assert.match(html, /Daily \(UTC\)/)
  assert.match(html, /<i class="info">One column per day \(UTC\)\. .*same formula as the tiles/, 'the long explanation sits behind the info icon')
  // 500k × $4 + 500k × $0.20 + 100k × $20 per million, plus $4 for graph answers and $0.50 for shorter prompts.
  assert.match(html, /<td class="num">\$8\.60<\/td>(<!--\]-->)?<\/tr>/)
  const utcDay = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' }).format(new Date(start * 1000))
  assert.ok(html.includes(`<td class="mono">${utcDay}</td>`), 'day columns are named by their UTC date')
  assert.equal((html.match(/<tr><td class="mono">/g) ?? []).length, 2, 'future buckets get no table row')
})

test('a stat tile keeps its explanation behind an info button that describes itself', async (t) => {
  const globals = { computed, ref, watch, useId, nextTick, onBeforeUnmount, GLOSSARY: { hotspot: 'Changed often and complex' } }
  for (const [key, value] of Object.entries(globals)) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, key)
    Object.defineProperty(globalThis, key, { value, configurable: true })
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, key, previous)
      else delete globalThis[key]
    })
  }
  const InfoTip = await compileComponent(t, './InfoTip.vue')
  const StatTile = await compileComponent(t, './StatTile.vue')
  const render = (component, props) => {
    const app = createSSRApp(component, props)
    app.component('InfoTip', InfoTip)
    return renderToString(app)
  }

  const html = await render(StatTile, {
    label: 'Spent on cloud models', value: '$0.25', hint: '616K read', tip: 'reported usage at the configured prices',
  })
  assert.match(html, /<p class="stat__hint"[^>]*>616K read<\/p>/, 'the short data line stays visible')
  const button = html.match(/<button[^>]*>/)?.[0] ?? ''
  assert.match(button, /type="button"/)
  assert.match(button, /aria-label="About Spent on cloud models"/)
  const describedBy = button.match(/aria-describedby="([^"]+)"/)?.[1]
  assert.ok(describedBy, 'the button names its description')
  const bubble = html.match(new RegExp(`<span[^>]*id="${describedBy}"[^>]*>([^<]*)</span>`))
  assert.ok(bubble, 'the description is in the page, not only on hover')
  assert.match(bubble[0], /role="tooltip"/)
  assert.match(bubble[0], /display:none/, 'closed until hover, focus or a tap')
  assert.equal(bubble[1], 'reported usage at the configured prices')

  const plain = await render(StatTile, { label: 'Sessions', value: '3', hint: '2 client(s)' })
  assert.doesNotMatch(plain, /<button/, 'no icon without a tip')

  const term = await render(InfoTip, { term: 'hotspot' })
  assert.match(term, /aria-label="More about this"/)
  assert.match(term, />Changed often and complex<\/span>/, 'glossary terms still resolve')
})
