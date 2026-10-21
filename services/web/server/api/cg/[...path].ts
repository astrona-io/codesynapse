// Proxies /api/cg/<path> to the Go API at /api/v1/<path>, adding the bearer
// token. Mutating requests must come from this origin (CSRF guard), and only
// API paths are reachable.
const ALLOWED_METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
// %2F is allowed: repository names are owner/name and travel as one encoded
// path element, which the API decodes after routing.
const SAFE_PATH = /^[A-Za-z0-9._~\-/%]+$/

function hasDotSegment(path: string): boolean {
  try {
    return path.split('/').some((seg) => decodeURIComponent(seg).split(/[/\\]/).some((part) => part === '..' || part === '.' || part.includes('\0')))
  } catch {
    return true // malformed percent-encoding
  }
}

export default defineEventHandler(async (event) => {
  const method = event.method.toUpperCase()
  if (!ALLOWED_METHODS.has(method)) {
    throw createError({ statusCode: 405, statusMessage: 'Method Not Allowed' })
  }
  // The raw path, not the router param: the param is already decoded, which
  // would turn owner%2Fname into two path elements.
  const path = getRequestURL(event).pathname.replace(/^\/api\/cg\//, '')
  if (!SAFE_PATH.test(path) || hasDotSegment(path)) {
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
