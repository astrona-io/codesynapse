// Sends visitors to sign-in (or first-run setup) when the installation has
// accounts. The API enforces every rule itself; this only routes people.
const OPEN = new Set(['/login', '/signup'])

export default defineNuxtRouteMiddleware(async (to) => {
  const s = useSession()
  await s.ensure()
  if (!s.accounts.value) {
    if (OPEN.has(to.path)) return navigateTo('/')
    return
  }
  if (s.status.value?.needs_setup && to.path !== '/signup') return navigateTo('/signup')
  const isHelp = to.path === '/help' || to.path.startsWith('/help/')
  if (!s.me.value && !OPEN.has(to.path) && !isHelp) return navigateTo({ path: '/login', query: to.fullPath !== '/' ? { next: to.fullPath } : {} })
  if (s.me.value && OPEN.has(to.path)) return navigateTo('/')
})
