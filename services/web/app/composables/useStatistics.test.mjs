import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createSSRApp, defineComponent, h, ref } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { useStatistics } from './useStatistics.ts'

test('statistics tabs reuse their parent request, with separate state per render', async (t) => {
  let calls = 0
  const route = { query: {}, path: '/stats/agents' }
  const replacements = []
  const globals = {
    useRoute: () => route,
    useRouter: () => ({ replace: (to) => replacements.push(to) }),
    useState: (_key, init) => ref(init()),
    useApiGet: () => {
      calls++
      return { data: ref({ request: calls }), error: ref(null), pending: ref(false) }
    },
  }
  for (const [name, value] of Object.entries(globals)) {
    const previous = Object.getOwnPropertyDescriptor(globalThis, name)
    Object.defineProperty(globalThis, name, { value, configurable: true })
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, name, previous)
      else delete globalThis[name]
    })
  }

  async function render() {
    let parent
    const children = []
    const Tab = defineComponent({
      setup() {
        children.push(useStatistics())
        return () => h('div')
      },
    })
    const Parent = defineComponent({
      setup() {
        parent = useStatistics()
        return () => h('div', [h(Tab), h(Tab)])
      },
    })
    await renderToString(createSSRApp(Parent))
    assert.equal(children.length, 2)
    for (const child of children) assert.equal(child, parent)
    return parent
  }

  const first = await render()
  assert.equal(calls, 1, 'parent and tabs issue just one workspace request')
  const second = await render()
  assert.equal(calls, 2)
  assert.notEqual(first, second, 'the shared fetch never crosses SSR requests')
  assert.equal(first.s.value.request, 1)
  assert.equal(second.s.value.request, 2)
})
