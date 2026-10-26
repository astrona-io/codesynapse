import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { stripTypeScriptTypes } from 'node:module'
import { test } from 'node:test'
import { createSSRApp, computed, ref, watch } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { parse, compileScript } from 'vue/compiler-sfc'

async function renderComponent(t, file, globals, props = {}) {
  for (const [key, value] of Object.entries({ computed, ref, watch, ...globals })) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, key)
    Object.defineProperty(globalThis, key, { value, configurable: true })
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, key, previous)
      else delete globalThis[key]
    })
  }
  const source = await readFile(new URL(file, import.meta.url), 'utf8')
  const { descriptor } = parse(source)
  let script = compileScript(descriptor, { id: file, inlineTemplate: true }).content
  script = stripTypeScriptTypes(script).replace(/from ["']vue["']/g, `from ${JSON.stringify(import.meta.resolve('vue'))}`)
  script = script.replace(/import \{ useStatistics \} from ['"]~\/composables\/useStatistics['"]/g, 'const useStatistics = globalThis.useStatistics')
  // A data: URL is imported once per text: keep each render's module, and
  // the stubs it captured, its own.
  script += `\n// ${t.fullName}`
  const { default: component } = await import(`data:text/javascript;base64,${Buffer.from(script).toString('base64')}`)
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
  app.component('StatTile', { props: ['label', 'value', 'hint'], template: '<div>{{ label }} {{ value }} {{ hint }}</div>' })
  app.component('ColumnChart', { template: '<div />' })
  app.component('RankList', { template: '<div />' })
  app.component('NuxtLink', { template: '<a><slot /></a>' })
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
  })
  assert.match(html, /Model usage unavailable/)
  assert.match(html, /Small edits and output/)
  assert.doesNotMatch(html, /Local models/, 'no AI providers in an older API, no local-model section')
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
  })
  assert.match(html, /AI work that stayed on your network/)
  assert.match(html, /qwen3\.8:27b/)
  assert.match(html, /Local requests 40 /, 'requests from the metrics store, not the API process')
  assert.match(html, /Tokens processed locally 10000 /, 'input + output tokens')
  assert.match(html, /Share of AI requests 80% /, '40 of 50 requests stayed local')
})
