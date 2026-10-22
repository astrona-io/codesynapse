<script setup lang="ts">
import type { RepoInfo } from '~/utils/types'
import type { ThemeChoice } from '~/composables/useTheme'

const route = useRoute()
const session = useSession()
const { choice: theme } = useTheme()
const repo = computed(() => (route.params.repo as string | undefined) ?? '')
const menuOpen = ref(false)
watch(() => route.fullPath, () => (menuOpen.value = false))

const { data: repos, refresh: refreshRepos } = useApiGet<{ repos: RepoInfo[] }>('repos')
// Newest index first, so a repository that was just (re)indexed moves to the top.
const latest = computed(() => [...(repos.value?.repos ?? [])].sort((a, b) => (b.ingested_at ?? '').localeCompare(a.ingested_at ?? '')).slice(0, 6))
// Syncs finish in the background: poll while the tab is visible, and catch
// up as soon as it is shown again.
let repoTimer: ReturnType<typeof setInterval> | undefined
const refreshIfVisible = () => { if (document.visibilityState === 'visible') refreshRepos() }
onMounted(() => {
  repoTimer = setInterval(refreshIfVisible, 15_000)
  document.addEventListener('visibilitychange', refreshIfVisible)
})
onBeforeUnmount(() => {
  clearInterval(repoTimer)
  document.removeEventListener('visibilitychange', refreshIfVisible)
})
const { data: health, error: healthError } = useFetch<{ status: string }>('/api/cg/version', { key: 'version' })

const main = [
  { to: '/overview', label: 'Overview', hint: 'What needs attention' },
  { to: '/', label: 'Repositories', hint: 'Everything you indexed' },
  { to: '/shared-code', label: 'Shared code', hint: 'Code several repositories copy' },
  { to: '/agents', label: 'Agents', hint: 'Connect Claude Code, Codex, Cursor' },
  { to: '/stats', label: 'Statistics', hint: 'Usage, tokens, cost, latency' },
]
const more = computed(() => [
  { to: '/orgs', label: 'Import', hint: 'Index every repository of a GitHub, GitLab or Forgejo organisation' },
  { to: '/ai', label: 'AI models', hint: 'Local and cloud language models' },
  ...(session.accounts.value ? [{ to: '/settings', label: 'Settings', hint: 'Profile, API keys, organisation, Git tokens' }] : []),
  ...(session.accounts.value && session.isPlatformAdmin.value ? [{ to: '/admin', label: 'Administration', hint: 'Organisations, people, health and the audit log' }] : []),
  { to: '/help', label: 'Help', hint: 'Getting started and what things mean' },
])
const repoGroups = computed(() => {
  if (!repo.value) return []
  const base = `/repos/${encodeURIComponent(repo.value)}`
  return [
    { label: 'Repository', links: [
      { to: base, label: 'Summary' },
      { to: `${base}/security`, label: 'Security' },
      { to: `${base}/dependencies`, label: 'Dependencies' },
      { to: `${base}/licenses`, label: 'Licences' },
      { to: `${base}/dependency-review`, label: 'Dependency review' },
      { to: `${base}/metrics`, label: 'Code health', also: [`${base}/cleanup`] },
      ...(session.accounts.value ? [{ to: `${base}/access`, label: 'Access' }] : []),
    ] },
    { label: 'Work', links: [
      { to: `${base}/reviews`, label: 'Reviews' },
      { to: `${base}/memory`, label: 'Agent memory' },
      { to: `${base}/review`, label: 'Impact of a change' },
      { to: `${base}/ai`, label: 'AI assistant' },
    ] },
    { label: 'Explore', links: [
      { to: `${base}/graph`, label: 'Graph' },
      { to: `${base}/explore`, label: 'Symbols' },
      { to: `${base}/config`, label: 'Kubernetes & Helm' },
      { to: `${base}/docs`, label: 'Docs' },
      { to: `${base}/packages`, label: 'Packages' },
    ] },
  ]
})
const isActive = (to: string) => (to === '/' ? route.path === '/' : route.path === to || route.path.startsWith(`${to}/`))
const themes: { id: ThemeChoice; label: string }[] = [{ id: 'system', label: 'Auto' }, { id: 'light', label: 'Light' }, { id: 'dark', label: 'Dark' }]
const orgValue = computed({
  get: () => session.current.value?.org.slug ?? '',
  set: (v: string) => session.switchOrg(v || null),
})
</script>

<template>
  <div class="shell">
    <a href="#main" class="skip">Skip to content</a>
    <aside class="sidebar" :class="{ open: menuOpen }">
      <div class="sidebar__top">
        <NuxtLink to="/overview" class="brand">
          <img src="/favicon.svg" alt="" width="24" height="24">
          <span class="brand__name">CodeSynapse</span>
        </NuxtLink>
        <button class="secondary tiny menu-toggle" :aria-expanded="menuOpen" @click="menuOpen = !menuOpen">Menu</button>
      </div>

      <div v-if="session.accounts.value && session.memberships.value.length > 1" class="org-switch">
        <label for="org-select" class="label">Organisation</label>
        <select id="org-select" v-model="orgValue">
          <option v-for="m in session.memberships.value" :key="m.org.id" :value="m.org.slug">{{ m.org.name }}</option>
        </select>
      </div>
      <p v-else-if="session.current.value" class="org-single"><span class="label">Organisation</span>{{ session.current.value.org.name }}</p>

      <nav class="sidebar__nav" aria-label="Main">
        <div class="nav-section">
          <NuxtLink v-for="l in main" :key="l.to" :to="l.to" class="nav-link" :class="{ 'is-active': isActive(l.to) && !repo }" :title="l.hint">{{ l.label }}</NuxtLink>
        </div>
        <template v-if="repo">
          <div class="nav-section">
            <p class="nav-label">Repository</p>
            <p class="nav-repo mono" :title="repo">{{ repo }}</p>
          </div>
          <div v-for="g in repoGroups" :key="g.label" class="nav-section">
            <p class="nav-label">{{ g.label }}</p>
            <NuxtLink v-for="l in g.links" :key="l.to" :to="l.to" class="nav-link nav-link--sub" :class="{ 'is-active': route.path === l.to || ('also' in l && l.also?.includes(route.path)) }">{{ l.label }}</NuxtLink>
          </div>
        </template>
        <div v-else-if="repos?.repos.length" class="nav-section">
          <p class="nav-label">Recently indexed</p>
          <NuxtLink v-for="r in latest" :key="r.name" :to="`/repos/${encodeURIComponent(r.name)}`" class="nav-link nav-link--sub nav-repo-link" :title="r.ingested_at ? `Indexed ${r.ingested_at}` : ''">
            <span class="mono nav-repo-name">{{ r.name }}</span>
            <span v-if="r.ingested_at" class="nav-repo-time">{{ relTime(r.ingested_at) }}</span>
          </NuxtLink>
        </div>
        <div class="nav-section">
          <NuxtLink v-for="l in more" :key="l.to" :to="l.to" class="nav-link" :class="{ 'is-active': isActive(l.to) }" :title="l.hint">{{ l.label }}</NuxtLink>
        </div>
      </nav>

      <div class="sidebar__foot">
        <div class="theme" role="radiogroup" aria-label="Theme">
          <button v-for="t in themes" :key="t.id" class="tiny" :class="theme === t.id ? '' : 'secondary'" role="radio" :aria-checked="theme === t.id" @click="theme = t.id">{{ t.label }}</button>
        </div>
        <div v-if="session.me.value" class="user">
          <div class="user__who">
            <strong class="truncate">{{ session.me.value.user.name }}</strong>
            <span class="muted small truncate">{{ session.me.value.user.email }}</span>
          </div>
          <button class="secondary tiny" @click="session.logout()">Sign out</button>
        </div>
        <p class="status small">
          <span class="dot" :class="healthError ? 'dot--bad' : health ? 'dot--ok' : 'dot--warn'" />
          <span class="muted">{{ healthError ? 'API unreachable' : health ? 'Connected' : 'Connecting…' }}</span>
        </p>
      </div>
    </aside>
    <main id="main" class="shell__main"><slot /></main>
  </div>
</template>

<style scoped>
.shell { display: grid; grid-template-columns: 248px minmax(0, 1fr); min-height: 100vh; }
.shell__main { display: flex; flex-direction: column; gap: 20px; padding: 28px var(--gutter) 48px; max-width: var(--shell-max); width: 100%; min-width: 0; }
.skip { position: absolute; left: -9999px; }
.skip:focus { left: 12px; top: 12px; z-index: 10; background: var(--panel); padding: 8px 12px; border-radius: var(--radius-sm); }
.sidebar {
  position: sticky; top: 0; height: 100vh; overflow-y: auto;
  display: flex; flex-direction: column; gap: 14px; padding: 18px 12px;
  background: var(--panel); border-right: 1px solid var(--line);
}
.sidebar__top { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 0 8px; }
.brand { display: flex; align-items: center; gap: 10px; color: var(--ink); }
.brand:hover { color: var(--ink); text-decoration: none; }
.brand__name { font-size: 16px; font-weight: 650; letter-spacing: -0.01em; }
.org-switch, .org-single { display: flex; flex-direction: column; gap: 4px; padding: 0 8px; }
.org-single { font-weight: 600; font-size: 13.5px; }
.sidebar__nav { display: flex; flex-direction: column; gap: 12px; }
.nav-section { display: flex; flex-direction: column; gap: 1px; }
.nav-label { font: 600 11.5px var(--font-sans); color: var(--dim); margin: 6px 10px 4px; }
.nav-repo { margin: 0 10px 2px; color: var(--ink); font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nav-link { display: flex; align-items: center; min-height: 34px; padding: 6px 10px; border-radius: var(--radius-sm); color: var(--muted); font-size: 14px; font-weight: 500; }
.nav-link--sub { min-height: 30px; font-size: 13.5px; }
.nav-link:hover { color: var(--ink); background: var(--hover); text-decoration: none; }
.nav-link.is-active { color: var(--ink); background: var(--accent-soft); }
.nav-repo-link { justify-content: space-between; gap: 8px; }
.nav-repo-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nav-repo-time { flex: none; font-size: 11.5px; color: var(--dim); }
.sidebar__foot { margin-top: auto; display: flex; flex-direction: column; gap: 10px; padding: 12px 8px 0; border-top: 1px solid var(--line); }
.theme { display: flex; gap: 4px; }
.theme button { flex: 1; }
.user { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.user__who { display: flex; flex-direction: column; min-width: 0; line-height: 1.35; font-size: 13.5px; }
.status { display: flex; align-items: center; gap: 8px; }
.menu-toggle { display: none; }
@media (max-width: 960px) {
  .shell { grid-template-columns: 1fr; }
  .sidebar { position: relative; height: auto; border-right: 0; border-bottom: 1px solid var(--line); }
  .menu-toggle { display: inline-flex; }
  .sidebar:not(.open) > :not(.sidebar__top) { display: none; }
}
</style>
