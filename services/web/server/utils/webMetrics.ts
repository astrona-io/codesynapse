// Metrics of the web server (docs/plans/TELEMETRY_PLAN.md, phase 6): requests the
// Nuxt server handles (rendered pages and the /api/cg proxy) and proxy calls
// that never reached the API. Nothing here has a trusted tenant, so every
// instrument is a platform_* series. Labels come only from fixed sets
// (routePattern.ts and the outcome list below); each instrument admits a
// bounded number of label combinations and counts the rest in
// telemetry_dropped_series_total, as the Go registry does.
//
// Export follows internal/app/metrics.go: OTLP/HTTP to the internal
// VictoriaMetrics every 15 s, credentials from the environment only, a
// resource naming service, version, environment and process instance.
// Nothing listens for scrapes.
import type { Attributes, Counter, Histogram, Meter } from '@opentelemetry/api'
import { AggregationTemporalityPreference, OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http'
import { resourceFromAttributes } from '@opentelemetry/resources'
import { MeterProvider, PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics'
import type { AggregationTemporality, InstrumentType, PushMetricExporter, ResourceMetrics } from '@opentelemetry/sdk-metrics'

/** The API's latency buckets (internal/telemetry/metrics.go), so panels compare like with like. */
export const LATENCY_BUCKETS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300]

/** Label combinations per counter, and per histogram (each holds 17 points per combination). */
export const MAX_SERIES = 2000
export const MAX_HISTOGRAM_SERIES = 300

/** Outcomes of a proxied call: the API answered, or why it did not. */
export const PROXY_OUTCOMES = ['ok', 'refused', 'unreachable', 'timeout', 'error'] as const
export type ProxyOutcome = (typeof PROXY_OUTCOMES)[number]

/**
 * SeriesCap admits at most max distinct label combinations and reports each
 * refused new one. A combination once admitted stays admitted.
 */
export class SeriesCap {
  readonly #seen = new Set<string>()
  readonly #max: number
  readonly #dropped: () => void

  constructor(max: number, dropped: () => void) {
    this.#max = max
    this.#dropped = dropped
  }

  admit(attrs: Record<string, string>): boolean {
    const key = Object.keys(attrs).sort().map((k) => `${k}\u0000${attrs[k]}`).join('\u0001')
    if (this.#seen.has(key)) return true
    if (this.#seen.size >= this.#max) {
      this.#dropped()
      return false
    }
    this.#seen.add(key)
    return true
  }

  get size(): number {
    return this.#seen.size
  }
}

export interface WebInstruments {
  /** request records one finished request; route must come from routePattern. */
  request(route: string, method: string, statusClass: string, seconds: number): void
  /** proxy records how a call through /api/cg ended. */
  proxy(outcome: ProxyOutcome): void
  /** exportFailed counts one failed export; it arrives with the next one. */
  exportFailed(): void
}

export interface Limits {
  series?: number
  histogramSeries?: number
}

/** createInstruments registers the web server's instruments on a meter. */
export function createInstruments(meter: Meter, limits: Limits = {}): WebInstruments {
  const dropped = meter.createCounter('telemetry_dropped_series_total', { description: 'New label combinations an instrument refused at its series cap.' })
  const capped = <T extends Counter | Histogram>(name: string, inst: T, max: number) => {
    const cap = new SeriesCap(max, () => dropped.add(1, { metric: name }))
    return (value: number, attrs: Record<string, string>) => {
      if (!cap.admit(attrs)) return
      if ('record' in inst) inst.record(value, attrs as Attributes)
      else inst.add(value, attrs as Attributes)
    }
  }
  const requests = capped('platform_web_requests_total',
    meter.createCounter('platform_web_requests_total', { description: 'Requests the web server handled, by route pattern, method and status class.' }),
    limits.series ?? MAX_SERIES)
  const duration = capped('platform_web_request_duration_seconds',
    meter.createHistogram('platform_web_request_duration_seconds', {
      description: 'Time the web server took to answer, by route pattern.', unit: 's',
      advice: { explicitBucketBoundaries: LATENCY_BUCKETS },
    }),
    limits.histogramSeries ?? MAX_HISTOGRAM_SERIES)
  const proxyCalls = meter.createCounter('platform_web_proxy_requests_total', { description: 'Calls through the API proxy by outcome: ok (the API answered), refused (by the proxy), unreachable, timeout, error.' })
  const exports = meter.createCounter('platform_telemetry_export_failures_total', { description: 'Metric exports that failed, by exporter.' })
  // Zero first values, so the first real increase is visible to increase().
  for (const outcome of PROXY_OUTCOMES) proxyCalls.add(0, { outcome })
  exports.add(0, { exporter: 'otlp' })
  return {
    request(route, method, statusClass, seconds) {
      requests(1, { route, method, status_class: statusClass })
      if (Number.isFinite(seconds) && seconds >= 0) duration(seconds, { route })
    },
    proxy(outcome) {
      if ((PROXY_OUTCOMES as readonly string[]).includes(outcome)) proxyCalls.add(1, { outcome })
    },
    exportFailed() {
      exports.add(1, { exporter: 'otlp' })
    },
  }
}

/** checkURL accepts http(s)://host[:port][/path] without credentials, query or fragment (telemetry.CheckURL). */
export function checkURL(raw: string): URL {
  let u: URL
  try {
    u = new URL(raw)
  } catch {
    throw new Error('metrics url must be http(s)://host:port without credentials, query or fragment')
  }
  if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.hostname || u.username || u.password || u.search || u.hash || raw.includes('?') || raw.includes('#')) {
    throw new Error('metrics url must be http(s)://host:port without credentials, query or fragment')
  }
  return u
}

// Counts failed exports, so a VictoriaMetrics outage shows up in
// platform_telemetry_export_failures_total like the Go services' failures.
class CountingExporter implements PushMetricExporter {
  readonly #inner: PushMetricExporter
  readonly #failed: (err?: Error) => void

  constructor(inner: PushMetricExporter, failed: (err?: Error) => void) {
    this.#inner = inner
    this.#failed = failed
  }

  export(metrics: ResourceMetrics, done: Parameters<PushMetricExporter['export']>[1]): void {
    this.#inner.export(metrics, (result) => {
      // ExportResultCode.SUCCESS is 0.
      if (result.code !== 0) this.#failed(result.error)
      done(result)
    })
  }

  selectAggregationTemporality(kind: InstrumentType): AggregationTemporality {
    return this.#inner.selectAggregationTemporality!(kind)
  }

  forceFlush(): Promise<void> {
    return this.#inner.forceFlush()
  }

  shutdown(): Promise<void> {
    return this.#inner.shutdown()
  }
}

export interface WebMetrics {
  instruments: WebInstruments
  /** flush exports now; it rejects when the export fails. */
  flush(): Promise<void>
  shutdown(): Promise<void>
}

/**
 * startWebMetrics starts the export when CODESYNAPSE_METRICS_URL is set and
 * returns undefined when it is not. Like the Go services it reads
 * CODESYNAPSE_METRICS_USER, CODESYNAPSE_METRICS_PASSWORD (never logged),
 * CODESYNAPSE_METRICS_EXPORTER (otlp only here: the web server has no
 * Prometheus push) and CODESYNAPSE_ENVIRONMENT.
 */
export function startWebMetrics(env: Record<string, string | undefined>): WebMetrics | undefined {
  const raw = env.CODESYNAPSE_METRICS_URL
  if (!raw) return undefined
  const exporterName = env.CODESYNAPSE_METRICS_EXPORTER || 'otlp'
  if (exporterName !== 'otlp') throw new Error('CODESYNAPSE_METRICS_EXPORTER: the web server exports otlp only')
  const base = checkURL(raw)
  const headers: Record<string, string> = {}
  if (env.CODESYNAPSE_METRICS_USER) {
    const pair = new TextEncoder().encode(`${env.CODESYNAPSE_METRICS_USER}:${env.CODESYNAPSE_METRICS_PASSWORD ?? ''}`)
    headers.authorization = `Basic ${btoa(String.fromCharCode(...pair))}`
  }
  const instance = crypto.randomUUID()
  const service = 'codesynapse-web'
  let instruments: WebInstruments | undefined
  let warned = 0
  const exporter = new CountingExporter(new OTLPMetricExporter({
    url: `${base.origin}${base.pathname.replace(/\/+$/, '')}/opentelemetry/v1/metrics`,
    headers,
    // Under the reader's 10 s, so a slow export ends as a failure this wrapper sees.
    timeoutMillis: 8_000,
    temporalityPreference: AggregationTemporalityPreference.CUMULATIVE,
  }), (err) => {
    instruments?.exportFailed()
    // The SDK reports export errors nowhere by default; log at most once a minute.
    if (Date.now() - warned > 60_000) {
      warned = Date.now()
      console.warn('pushing metrics failed:', err?.message ?? 'unknown error')
    }
  })
  const provider = new MeterProvider({
    resource: resourceFromAttributes({
      'service.namespace': 'codesynapse', 'service.name': service,
      'service.instance.id': instance, 'service.version': env.CODESYNAPSE_VERSION || 'unknown',
      'deployment.environment.name': env.CODESYNAPSE_ENVIRONMENT || 'unknown',
      job: service, instance,
    }),
    readers: [new PeriodicExportingMetricReader({ exporter, exportIntervalMillis: 15_000, exportTimeoutMillis: 10_000 })],
  })
  instruments = createInstruments(provider.getMeter('codesynapse-web'))
  return {
    instruments,
    flush: () => provider.forceFlush(),
    // One last export with a five-second deadline, as the Go services do.
    shutdown: async () => {
      let timer: ReturnType<typeof setTimeout> | undefined
      await Promise.race([provider.shutdown(), new Promise<void>((resolve) => { timer = setTimeout(resolve, 5_000) })])
      clearTimeout(timer)
    },
  }
}

let active: WebInstruments | undefined

/** useWebMetrics sets the instruments the request hooks and the proxy record into. */
export function useWebMetrics(instruments: WebInstruments | undefined): void {
  active = instruments
}

/** recordProxy counts one proxied call; a no-op while metrics are off. */
export function recordProxy(outcome: ProxyOutcome): void {
  active?.proxy(outcome)
}

/** recordRequest counts one finished request; a no-op while metrics are off. */
export function recordRequest(route: string, method: string, statusClass: string, seconds: number): void {
  active?.request(route, method, statusClass, seconds)
}

/** proxyFailure tells why a call to the API failed, from the error's kind and code, never its text. */
export function proxyFailure(err: unknown): ProxyOutcome {
  const e = err as { name?: string, code?: string, cause?: { name?: string, code?: string } } | undefined
  const names = [e?.name, e?.cause?.name]
  const codes = [e?.code, e?.cause?.code]
  if (names.includes('TimeoutError') || codes.some((c) => c === 'UND_ERR_CONNECT_TIMEOUT' || c === 'UND_ERR_HEADERS_TIMEOUT' || c === 'UND_ERR_BODY_TIMEOUT' || c === 'ETIMEDOUT')) return 'timeout'
  if (codes.some((c) => c !== undefined && ['ECONNREFUSED', 'ECONNRESET', 'ENOTFOUND', 'EAI_AGAIN', 'EHOSTUNREACH', 'ENETUNREACH', 'EPIPE', 'UND_ERR_SOCKET', 'UND_ERR_CLOSED'].includes(c))) return 'unreachable'
  return 'error'
}
