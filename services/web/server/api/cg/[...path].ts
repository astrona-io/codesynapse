// Proxies /api/cg/<path> to the Go API at /api/v1/<path>, adding the bearer
// token. Mutating requests must come from this origin (CSRF guard), and only
// API paths are reachable.
const ALLOWED_METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
const SAFE_PATH = /^[A-Za-z0-9._~\-/]+$/

export default defineEventHandler(async (event) => {
  const method = event.method.toUpperCase()
  if (!ALLOWED_METHODS.has(method)) {
    throw createError({ statusCode: 405, statusMessage: 'Method Not Allowed' })
  }
  const path = getRouterParam(event, 'path') ?? ''
  if (!SAFE_PATH.test(path) || path.split('/').some((seg) => seg === '..' || seg === '.')) {
    throw createError({ statusCode: 400, statusMessage: 'Bad path' })
  }
  if (method !== 'GET') {
    const origin = getRequestHeader(event, 'origin')
    const host = getRequestHeader(event, 'host')
    if (!origin || !host || new URL(origin).host !== host) {
      throw createError({ statusCode: 403, statusMessage: 'Cross-origin request rejected' })
    }
  }

  const { apiBase, apiToken } = useRuntimeConfig(event)
  const search = getRequestURL(event).search
  const target = `${apiBase.replace(/\/$/, '')}/api/v1/${path}${search}`
  const headers: Record<string, string> = { accept: 'application/json' }
  if (apiToken) headers.authorization = `Bearer ${apiToken}`
  const contentType = getRequestHeader(event, 'content-type')
  if (contentType) headers['content-type'] = contentType

  const body = ['POST', 'PUT', 'PATCH'].includes(method) ? await readRawBody(event, false) : undefined
  const res = await fetch(target, { method, headers, body, redirect: 'error', signal: AbortSignal.timeout(120_000) })
  setResponseStatus(event, res.status)
  const ct = res.headers.get('content-type')
  if (ct) setResponseHeader(event, 'content-type', ct)
  setResponseHeader(event, 'cache-control', 'no-store')
  return res.status === 204 ? null : new Uint8Array(await res.arrayBuffer())
})
