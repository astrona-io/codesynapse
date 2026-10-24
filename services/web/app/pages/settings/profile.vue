<script setup lang="ts">
import type { ExternalIdentity, ReachView, SessionView } from '~/utils/types'

// Your own account: who you are (name, email), how you sign in (password,
// sessions, single sign-on) and where you belong (organisations). Every
// change needs this signed-in browser session; the API checks it.
const session = useSession()
const route = useRoute()
const router = useRouter()
const user = computed(() => session.me.value?.user)

type Tab = 'profile' | 'security' | 'organisations'
const tabs: { id: Tab; label: string }[] = [{ id: 'profile', label: 'Profile' }, { id: 'security', label: 'Security' }, { id: 'organisations', label: 'Organisations' }]
const tab = computed<Tab>(() => (tabs.some((t) => t.id === route.query.tab) ? route.query.tab as Tab : 'profile'))
const pick = (t: Tab) => router.replace({ query: { ...route.query, tab: t === 'profile' ? undefined : t } })

const initials = computed(() => {
  const parts = (user.value?.name || user.value?.email || '').split(/[\s@._-]+/).filter(Boolean)
  return ((parts[0]?.[0] ?? '') + (parts[1]?.[0] ?? '')).toUpperCase() || '·'
})

// Profile: name and email.
const name = ref(user.value?.name ?? '')
const nameMsg = ref('')
const nameError = ref('')
async function saveName() {
  nameMsg.value = nameError.value = ''
  try {
    await apiPatch('me', { name: name.value })
    await session.load()
    nameMsg.value = 'Name saved.'
  } catch (e) { nameError.value = apiErrorMessage(e) }
}
const newEmail = ref('')
const emailPassword = ref('')
const emailMsg = ref('')
const emailError = ref('')
async function changeEmail() {
  emailMsg.value = emailError.value = ''
  try {
    await apiPost('me/email', { email: newEmail.value, password: emailPassword.value })
    await session.load()
    emailMsg.value = 'Email changed. Your other sessions were signed out.'
    newEmail.value = emailPassword.value = ''
  } catch (e) { emailError.value = apiErrorMessage(e) }
}

// Security: password, sessions, single sign-on.
const current = ref('')
const next = ref('')
const pwMsg = ref('')
const pwError = ref('')
async function changePassword() {
  pwMsg.value = pwError.value = ''
  try {
    await apiPost('me/password', { current: current.value, next: next.value })
    pwMsg.value = 'Password changed. Other devices were signed out.'
    current.value = next.value = ''
  } catch (e) { pwError.value = apiErrorMessage(e) }
}
const { data: sessions, refresh: refreshSessions } = useApiGet<{ sessions: SessionView[] }>('me/sessions')
const sessionError = ref('')
async function endSession(s: SessionView) {
  sessionError.value = ''
  try {
    await apiDelete(`me/sessions/${encodeURIComponent(s.id)}`)
    if (s.current) { await session.load(); await navigateTo('/login'); return }
    await refreshSessions()
  } catch (e) { sessionError.value = apiErrorMessage(e) }
}
const endingOthers = ref(false)
async function endOthers() {
  endingOthers.value = false
  sessionError.value = ''
  try {
    await apiPost('me/sessions/end-others', {})
    await refreshSessions()
  } catch (e) { sessionError.value = apiErrorMessage(e) }
}
const others = computed(() => (sessions.value?.sessions ?? []).filter((s) => !s.current).length)

// Root never signs in with single sign-on, so it has no identities.
const { data: idents, refresh: refreshIdents } = useApiGet<{ identities: ExternalIdentity[] }>('me/identities', { immediate: !session.restricted.value })
const orgName = (id: string) => session.memberships.value.find((m) => m.org.id === id)?.org.name ?? 'a former organisation'
const ssoError = ref('')
async function link(slug: string) {
  ssoError.value = ''
  try { await startSSO(`me/sso/${encodeURIComponent(slug)}/link`) } catch (e) { ssoError.value = apiErrorMessage(e) }
}
const unlinking = ref<ExternalIdentity | null>(null)
async function unlink() {
  const i = unlinking.value
  unlinking.value = null
  if (!i) return
  ssoError.value = ''
  try {
    await apiDelete(`me/identities/${encodeURIComponent(i.id)}`)
    await refreshIdents()
  } catch (e) { ssoError.value = apiErrorMessage(e) }
}

// Organisations.
const { data: access } = useApiGet<{ repos: ReachView[] }>('me/access', { immediate: !session.restricted.value })
const shared = computed(() => (access.value?.repos ?? []).filter((r) => r.shared))
async function open(slug: string, to: string) {
  session.switchOrg(slug)
  await navigateTo(to)
}
const manages = (role: string) => role === 'owner' || role === 'admin'
</script>

<template>
  <div v-if="user" class="stack">
    <section class="panel head" aria-label="Your account">
      <span class="avatar" aria-hidden="true">{{ initials }}</span>
      <div class="head__who">
        <h2 class="head__name">{{ user.name }}</h2>
        <p class="muted">{{ user.email }}</p>
        <div class="row">
          <span v-for="m in session.memberships.value" :key="m.org.id" class="pill">{{ m.role[0]!.toUpperCase() + m.role.slice(1) }} · {{ m.org.name }}</span>
          <span v-if="user.platform_admin" class="pill pill--accent">Platform admin</span>
        </div>
      </div>
    </section>

    <div class="tabs" role="tablist" aria-label="Account sections">
      <a v-for="t in tabs" :key="t.id" :href="`?tab=${t.id}`" role="tab" :aria-selected="tab === t.id" :class="{ 'router-link-exact-active': tab === t.id }" @click.prevent="pick(t.id)">{{ t.label }}</a>
    </div>

    <!-- Profile -->
    <div v-if="tab === 'profile'" class="grid cols-2" role="tabpanel">
      <section class="panel">
        <h2>Profile</h2>
        <form class="stack--tight" @submit.prevent="saveName">
          <div class="field"><label for="name">Name</label><input id="name" v-model="name" type="text" autocomplete="name" maxlength="100" required><span class="hint">Shown to your colleagues and in the audit log.</span></div>
          <p v-if="nameMsg" class="notice notice--ok" role="status">{{ nameMsg }}</p>
          <p v-if="nameError" class="notice notice--danger" role="alert">{{ nameError }}</p>
          <div><button type="submit" :disabled="name.trim() === user.name">Save name</button></div>
        </form>
        <p class="muted small">Member since {{ new Date(user.created_at).toLocaleDateString() }}.</p>
      </section>
      <section class="panel">
        <h2>Email</h2>
        <p class="muted small">You sign in with <strong>{{ user.email }}</strong>. Changing it needs your password and signs you out everywhere else. Accounts made through single sign-on change their email at the identity provider.</p>
        <form class="stack--tight" @submit.prevent="changeEmail">
          <div class="field"><label for="email">New email</label><input id="email" v-model="newEmail" type="email" autocomplete="email" required></div>
          <div class="field"><label for="email-pw">Current password</label><input id="email-pw" v-model="emailPassword" type="password" autocomplete="current-password" required></div>
          <p v-if="emailMsg" class="notice notice--ok" role="status">{{ emailMsg }}</p>
          <p v-if="emailError" class="notice notice--danger" role="alert">{{ emailError }}</p>
          <div><button type="submit">Change email</button></div>
        </form>
      </section>
    </div>

    <!-- Security -->
    <div v-else-if="tab === 'security'" class="grid cols-2" role="tabpanel">
      <section class="panel">
        <h2>Password</h2>
        <p class="muted small">Changing it signs you out everywhere else.</p>
        <form class="stack--tight" @submit.prevent="changePassword">
          <input type="email" autocomplete="username" :value="user.email" readonly hidden>
          <div class="field"><label for="cur">Current password</label><input id="cur" v-model="current" type="password" autocomplete="current-password" required></div>
          <div class="field"><label for="new">New password</label><input id="new" v-model="next" type="password" autocomplete="new-password" minlength="12" required><span class="hint">At least 12 characters.</span></div>
          <p v-if="pwMsg" class="notice notice--ok" role="status">{{ pwMsg }}</p>
          <p v-if="pwError" class="notice notice--danger" role="alert">{{ pwError }}</p>
          <div><button type="submit">Update password</button></div>
        </form>
      </section>
      <section class="panel">
        <div class="panel__header">
          <h2>Active sessions</h2>
          <button v-if="others" type="button" class="secondary tiny" @click="endingOthers = true">Sign out everywhere else</button>
        </div>
        <ul v-if="sessions?.sessions.length" class="sessions">
          <li v-for="s in sessions.sessions" :key="s.id" class="session">
            <span class="session__what">
              <strong>{{ deviceName(s.device) }}</strong>
              <span class="muted small">Signed in {{ relTime(s.created_at) }}</span>
            </span>
            <span v-if="s.current" class="pill">This device</span>
            <button v-else type="button" class="secondary tiny" @click="endSession(s)">Sign out</button>
          </li>
        </ul>
        <p v-else class="muted small">No sessions.</p>
        <p v-if="sessionError" class="notice notice--danger" role="alert">{{ sessionError }}</p>
      </section>
      <section v-if="!session.restricted.value" class="panel">
        <h2>Single sign-on</h2>
        <p class="muted small">Sign in through your organisation's identity provider. Linking needs the provider to confirm the same person; you can unlink unless it is your only way to sign in.</p>
        <ul v-if="idents?.identities.length" class="sessions">
          <li v-for="i in idents.identities" :key="i.id" class="session">
            <span class="session__what">
              <strong>{{ orgName(i.org_id) }}</strong>
              <span class="muted small">{{ i.email }} · linked {{ relTime(i.linked_at) }}<template v-if="i.last_login_at"> · last used {{ relTime(i.last_login_at) }}</template></span>
            </span>
            <button type="button" class="secondary tiny" @click="unlinking = i">Unlink</button>
          </li>
        </ul>
        <p v-else class="muted small">No identities linked.</p>
        <p class="label">Link an organisation's provider</p>
        <div class="row">
          <button v-for="m in session.memberships.value" :key="m.org.id" type="button" class="secondary tiny" @click="link(m.org.slug)">{{ m.org.name }}</button>
        </div>
        <p v-if="ssoError" class="notice notice--danger" role="alert">{{ ssoError }}</p>
      </section>
    </div>

    <!-- Organisations -->
    <div v-else class="grid cols-2" role="tabpanel">
      <section class="panel">
        <h2>Your organisations</h2>
        <p class="muted small">Organisations are created by the platform administrator; their owners add members.</p>
        <ul v-if="session.memberships.value.length" class="sessions">
          <li v-for="m in session.memberships.value" :key="m.org.id" class="session">
            <span class="session__what">
              <strong>{{ m.org.name }}</strong>
              <span class="muted small">{{ m.role[0]!.toUpperCase() + m.role.slice(1) }} · member since {{ new Date(m.added_at).toLocaleDateString() }}</span>
            </span>
            <span class="row">
              <button type="button" class="secondary tiny" @click="open(m.org.slug, '/overview')">Open</button>
              <button v-if="manages(m.role)" type="button" class="secondary tiny" @click="open(m.org.slug, '/settings/organisation')">Manage</button>
            </span>
          </li>
        </ul>
        <p v-else class="muted small">You belong to no organisation yet.</p>
      </section>
      <section v-if="shared.length" class="panel">
        <h2>Shared with you</h2>
        <ul class="sessions">
          <li v-for="r in shared" :key="r.org_id + r.graph" class="session">
            <span class="session__what">
              <NuxtLink :to="`/repos/${encodeURIComponent(r.graph)}`" class="mono">{{ r.graph }}</NuxtLink>
              <span class="muted small">from {{ r.org }}, read-only</span>
            </span>
          </li>
        </ul>
      </section>
    </div>

    <ConfirmDialog :open="!!unlinking" title="Unlink this identity?" confirm-label="Unlink" danger @confirm="unlink" @cancel="unlinking = null">
      You will no longer sign in through {{ unlinking ? orgName(unlinking.org_id) : '' }}'s identity provider with this account.
    </ConfirmDialog>
    <ConfirmDialog :open="endingOthers" title="Sign out everywhere else?" confirm-label="Sign out" @confirm="endOthers" @cancel="endingOthers = false">
      Every other browser signed in to your account is signed out. This one stays signed in.
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.head { flex-direction: row; align-items: center; gap: 22px; flex-wrap: wrap; }
.avatar { flex: none; width: 88px; height: 88px; border-radius: 28px; display: grid; place-items: center; background: var(--accent); color: var(--on-accent); font: 800 32px var(--font-display); letter-spacing: -0.03em; }
.head__who { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.head__name { font-size: clamp(26px, 3vw, 34px); }
.pill--accent { background: var(--accent-soft); }
.sessions { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
.session { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; padding: 12px 14px; border-radius: var(--radius-sm); background: var(--panel-2); }
.session__what { display: flex; flex-direction: column; min-width: 0; }
</style>
