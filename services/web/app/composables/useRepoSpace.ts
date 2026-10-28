// Where an added repository goes: the organisation's (owners and admins;
// the organisation default applies) or the adder's own space (members,
// while the organisation allows it; only grants open it to others). The
// API enforces both rules; this only offers the choices that will pass.
export type RepoSpace = 'org' | 'me'

export function useRepoSpace() {
  const session = useSession()
  const slug = computed(() => session.current.value?.org.slug)
  const { data } = useApiGet<{ enabled: boolean }>(() => `accounts/orgs/${slug.value ?? '-'}/member-repos`, { immediate: session.accounts.value })
  const canOrg = computed(() => session.can('admin'))
  const canMe = computed(() => session.accounts.value && (data.value?.enabled ?? true))
  const space = ref<RepoSpace>(canOrg.value ? 'org' : 'me')
  watch(canOrg, (on) => { if (!on) space.value = 'me' })
  // Single-tenant installs have no spaces: send nothing.
  const body = () => (session.accounts.value ? { space: space.value } : {})
  return { space, canOrg, canMe, body }
}
