<script setup lang="ts">
import type { RepoInfo, RepoSections } from '~/utils/types'
import type { SearchItem } from '~/components/QuickSearch.vue'

const route = useRoute()
const session = useSession()
const repo = computed(() => (route.params.repo as string | undefined) ?? '')
const menuOpen = ref(false)
watch(() => route.fullPath, () => (menuOpen.value = false))

// The root account does no organisation work: no repositories to load.
const { data: repos, refresh: refreshRepos } = useApiGet<{ repos: RepoInfo[] }>('repos', { immediate: !session.restricted.value })
// Newest index first, so a repository that was just (re)indexed moves to the top.
const latest = computed(() => [...(repos.value?.repos ?? [])].sort((a, b) => (b.ingested_at ?? '').localeCompare(a.ingested_at ?? '')).slice(0, 6))
// Syncs finish in the background: poll while the tab is visible, and catch
// up as soon as it is shown again.
let repoTimer: ReturnType<typeof setInterval> | undefined
const refreshIfVisible = () => { if (document.visibilityState === 'visible' && !session.restricted.value) refreshRepos() }
onMounted(() => {
  repoTimer = setInterval(refreshIfVisible, 15_000)
  document.addEventListener('visibilitychange', refreshIfVisible)
})
onBeforeUnmount(() => {
  clearInterval(repoTimer)
  document.removeEventListener('visibilitychange', refreshIfVisible)
})
const { data: health, error: healthError } = useFetch<{ status: string }>('/api/cg/version', { key: 'version' })

const main = computed(() => session.restricted.value ? [] : [
  { to: '/overview', label: 'Overview', hint: 'What needs attention' },
  { to: '/', label: 'Repositories', hint: 'Everything you indexed' },
  { to: '/shared-code', label: 'Shared code', hint: 'Code several repositories copy' },
  { to: '/agents', label: 'Agents', hint: 'Connect Claude Code, Codex, Cursor' },
  { to: '/stats', label: 'Statistics', hint: 'Usage, tokens, cost, latency' },
])
const more = computed(() => session.restricted.value ? [
  { to: '/admin', label: 'Administration', hint: 'Organisations, people, health and the audit log' },
  { to: '/settings/profile', label: 'Profile', hint: 'Your name and password' },
  { to: '/help', label: 'Help', hint: 'Getting started and what things mean' },
] : [
  // With accounts, import adds picked repositories to the selected
  // organisation; without, it watches organisations installation-wide.
  session.accounts.value
    ? { to: '/import', label: 'Import', hint: 'Add repositories of a GitHub, GitLab or Forgejo organisation' }
    : { to: '/orgs', label: 'Import', hint: 'Index every repository of a GitHub, GitLab or Forgejo organisation' },
  { to: '/ai', label: 'AI models', hint: 'Local and cloud language models' },
  ...(session.accounts.value ? [{ to: '/settings', label: 'Settings', hint: 'Profile, API keys, organisation, Git tokens' }] : []),
  ...(session.accounts.value && session.isPlatformAdmin.value ? [{ to: '/admin', label: 'Administration', hint: 'Organisations, people, health and the audit log' }] : []),
  { to: '/help', label: 'Help', hint: 'Getting started and what things mean' },
])
// The repository menu shows the pages that have something in them; the
// rest wait under "More" with the reason, so a deep link or the next sync
// never strands anyone. Until the counts arrive (or if they fail), every
// link counts as present.
const sectionsRepo = ref(repo.value)
watch(repo, (r) => { if (r) sectionsRepo.value = r })
const { data: sections, refresh: refreshSections } = useApiGet<RepoSections>(
  () => repoPath(sectionsRepo.value || '-', 'sections'),
  { immediate: !!repo.value && !session.restricted.value },
)
const indexedAt = computed(() => repos.value?.repos.find(r => r.name === repo.value)?.ingested_at)
watch(indexedAt, (now, before) => { if (repo.value && before && now !== before) refreshSections() })

type RepoLink = { to: string; label: string; also?: string[]; missing?: string }
const repoMenu = computed(() => {
  if (!repo.value) return { groups: [] as { label: string; links: RepoLink[] }[], more: [] as RepoLink[] }
  const base = `/repos/${encodeURIComponent(repo.value)}`
  // missing is why a link has nothing to show; links without a rule stay.
  const has = sections.value && sectionsRepo.value === repo.value ? sections.value : null
  const need = (ok: (s: RepoSections) => boolean, why: string) => (has && !ok(has) ? why : undefined)
  const accounts = session.accounts.value
  const groups: { label: string; links: RepoLink[] }[] = [
    { label: 'Overview', links: [
      { to: base, label: 'Summary' },
      { to: `${base}/architecture`, label: 'Architecture', missing: need(s => s.records + s.proposals > 0, 'no architecture records or proposals') },
      { to: `${base}/history`, label: 'Trends', missing: need(s => s.snapshots > 0, 'no snapshots yet; each sync adds one') },
    ] },
    { label: 'Quality and security', links: [
      { to: `${base}/security`, label: 'Security' },
      { to: `${base}/metrics`, label: 'Code health', also: [`${base}/cleanup`] },
      { to: `${base}/entry-points`, label: 'Entry points', missing: need(s => s.routes > 0, 'no HTTP routes, MCP tools or CLI commands found') },
    ] },
    { label: 'Dependencies', links: [
      { to: `${base}/dependencies`, label: 'Dependencies', missing: need(s => s.dependencies > 0, 'no dependency manifests indexed') },
      { to: `${base}/licenses`, label: 'Licences', missing: need(s => s.licences, 'no licence report; sync to create one') },
      { to: `${base}/dependency-review`, label: 'Dependency review', missing: need(s => s.dependencies > 0, 'no dependency manifests indexed') },
    ] },
    { label: 'Work', links: [
      { to: `${base}/reviews`, label: 'Reviews', missing: need(s => s.reviews > 0, 'no reviews yet') },
      { to: `${base}/review`, label: 'Impact of a change' },
      { to: `${base}/ai`, label: 'AI assistant' },
      { to: `${base}/memory`, label: 'Agent memory', missing: need(s => s.memory_facts > 0, 'agents saved nothing yet') },
    ] },
    { label: 'Explore', links: [
      { to: `${base}/explore`, label: 'Symbols' },
      { to: `${base}/graph`, label: 'Graph' },
      { to: `${base}/docs`, label: 'Docs', missing: need(s => s.documents > 0, 'no Markdown documents indexed') },
      { to: `${base}/packages`, label: 'Packages', missing: need(s => s.packages > 0, 'no packages or modules indexed') },
      { to: `${base}/config`, label: 'Kubernetes & Helm', missing: need(s => s.config_files > 0, 'no YAML, Helm charts or Kubernetes manifests indexed') },
    ] },
    ...(accounts ? [{ label: 'Settings', links: [
      { to: `${base}/ingestion`, label: 'Ingestion reports' },
      { to: `${base}/access`, label: 'Access', missing: need(s => s.admin, 'repository admins manage access') },
      { to: `${base}/policy`, label: 'Indexing policy', missing: need(s => s.admin, 'repository admins approve the policy') },
    ] }] : []),
  ]
  const more = groups.flatMap(g => g.links.filter(l => l.missing))
  return {
    groups: groups.map(g => ({ ...g, links: g.links.filter(l => !l.missing) })).filter(g => g.links.length),
    more,
  }
})
const linkActive = (l: RepoLink) => route.path === l.to || !!l.also?.includes(route.path)
const moreOpen = ref(false)
// Open "More" when the page shown is in it, so the active link is visible.
watch(() => [route.path, repoMenu.value.more.length], () => { if (repoMenu.value.more.some(linkActive)) moreOpen.value = true }, { immediate: true })
const isActive = (to: string) => (to === '/' ? route.path === '/' : route.path === to || route.path.startsWith(`${to}/`))
// What the top bar's search finds: the pages, the repositories and, inside
// one, its pages.
const searchItems = computed<SearchItem[]>(() => {
  const items: SearchItem[] = [...main.value, ...more.value].map((l) => ({ label: l.label, to: l.to, group: 'Page', hint: l.hint }))
  for (const r of repos.value?.repos ?? []) items.push({ label: r.name, to: `/repos/${encodeURIComponent(r.name)}`, group: 'Repository' })
  for (const g of repoMenu.value.groups) for (const l of g.links) items.push({ label: l.label, to: l.to, group: repo.value })
  for (const l of repoMenu.value.more) items.push({ label: l.label, to: l.to, group: repo.value })
  return items
})
const orgValue = computed({
  get: () => session.current.value?.org.slug ?? '',
  set: (v: string) => session.switchOrg(v || null),
})
</script>

<template>
  <div class="app">
    <a href="#main" class="skip">Skip to content</a>
    <header class="bar">
      <NuxtLink to="/overview" class="brand" aria-label="CodeSynapse, overview">
        <BrandMark :size="36" />
        <span class="brand__name">CodeSynapse</span>
      </NuxtLink>
      <a href="https://github.com/astrona-io/codesynapse" class="oss" rel="noopener">Open source</a>
      <label v-if="session.accounts.value && session.memberships.value.length > 1" class="org" for="org-select">
        <span class="org__label">Org</span>
        <select id="org-select" v-model="orgValue">
          <option v-for="m in session.memberships.value" :key="m.org.id" :value="m.org.slug">{{ m.org.name }}</option>
        </select>
      </label>
      <p v-else-if="session.current.value" class="org"><span class="org__label">Org</span><span class="org__name">{{ session.current.value.org.name }}</span></p>
      <QuickSearch v-if="!session.restricted.value" :items="searchItems" :repo="repo || undefined" />
      <div class="bar__end">
        <span class="status" :title="healthError ? 'API unreachable' : health ? 'Connected' : 'Connecting…'">
          <span class="dot" :class="healthError ? 'dot--bad' : health ? 'dot--ok' : 'dot--warn'" />
          <span class="sr-only">{{ healthError ? 'API unreachable' : health ? 'Connected' : 'Connecting…' }}</span>
        </span>
        <NotificationBell v-if="!session.restricted.value" />
        <UserMenu />
        <button type="button" class="secondary menu-toggle" :aria-expanded="menuOpen" aria-controls="sidebar" @click="menuOpen = !menuOpen">Menu</button>
      </div>
    </header>

    <div class="app__body">
      <aside id="sidebar" class="sidebar" :class="{ open: menuOpen }">
        <nav class="sidebar__nav" aria-label="Main">
          <div class="nav-section">
            <NuxtLink v-for="l in main" :key="l.to" :to="l.to" class="nav-link" :class="{ 'is-active': isActive(l.to) && !repo }" :title="l.hint">{{ l.label }}</NuxtLink>
          </div>
          <template v-if="repo">
            <div class="nav-section">
              <p class="nav-label">Repository</p>
              <p class="nav-repo mono" :title="repo">{{ repo }}</p>
            </div>
            <div v-for="g in repoMenu.groups" :key="g.label" class="nav-section">
              <p class="nav-label">{{ g.label }}</p>
              <NuxtLink v-for="l in g.links" :key="l.to" :to="l.to" class="nav-link nav-link--sub" :class="{ 'is-active': linkActive(l) }">{{ l.label }}</NuxtLink>
            </div>
            <details v-if="repoMenu.more.length" class="nav-section nav-more" :open="moreOpen" @toggle="moreOpen = ($event.target as HTMLDetailsElement).open">
              <summary class="nav-label nav-more__toggle">More · {{ repoMenu.more.length }} with nothing yet</summary>
              <NuxtLink v-for="l in repoMenu.more" :key="l.to" :to="l.to" class="nav-link nav-link--sub nav-link--empty" :class="{ 'is-active': linkActive(l) }" :title="l.missing">
                <span>{{ l.label }}</span>
                <span class="nav-why">{{ l.missing }}</span>
              </NuxtLink>
            </details>
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
        <div v-if="!session.restricted.value" class="connect">
          <p class="connect__title">Connect your agent</p>
          <p class="connect__text">Claude Code, Codex, Cursor and others ask the graph over MCP.</p>
          <NuxtLink to="/agents" class="btn">Set it up</NuxtLink>
        </div>
      </aside>
      <main id="main" class="main">
        <p v-if="session.restricted.value" class="notice">
          You are signed in as the root account: administration only.
          <template v-if="session.me.value?.setup === 'ambiguous'">This installation's setup history did not identify its root account, so every platform administrator is limited to administration until an operator runs <code>codesynapse-api designate-root --email …</code>.</template>
          <template v-else>For organisation work, sign out and sign in with a standard account.</template>
        </p>
        <slot />
      </main>
    </div>
  </div>
</template>

<style scoped>
.app { min-height: 100vh; display: flex; flex-direction: column; }
.skip { position: absolute; left: -9999px; }
.skip:focus { left: 12px; top: 12px; z-index: 40; background: var(--panel); padding: 8px 12px; border-radius: var(--radius-sm); }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }

.bar {
  position: sticky; top: 0; z-index: 20; display: flex; flex-wrap: wrap; align-items: center; gap: 12px 16px;
  padding: 10px var(--gutter); background: var(--bg); border-bottom: 2px solid var(--line);
}
.brand { display: flex; align-items: center; gap: 10px; color: var(--ink); }
.brand:hover { color: var(--ink); text-decoration: none; }
.brand__name { font: 800 22px/1 var(--font-display); letter-spacing: -0.04em; }
.oss { padding: 6px 12px; border-radius: var(--pill); background: var(--accent); color: var(--on-accent); font-size: 12.5px; font-weight: 700; }
.oss:hover { color: var(--on-accent); background: var(--accent-hover); text-decoration: none; }
.org { display: flex; align-items: center; gap: 8px; margin: 0; min-height: 44px; padding: 0 6px 0 14px; border-radius: var(--pill); background: var(--panel-2); font-size: 14px; font-weight: 600; }
.org__label { font: 700 11.5px var(--font-mono); color: var(--muted); text-transform: uppercase; }
.org__name { padding-right: 10px; }
.org select { width: auto; min-height: 36px; border: 0; background: transparent; padding: 0 8px; font-weight: 700; }
.bar__end { margin-left: auto; display: flex; align-items: center; gap: 12px; }
.status { display: inline-flex; padding: 6px; }

.app__body { flex: 1; display: flex; flex-wrap: wrap; align-items: stretch; }
.sidebar {
  flex: 0 0 248px; display: flex; flex-direction: column; gap: 22px; padding: 22px 16px;
  background: var(--panel-2); position: sticky; top: 70px; align-self: flex-start; min-height: calc(100vh - 70px); max-height: calc(100vh - 70px); overflow-y: auto;
}
.sidebar__nav { display: flex; flex-direction: column; gap: 14px; }
.nav-section { display: flex; flex-direction: column; gap: 2px; }
.nav-label { font: 700 11.5px var(--font-mono); text-transform: uppercase; letter-spacing: 0.04em; color: var(--muted); margin: 6px 14px 4px; }
.nav-repo { margin: 0 14px 2px; color: var(--ink); font-size: 13px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nav-link { display: flex; align-items: center; min-height: 40px; padding: 9px 14px; border-radius: 14px; color: var(--ink); font-size: 14.5px; font-weight: 600; }
.nav-link--sub { min-height: 36px; font-size: 14px; font-weight: 500; }
.nav-link:hover { background: var(--bg); color: var(--ink); text-decoration: none; }
.nav-link.is-active, .nav-link.is-active:hover { background: var(--primary); color: var(--on-primary); }
.nav-more__toggle { cursor: pointer; list-style-position: inside; }
.nav-link--empty { flex-direction: column; align-items: flex-start; gap: 1px; }
.nav-why { font-size: 12px; color: var(--muted); font-weight: 400; line-height: 1.3; }
.nav-link--empty.is-active .nav-why { color: inherit; }
.nav-repo-link { justify-content: space-between; gap: 8px; }
.nav-repo-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nav-repo-time { flex: none; font-size: 12px; color: var(--muted); }
.nav-repo-link.is-active .nav-repo-time { color: inherit; }
.connect { margin-top: auto; display: flex; flex-direction: column; gap: 6px; padding: 18px; border-radius: 20px; background: var(--bg); }
.connect__title { font: 800 16px/1.2 var(--font-display); letter-spacing: -0.02em; }
.connect__text { font-size: 13px; color: var(--muted); margin-bottom: 6px; }
.main { flex: 999 1 560px; min-width: 0; display: flex; flex-direction: column; gap: 22px; padding: 30px var(--gutter) 48px; max-width: var(--shell-max); }
.menu-toggle { display: none; }

@media (max-width: 960px) {
  .sidebar { flex: 1 1 100%; position: static; max-height: none; }
  .sidebar:not(.open) { display: none; }
  .menu-toggle { display: inline-flex; }
  .oss, .status { display: none; }
}
</style>
