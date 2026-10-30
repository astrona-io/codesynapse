// Route patterns for the web server's metrics (docs/plans/TELEMETRY_PLAN.md,
// phase 6). A metric label never carries a raw path: repository names,
// organisation slugs, IDs and anything a visitor typed become a placeholder
// or "other". Every value returned here comes from a fixed list or from a
// page's route definition, never from the request.

// First path elements of the Go API under /api/v1 that the proxy forwards.
// One the list does not know is counted as other, which is safe when the
// API gains a route before this list does.
const API_TOP = new Set([
  'accounts', 'admin', 'agent-rules', 'agents', 'ai', 'attention', 'auth', 'execution', 'imports', 'ingest',
  'jobs', 'me', 'memory', 'notifications', 'orgs', 'overview', 'repos', 'reviews', 'rules', 'search',
  'settings', 'shared-code', 'sources', 'stats', 'usage', 'version', 'watches',
])

// Sub-resources of /api/v1/repos/{repo}/.
const REPO_SUB = new Set([
  'ai', 'annotations', 'architecture', 'ask-local', 'branches', 'brief-plan', 'cleanup', 'code-records',
  'conventions', 'dashboard', 'dependencies', 'duplicates', 'entry-points', 'graph', 'history', 'hotspots',
  'identities', 'images', 'impact', 'ingestion', 'licenses', 'list', 'local-draft', 'memory', 'metrics',
  'outline', 'overview', 'packages', 'policy', 'remote', 'reviews', 'runtime', 'sbom', 'schedule', 'search',
  'sections', 'security', 'semantic', 'session-memory', 'source-extract', 'symbol', 'sync', 'triage', 'usage',
])

const METHODS = new Set(['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE'])

/** methodLabel keeps the method label to a fixed set. */
export function methodLabel(method: string | undefined): string {
  const m = (method ?? '').toUpperCase()
  return METHODS.has(m) ? m : 'OTHER'
}

/** statusClass turns a status code into 1xx…5xx, or other. */
export function statusClass(status: number | undefined): string {
  if (!Number.isInteger(status) || status! < 100 || status! > 599) return 'other'
  return `${Math.floor(status! / 100)}xx`
}

/** proxyPattern maps the path after /api/cg/ to a pattern of the API route it reaches. */
export function proxyPattern(path: string): string {
  const parts = path.replace(/^\/+/, '').split('?')[0]!.split('/').filter(Boolean)
  const top = parts[0]
  if (!top || !API_TOP.has(top)) return '/api/cg/other'
  if (top === 'repos') {
    if (parts.length === 1) return '/api/cg/repos'
    if (parts.length === 2) return '/api/cg/repos/{repo}'
    const sub = REPO_SUB.has(parts[2]!) ? parts[2] : 'other'
    return `/api/cg/repos/{repo}/${sub}${parts.length > 3 ? '/*' : ''}`
  }
  return `/api/cg/${top}${parts.length > 1 ? '/*' : ''}`
}

// A page route definition such as /repos/:repo()/architecture/:id(). Only
// these characters may appear in one; anything else is not a definition.
const PAGE_DEF = /^\/[A-Za-z0-9_\-/:().*]{0,120}$/

/** pagePattern turns a vue-router definition into a label: /repos/:repo()/security → /repos/{repo}/security. */
export function pagePattern(def: string | undefined): string | undefined {
  if (!def || !PAGE_DEF.test(def)) return undefined
  return def.replace(/:([A-Za-z0-9_]+)(\([^)]*\))?[*+?]?/g, '{$1}')
}

/**
 * routePattern names the route a request took. page is the matched page
 * definition when the server rendered a page; otherwise the raw path decides
 * between the proxy, the health check, static files and other.
 */
export function routePattern(path: string, page?: string): string {
  const p = pagePattern(page)
  if (p) return p
  const pathname = path.split('?')[0]!
  if (pathname.startsWith('/api/cg/')) return proxyPattern(pathname.slice('/api/cg/'.length))
  if (pathname === '/api/health') return '/api/health'
  if (pathname.startsWith('/api/')) return '/api/other'
  if (pathname.startsWith('/_nuxt/') || /\.[A-Za-z0-9]{1,8}$/.test(pathname)) return 'static'
  return 'other'
}
