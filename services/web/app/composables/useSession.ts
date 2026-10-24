import type { AuthStatus, Me, Membership, Role } from '~/utils/types'

const ROLE_RANK: Record<Role, number> = { guest: 0, viewer: 1, member: 2, admin: 3, owner: 4 }

// The signed-in user and the organisation they work in. The session itself
// is an HTTP-only cookie the browser never reads; this only holds what the
// API says about it. The organisation choice is a plain cookie (cs_org)
// that the server proxy turns into the X-CodeSynapse-Org header.
export function useSession() {
  const status = useState<AuthStatus | null>('auth-status', () => null)
  const me = useState<Me | null>('auth-me', () => null)
  const loaded = useState('auth-loaded', () => false)
  const orgSlug = useCookie<string | null>('cs_org', { sameSite: 'lax', path: '/', maxAge: 60 * 60 * 24 * 365 })

  async function load() {
    const fetcher = useRequestFetch()
    status.value = await fetcher<AuthStatus>('/api/cg/auth/status').catch(() => ({ mode: 'accounts' }) as AuthStatus)
    me.value = status.value.mode === 'accounts' ? await fetcher<Me>('/api/cg/me').catch(() => null) : null
    loaded.value = true
    const ms = me.value?.memberships ?? []
    // Platform administration does not grant organisation membership.
    if (!ms.some((m) => m.org.slug === orgSlug.value)) {
      orgSlug.value = ms[0]?.org.slug ?? null
    }
  }
  async function ensure() {
    if (!loaded.value) await load()
  }
  async function logout() {
    await $fetch('/api/cg/auth/logout', { method: 'POST' }).catch(() => undefined)
    me.value = null
    orgSlug.value = null
    await navigateTo('/login')
  }

  const accounts = computed(() => status.value?.mode === 'accounts')
  const memberships = computed<Membership[]>(() => me.value?.memberships ?? [])
  const current = computed(() => memberships.value.find((m) => m.org.slug === orgSlug.value))
  const isPlatformAdmin = computed(() => !!me.value?.user.platform_admin)
  // The bootstrap root: administration only, never organisation work.
  const restricted = computed(() => !!me.value?.restricted)
  const autoCreated = computed(() => !!me.value?.user.auto_created)
  const setupPending = computed(() => me.value?.setup === 'pending')
  const can = (least: Role) => !accounts.value || (current.value ? ROLE_RANK[current.value.role] >= ROLE_RANK[least] : false)
  async function switchOrg(slug: string | null) {
    orgSlug.value = slug
    await refreshNuxtData()
  }
  return { status, me, accounts, memberships, current, orgSlug, isPlatformAdmin, restricted, setupPending, autoCreated, can, load, ensure, logout, switchOrg }
}
