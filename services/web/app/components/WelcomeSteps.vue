<script setup lang="ts">
import type { RepoInfo } from '~/utils/types'

// The welcome guide: what to do next as a new user, as steps you can follow
// or skip. Root gets the installation steps (replace the first-start admin,
// create the organisation and its owner); everyone else gets the product
// steps. Steps that can be detected tick themselves.
const session = useSession()
const welcome = useWelcome()
const route = useRoute()

const { data: repos } = useApiGet<{ repos: RepoInfo[] }>('repos', { immediate: !session.restricted.value })
const role = computed(() => session.current.value?.role)
const canManage = computed(() => role.value === 'owner' || role.value === 'admin')

interface Step { id: string; title: string; text: string; to: string; action: string; done: boolean; manual?: boolean }
const steps = computed<Step[]>(() => {
  if (session.restricted.value) {
    return [
      ...(session.autoCreated.value ? [{ id: 'replace-admin', title: 'Replace the admin account', text: 'This account was made automatically at first start. Create your own and delete it.', to: '/replace-admin', action: 'Replace it', done: false }] : []),
      ...(session.setupPending.value ? [{ id: 'organisation', title: 'Create the first organisation', text: 'An organisation holds your repositories and people. Its owner is a separate account, because root only administers the platform.', to: '/setup', action: 'Create it', done: false }] : []),
    ]
  }
  return [
    { id: 'repository', title: 'Add your first repository', text: 'Index a repository so the graph can answer questions about it.', to: '/', action: 'Add one', done: (repos.value?.repos.length ?? 0) > 0 },
    { id: 'agent', title: 'Connect your coding agent', text: 'Claude Code, Codex, Cursor and others ask the graph over MCP.', to: '/agents', action: 'Set it up', done: welcome.isDone('agent'), manual: true },
    ...(canManage.value ? [{ id: 'team', title: 'Invite your team', text: 'Add people and choose what each may do.', to: '/settings', action: 'Invite', done: welcome.isDone('team'), manual: true }] : []),
  ]
})
const open = computed(() => steps.value.filter((s) => !s.done))
// Only on the pages people land on, and never while it has nothing to say.
const LANDING = new Set(['/', '/overview', '/admin/overview'])
const visible = computed(() => !welcome.skipped.value && open.value.length > 0 && LANDING.has(route.path))
const finished = computed(() => steps.value.length - open.value.length)
</script>

<template>
  <section v-if="visible" class="welcome panel" aria-labelledby="welcome-title">
    <div class="welcome__head">
      <div>
        <p class="label">Welcome</p>
        <h2 id="welcome-title">Get started with CodeSynapse</h2>
        <p class="muted small">{{ finished }} of {{ steps.length }} steps done. Follow them in any order, or skip the guide; you can reopen it from your account menu.</p>
      </div>
      <button type="button" class="secondary tiny" @click="welcome.skip()">Skip the guide</button>
    </div>
    <ol class="welcome__steps">
      <li v-for="s in steps" :key="s.id" class="step" :class="{ 'step--done': s.done }">
        <span class="step__mark" aria-hidden="true">{{ s.done ? '✓' : '' }}</span>
        <div class="step__body">
          <strong>{{ s.title }}</strong>
          <span class="muted small">{{ s.text }}</span>
        </div>
        <div class="step__act">
          <NuxtLink v-if="!s.done" :to="s.to" class="btn accent tiny" @click="s.manual && welcome.markDone(s.id)">{{ s.action }}</NuxtLink>
          <span v-else class="muted small">Done</span>
        </div>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.welcome { display: flex; flex-direction: column; gap: 16px; }
.welcome__head { display: flex; flex-wrap: wrap; gap: 12px; justify-content: space-between; align-items: flex-start; }
.welcome__head h2 { margin: 2px 0 4px; }
.welcome__steps { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
.step { display: flex; align-items: center; gap: 14px; padding: 12px 14px; border: 2px solid var(--line); border-radius: var(--radius-sm); background: var(--panel-raised); }
.step--done { opacity: 0.7; }
.step__mark { flex: none; width: 26px; height: 26px; border-radius: var(--pill); border: 2px solid var(--line-strong); display: grid; place-items: center; font-weight: 800; }
.step--done .step__mark { background: var(--success-soft); border-color: var(--success); color: var(--success); }
.step__body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.step__act { flex: none; }
</style>
