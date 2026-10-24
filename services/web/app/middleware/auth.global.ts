// Sends visitors to sign-in (or first-run setup) when the installation has
// accounts. The API enforces every rule itself; this only routes people.
const OPEN = new Set(['/login', '/signup'])
// Reachable without a session: the handover just ended root's.
// The single sign-on return works signed in (linking) or not (sign-in).
const PUBLIC = new Set(['/setup/done', '/sso/callback'])

// The bootstrap root does governance only (docs/ROOT_BOOTSTRAP_PLAN.md).
function rootMay(path: string): boolean {
  return path === '/admin' || path.startsWith('/admin/') || path === '/settings' || path === '/settings/profile' ||
    path === '/help' || path.startsWith('/help/')
}

export default defineNuxtRouteMiddleware(async (to) => {
  const s = useSession()
  await s.ensure()
  if (!s.accounts.value) {
    if (OPEN.has(to.path) || to.path.startsWith('/setup')) return navigateTo('/')
    return
  }
  if (PUBLIC.has(to.path)) return
  if (s.status.value?.needs_setup && to.path !== '/signup') return navigateTo('/signup')
  const isHelp = to.path === '/help' || to.path.startsWith('/help/')
  if (!s.me.value && !OPEN.has(to.path) && !isHelp) return navigateTo({ path: '/login', query: to.fullPath !== '/' ? { next: to.fullPath } : {} })
  if (s.me.value && OPEN.has(to.path)) return navigateTo(s.restricted.value ? '/admin/overview' : '/')
  if (s.restricted.value) {
    // The administrator made at first start is replaced before anything else.
    if (s.autoCreated.value && s.setupPending.value) return to.path === '/replace-admin' || isHelp ? undefined : navigateTo('/replace-admin')
    if (to.path === '/replace-admin' && s.autoCreated.value) return
    if (s.setupPending.value) return to.path === '/setup' || isHelp ? undefined : navigateTo('/setup')
    if (to.path === '/setup' || !rootMay(to.path)) return navigateTo('/admin/overview')
  } else if (to.path === '/setup') {
    return navigateTo('/')
  }
})
