// Proxies /api/cg/<path> to the Go API at /api/v1/<path>. Mutating requests
// must come from this origin (CSRF guard), and only API paths are reachable.
//
// Who the API sees: with accounts (PostgreSQL configured), the browser's
// own session cookie is forwarded and the API's Set-Cookie comes back; the
// installation token is never added, so a visitor is exactly the user they
// logged in as. Only a single-tenant install (no accounts) gets the server's
// token. When the mode cannot be read, no token is added (fail closed).
const ALLOWED_METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
// %2F is allowed: repository names are owner/name and travel as one encoded
// path element, which the API decodes after routing.
const SAFE_PATH = /^[A-Za-z0-9._~\-/%]+$/
const SESSION_COOKIE = 'cs_session'
// Binds a pending single sign-on to this browser; only the SSO endpoints
// see it.
const SSO_COOKIE = 'cs_sso'
const SSO_PATH = /^(auth\/sso\/|me\/sso\/)/
const TOKEN = /^[A-Za-z0-9_-]{16,128}$/
// The organisation the user works in (a UI choice, not a credential).
const ORG_COOKIE = 'cs_org'
const SLUG = /^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$/

function hasDotSegment(path: string): boolean {
  try {
    return path.split('/').some((seg) => decodeURIComponent(seg).split(/[/\\]/).some((part) => part === '..' || part === '.' || part.includes('\0')))
  } catch {
    return true // malformed percent-encoding
  }
}

let mode: { value: 'single' | 'accounts'; at: number } | undefined
async function apiMode(base: string): Promise<'single' | 'accounts'> {
  if (mode && Date.now() - mode.at < 30_000) return mode.value
  try {
    const res = await fetch(`${base}/api/v1/auth/status`, { headers: { accept: 'application/json' }, signal: AbortSignal.timeout(5_000) })
    const body = (await res.json()) as { mode?: string }
    mode = { value: body.mode === 'single' ? 'single' : 'accounts', at: Date.now() }
    return mode.value
  } catch {
    return 'accounts'
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

  const { apiBase, apiToken, maxBodyBytes } = useRuntimeConfig(event)
  const base = apiBase.replace(/\/$/, '')
  const search = getRequestURL(event).search
  const target = `${base}/api/v1/${path}${search}`
  const headers: Record<string, string> = { accept: 'application/json' }
  const ua = getRequestHeader(event, 'user-agent')
  if (ua) headers['user-agent'] = ua.slice(0, 300)
  // The API trusts this only from the web app's address (TRUSTED_PROXIES);
  // it is the connection's address, never a header the client sent.
  const ip = getRequestIP(event, { xForwardedFor: false })
  if (ip) headers['x-forwarded-for'] = ip
  if ((await apiMode(base)) === 'single') {
    if (apiToken) headers.authorization = `Bearer ${apiToken}`
  } else {
    const cookies: string[] = []
    const session = getCookie(event, SESSION_COOKIE)
    if (session && TOKEN.test(session)) cookies.push(`${SESSION_COOKIE}=${session}`)
    const binder = getCookie(event, SSO_COOKIE)
    if (binder && TOKEN.test(binder) && SSO_PATH.test(path)) cookies.push(`${SSO_COOKIE}=${binder}`)
    if (cookies.length) headers.cookie = cookies.join('; ')
    const org = getCookie(event, ORG_COOKIE)
    if (org && SLUG.test(org)) headers['x-codesynapse-org'] = org
  }
  const contentType = getRequestHeader(event, 'content-type')
  if (contentType) headers['content-type'] = contentType

  let body: Uint8Array<ArrayBuffer> | undefined
  if (['POST', 'PUT', 'PATCH'].includes(method)) {
    try {
      body = await readBounded(getRequestWebStream(event), Number(maxBodyBytes), getRequestHeader(event, 'content-length'))
    } catch (e) {
      if (e instanceof BodyTooLarge) throw createError({ statusCode: 413, statusMessage: 'Request body too large' })
      throw e
    }
  }
  const res = await fetch(target, { method, headers, body, redirect: 'error', signal: AbortSignal.timeout(120_000) })
  setResponseStatus(event, res.status)
  const ct = res.headers.get('content-type')
  if (ct) setResponseHeader(event, 'content-type', ct)
  // Only the one download the API offers (the SBOM), with a plain file name.
  const disposition = res.headers.get('content-disposition')
  if (disposition && /^attachment; filename="[A-Za-z0-9._-]{1,200}"$/.test(disposition)) setResponseHeader(event, 'content-disposition', disposition)
  // Login, sign-up and logout set or clear the session cookie; single
  // sign-on sets and clears its binder.
  for (const c of res.headers.getSetCookie()) {
    if (c.startsWith(`${SESSION_COOKIE}=`) || (c.startsWith(`${SSO_COOKIE}=`) && SSO_PATH.test(path))) appendResponseHeader(event, 'set-cookie', c)
  }
  setResponseHeader(event, 'cache-control', 'no-store')
  return res.status === 204 ? null : new Uint8Array(await res.arrayBuffer())
})
