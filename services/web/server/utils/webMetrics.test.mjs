// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { test } from 'node:test'
import { MeterProvider, MetricReader } from '@opentelemetry/sdk-metrics'
import { SeriesCap, checkURL, createInstruments, proxyFailure, startWebMetrics } from './webMetrics.ts'

class TestReader extends MetricReader {
  async onForceFlush() {}
  async onShutdown() {}
}

async function collect(reader) {
  const { resourceMetrics } = await reader.collect()
  const out = {}
  for (const scope of resourceMetrics.scopeMetrics) {
    for (const m of scope.metrics) out[m.descriptor.name] = m.dataPoints
  }
  return out
}

function setup(limits) {
  const reader = new TestReader()
  const provider = new MeterProvider({ readers: [reader] })
  return { reader, inst: createInstruments(provider.getMeter('test'), limits) }
}

test('SeriesCap admits up to its limit and reports each refused new combination', () => {
  let dropped = 0
  const cap = new SeriesCap(2, () => dropped++)
  assert.ok(cap.admit({ route: '/a', method: 'GET' }))
  assert.ok(cap.admit({ method: 'GET', route: '/a' }), 'attribute order does not matter')
  assert.ok(cap.admit({ route: '/b', method: 'GET' }))
  assert.equal(cap.admit({ route: '/c', method: 'GET' }), false)
  assert.equal(cap.admit({ route: '/d', method: 'GET' }), false)
  assert.ok(cap.admit({ route: '/a', method: 'GET' }), 'admitted combinations stay admitted')
  assert.equal(dropped, 2)
  assert.equal(cap.size, 2)
})

test('instruments cap their series and count the drops per metric', async () => {
  const { reader, inst } = setup({ series: 2, histogramSeries: 1 })
  inst.request('/a', 'GET', '2xx', 0.01)
  inst.request('/a', 'GET', '2xx', 0.02)
  inst.request('/b', 'GET', '5xx', 0.5)
  inst.request('/c', 'GET', '2xx', 0.1)
  const got = await collect(reader)
  const requests = got.platform_web_requests_total
  assert.equal(requests.length, 2)
  assert.equal(requests.find((p) => p.attributes.route === '/a').value, 2)
  assert.deepEqual(Object.keys(requests[0].attributes).sort(), ['method', 'route', 'status_class'])
  const hist = got.platform_web_request_duration_seconds
  assert.equal(hist.length, 1)
  assert.equal(hist[0].value.count, 2)
  assert.deepEqual(hist[0].value.buckets.boundaries.slice(0, 3), [0.005, 0.01, 0.025], 'the API latency buckets')
  const dropped = Object.fromEntries(got.telemetry_dropped_series_total.map((p) => [p.attributes.metric, p.value]))
  assert.deepEqual(dropped, { platform_web_requests_total: 1, platform_web_request_duration_seconds: 2 })
})

test('proxy outcomes stay in their set and start at zero', async () => {
  const { reader, inst } = setup()
  inst.proxy('timeout')
  inst.proxy('unreachable')
  inst.proxy('unreachable')
  inst.proxy('acme-corp') // not an outcome: ignored
  inst.exportFailed()
  const got = await collect(reader)
  const proxy = Object.fromEntries(got.platform_web_proxy_requests_total.map((p) => [p.attributes.outcome, p.value]))
  assert.deepEqual(proxy, { ok: 0, refused: 0, unreachable: 2, timeout: 1, error: 0 })
  assert.deepEqual(got.platform_telemetry_export_failures_total.map((p) => [p.attributes.exporter, p.value]), [['otlp', 1]])
})

test('proxyFailure classifies by error kind and code, never by text', () => {
  const timeout = new DOMException('The operation was aborted due to timeout', 'TimeoutError')
  assert.equal(proxyFailure(timeout), 'timeout')
  const refused = new TypeError('fetch failed', { cause: Object.assign(new Error('connect ECONNREFUSED 10.0.0.1:8080'), { code: 'ECONNREFUSED' }) })
  assert.equal(proxyFailure(refused), 'unreachable')
  const dns = new TypeError('fetch failed', { cause: Object.assign(new Error('getaddrinfo ENOTFOUND api'), { code: 'ENOTFOUND' }) })
  assert.equal(proxyFailure(dns), 'unreachable')
  const connectTimeout = new TypeError('fetch failed', { cause: Object.assign(new Error('x'), { code: 'UND_ERR_CONNECT_TIMEOUT' }) })
  assert.equal(proxyFailure(connectTimeout), 'timeout')
  assert.equal(proxyFailure(new TypeError('ECONNREFUSED in the message only')), 'error')
  assert.equal(proxyFailure(undefined), 'error')
})

test('checkURL refuses credentials, queries and other schemes', () => {
  assert.equal(checkURL('http://victoriametrics:8428').host, 'victoriametrics:8428')
  for (const bad of ['ftp://vm:8428', 'http://user:pw@vm:8428', 'http://vm:8428/?x=1', 'http://vm:8428/#f', 'http://vm:8428?', 'not a url']) {
    assert.throws(() => checkURL(bad), /metrics url/, bad)
  }
})

test('startWebMetrics is off without a URL and refuses other exporters', () => {
  assert.equal(startWebMetrics({}), undefined)
  assert.throws(() => startWebMetrics({ CODESYNAPSE_METRICS_URL: 'http://127.0.0.1:1', CODESYNAPSE_METRICS_EXPORTER: 'prometheus' }), /otlp only/)
})

test('startWebMetrics exports OTLP with basic auth and the service resource, and counts failed exports', async () => {
  const bodies = []
  let fail = true
  const server = createServer((req, res) => {
    let data = ''
    req.on('data', (c) => (data += c))
    req.on('end', () => {
      bodies.push({ url: req.url, auth: req.headers.authorization, body: data })
      res.statusCode = fail ? 500 : 200
      res.end(fail ? '' : '{}')
    })
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const { port } = server.address()
  try {
    const m = startWebMetrics({
      CODESYNAPSE_METRICS_URL: `http://127.0.0.1:${port}`, CODESYNAPSE_METRICS_USER: 'codesynapse', CODESYNAPSE_METRICS_PASSWORD: 'pw',
      CODESYNAPSE_ENVIRONMENT: 'test', CODESYNAPSE_VERSION: '1.2.3',
    })
    m.instruments.request('/repos/{repo}', 'GET', '2xx', 0.02)
    m.instruments.proxy('unreachable')
    // The first export fails (500 is not retried); the
    // failure is counted and goes out with the next export.
    fail = true
    await m.flush().catch(() => {})
    assert.equal(bodies.length, 1, "one export was tried")
    fail = false
    await m.shutdown()
    const last = bodies.at(-1)
    assert.equal(last.url, '/opentelemetry/v1/metrics')
    assert.equal(last.auth, `Basic ${Buffer.from('codesynapse:pw').toString('base64')}`)
    const payload = JSON.parse(last.body)
    const rm = payload.resourceMetrics[0]
    const attrs = Object.fromEntries(rm.resource.attributes.map((a) => [a.key, a.value.stringValue]))
    assert.equal(attrs['service.name'], 'codesynapse-web')
    assert.equal(attrs.job, 'codesynapse-web')
    assert.equal(attrs['service.version'], '1.2.3')
    assert.equal(attrs['deployment.environment.name'], 'test')
    assert.equal(attrs.instance, attrs['service.instance.id'])
    const names = rm.scopeMetrics.flatMap((s) => s.metrics.map((x) => x.name))
    for (const n of ['platform_web_requests_total', 'platform_web_request_duration_seconds', 'platform_web_proxy_requests_total']) assert.ok(names.includes(n), n)
    const failures = rm.scopeMetrics.flatMap((s) => s.metrics).find((x) => x.name === 'platform_telemetry_export_failures_total')
    assert.ok(Number(failures.sum.dataPoints[0].asDouble ?? failures.sum.dataPoints[0].asInt) >= 1, 'the failed export was counted')
  } finally {
    server.close()
  }
})
