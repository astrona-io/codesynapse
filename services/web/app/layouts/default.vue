<script setup lang="ts">
import type { RepoInfo } from '~/utils/types'

const route = useRoute()
const repo = computed(() => (route.params.repo as string | undefined) ?? '')
const menuOpen = ref(false)
watch(() => route.fullPath, () => (menuOpen.value = false))

const { data: repos } = useApiGet<{ repos: RepoInfo[] }>('repos')
const { data: health, error: healthError } = useFetch<{ status: string }>('/api/cg/version', { key: 'version' })

const repoGroups = computed(() => {
  if (!repo.value) return []
  const base = `/repos/${encodeURIComponent(repo.value)}`
  return [
    { label: 'Understand', links: [
      { to: base, label: 'Summary', hint: 'Health and next steps' },
      { to: `${base}/graph`, label: 'Graph', hint: 'Explore how code connects' },
      { to: `${base}/explore`, label: 'Symbols', hint: 'Look up any function or type' },
      { to: `${base}/config`, label: 'Kubernetes & Helm', hint: 'Manifests, charts, values' },
      { to: `${base}/docs`, label: 'Docs', hint: 'Markdown and what it describes' },
      { to: `${base}/packages`, label: 'Packages', hint: 'Architecture and imports' },
    ] },
    { label: 'Improve', links: [
      { to: `${base}/security`, label: 'Security', hint: 'Findings and vulnerable dependencies' },
      { to: `${base}/cleanup`, label: 'Cleanup', hint: 'Unused code to remove' },
      { to: `${base}/metrics`, label: 'Quality metrics', hint: 'Complexity, tests, token savings' },
      { to: `${base}/ai`, label: 'AI assistant', hint: 'Triage, review and briefings by a model' },
    ] },
    { label: 'Change', links: [
      { to: `${base}/reviews`, label: 'Reviews', hint: 'Code reviews submitted by agents' },
      { to: `${base}/review`, label: 'Impact of a change', hint: 'Impact and risk of a diff' },
    ] },
  ]
})
const isActive = (to: string) => route.path === to
</script>

<template>
  <div class="console">
    <SpaceBackdrop />
    <aside class="sidebar panel--glass" :class="{ open: menuOpen }">
      <div class="sidebar__top">
        <NuxtLink to="/" class="brand">
          <span class="brand__logo"><img src="/favicon.svg" alt="" width="26" height="26"></span>
          <span>
            <span class="brand__name">CodeSynapse</span>
            <span class="brand__tag">code intelligence</span>
          </span>
        </NuxtLink>
        <button class="secondary tiny menu-toggle" :aria-expanded="menuOpen" @click="menuOpen = !menuOpen">Menu</button>
      </div>

      <nav class="sidebar__nav">
        <div class="nav-section">
          <p class="nav-label">Workspace</p>
          <NuxtLink to="/" class="nav-link" :class="{ 'is-active': route.path === '/' }" title="Your indexed repositories and what to add">Repositories</NuxtLink>
          <NuxtLink to="/orgs" class="nav-link" :class="{ 'is-active': route.path === '/orgs' }" title="Index every repository of a GitHub organisation">Organisations</NuxtLink>
          <NuxtLink to="/overview" class="nav-link" :class="{ 'is-active': route.path === '/overview' }" title="Everything at a glance, worst first">Workspace health</NuxtLink>
          <NuxtLink to="/ai" class="nav-link" :class="{ 'is-active': route.path === '/ai' }" title="Local and cloud language models">AI models</NuxtLink>
        </div>
        <template v-if="repo">
          <div class="nav-repo-head">
            <p class="nav-label">Repository</p>
            <p class="nav-repo mono" :title="repo">{{ repo }}</p>
          </div>
          <div v-for="g in repoGroups" :key="g.label" class="nav-section">
            <p class="nav-label">{{ g.label }}</p>
            <NuxtLink v-for="l in g.links" :key="l.to" :to="l.to" class="nav-link" :class="{ 'is-active': isActive(l.to) }" :title="l.hint">{{ l.label }}</NuxtLink>
          </div>
        </template>
        <div v-else-if="repos?.repos.length" class="nav-section">
          <p class="nav-label">Indexed</p>
          <NuxtLink v-for="r in repos.repos.slice(0, 8)" :key="r.name" :to="`/repos/${encodeURIComponent(r.name)}`" class="nav-link mono">{{ r.name }}</NuxtLink>
        </div>
      </nav>

      <div class="sidebar__foot">
        <span class="dot" :class="healthError ? 'dot--bad' : health ? 'dot--ok' : 'dot--warn'" />
        <span class="muted">{{ healthError ? 'API unreachable' : health ? 'API connected' : 'Connecting…' }}</span>
      </div>
    </aside>
    <main id="main" class="console__main"><slot /></main>
  </div>
</template>

<style scoped>
.console { position: relative; display: grid; grid-template-columns: 272px minmax(0, 1fr); gap: 24px; padding: 24px var(--gutter); min-height: 100vh; }
.console__main { position: relative; z-index: 1; display: flex; flex-direction: column; gap: 24px; max-width: var(--shell-max); min-width: 0; }
.sidebar {
  position: sticky; top: 24px; z-index: 2; align-self: start; max-height: calc(100vh - 48px); overflow-y: auto;
  display: flex; flex-direction: column; gap: 18px; padding: 20px 16px;
  border: 1px solid var(--line); border-radius: var(--radius);
}
.sidebar__top { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.brand { display: flex; align-items: center; gap: 12px; color: var(--ink); }
.brand:hover { color: var(--ink); }
.brand__logo { display: grid; place-items: center; width: 42px; height: 42px; border-radius: 13px; background: var(--panel); border: 1px solid var(--line); box-shadow: 0 0 14px rgba(255, 122, 24, 0.25); }
.brand__name { display: block; font-size: 18px; font-weight: 700; letter-spacing: 0.02em; line-height: 1.2; }
.brand__tag { display: block; font: 400 9px var(--font-mono); text-transform: uppercase; letter-spacing: 0.16em; color: var(--green); }
.sidebar__nav { display: flex; flex-direction: column; gap: 14px; }
.nav-section { display: flex; flex-direction: column; gap: 2px; padding-top: 14px; border-top: 1px dashed var(--line); }
.nav-label { font: 400 10px var(--font-mono); text-transform: uppercase; letter-spacing: 0.14em; color: var(--violet); margin: 0 12px 6px; }
.nav-repo-head { padding-top: 14px; border-top: 1px dashed var(--line); }
.nav-repo { margin: -2px 12px 0; color: var(--ink); font-size: 13.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nav-link {
  display: flex; align-items: center; min-height: 40px; padding: 8px 12px;
  border-left: 2px solid transparent; border-radius: var(--radius-xs);
  color: var(--muted); font-size: 14px; font-weight: 500;
}
.nav-link:hover { color: var(--ink); background: rgba(236, 231, 245, 0.04); }
.nav-link.is-active { color: var(--ink); background: var(--tint-orange); border-left-color: var(--orange); }
.sidebar__foot { margin-top: auto; display: flex; align-items: center; gap: 10px; padding-top: 14px; border-top: 1px dashed var(--line); font-size: 13px; }
.menu-toggle { display: none; }
@media (max-width: 1080px) {
  .console { grid-template-columns: 1fr; }
  .sidebar { position: relative; top: 0; max-height: none; }
  .menu-toggle { display: inline-flex; }
  .sidebar:not(.open) .sidebar__nav, .sidebar:not(.open) .sidebar__foot { display: none; }
}
</style>
