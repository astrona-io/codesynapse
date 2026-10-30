// Sends visitors to sign-in (or first-run setup) when the installation has
// accounts. The API enforces every rule itself; this only routes people.
const OPEN = new Set(['/login', '/signup'])
// Reachable without a session: the handover just ended root's.
// The single sign-on return works signed in (linking) or not (sign-in).
const PUBLIC = new Set(['/setup/done', '/sso/callback'])

// The bootstrap root does governance only (docs/plans/ROOT_BOOTSTRAP_PLAN.md).
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
    // Setup steps are offered by the welcome guide, never forced.
    if (to.path === '/replace-admin') return s.autoCreated.value ? undefined : navigateTo('/admin/overview')
    if (to.path === '/setup') return s.setupPending.value ? undefined : navigateTo('/admin/overview')
    if (!rootMay(to.path)) return navigateTo('/admin/overview')
  } else if (to.path === '/setup') {
    return navigateTo('/')
  }
})
