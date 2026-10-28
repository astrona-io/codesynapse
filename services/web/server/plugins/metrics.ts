// Starts the web server's metrics (server/utils/webMetrics.ts) when
// CODESYNAPSE_METRICS_URL is set, and records every request the server
// answers by route pattern, method and status class. A bad metrics setting
// stops the server at start, as it stops the Go services.
import type { H3Event } from 'h3'

const START = Symbol('metricsStart')
const DONE = Symbol('metricsDone')

// The server's types carry no Node.js declarations; process exists at run time.
const env = (globalThis as { process?: { env: Record<string, string | undefined> } }).process?.env ?? {}

export default defineNitroPlugin((nitroApp) => {
  const metrics = startWebMetrics(env)
  if (!metrics) return
  useWebMetrics(metrics.instruments)
  console.info('metrics push enabled (exporter otlp, every 15s)')

  const record = (event: H3Event, status: number) => {
    const ctx = event.context as Record<symbol, number | boolean>
    if (ctx[DONE]) return
    ctx[DONE] = true
    const start = ctx[START]
    const seconds = typeof start === 'number' ? (performance.now() - start) / 1000 : Number.NaN
    // The page plugin (app/plugins/metrics-route.server.ts) names the page
    // a server render matched; everything else is named from the path.
    const route = routePattern(event.path, event.context.metricsPage as string | undefined)
    recordRequest(route, methodLabel(event.method), statusClass(status), seconds)
  }
  nitroApp.hooks.hook('request', (event) => {
    ;(event.context as Record<symbol, number>)[START] = performance.now()
  })
  nitroApp.hooks.hook('afterResponse', (event) => record(event, event.node.res.statusCode))
  // h3 skips afterResponse when Nitro's error handler already answered, so
  // errors (a refused proxy call, an unreachable API) are counted here.
  nitroApp.hooks.hook('error', (error, { event }) => {
    if (event) record(event, (error as { statusCode?: number }).statusCode ?? 500)
  })
  nitroApp.hooks.hook('close', async () => {
    useWebMetrics(undefined)
    await metrics.shutdown()
  })
})
