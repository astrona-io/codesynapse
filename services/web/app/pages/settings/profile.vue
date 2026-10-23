<script setup lang="ts">
import type { ExternalIdentity, ReachView } from '~/utils/types'

const session = useSession()
const { data: access } = useApiGet<{ repos: ReachView[] }>('me/access')
const shared = computed(() => (access.value?.repos ?? []).filter((r) => r.shared))
const current = ref('')
const next = ref('')
const msg = ref('')
const error = ref('')
// Single sign-on identities. Root never signs in this way, so it gets none.
const { data: idents, refresh: refreshIdents } = useApiGet<{ identities: ExternalIdentity[] }>('me/identities', { immediate: !session.restricted.value })
const orgName = (id: string) => session.memberships.value.find((m) => m.org.id === id)?.org.name ?? 'a former organisation'
const ssoError = ref('')
async function link(slug: string) {
  ssoError.value = ''
  try {
    await startSSO(`me/sso/${encodeURIComponent(slug)}/link`)
  } catch (e) {
    ssoError.value = apiErrorMessage(e)
  }
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
  } catch (e) {
    ssoError.value = apiErrorMessage(e)
  }
}
async function change() {
  msg.value = ''
  error.value = ''
  try {
    await apiPost('me/password', { current: current.value, next: next.value })
    msg.value = 'Password changed. Other devices were signed out.'
    current.value = ''
    next.value = ''
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
</script>

<template>
  <div v-if="session.me.value" class="grid cols-2">
    <section class="panel">
      <h2>You</h2>
      <dl class="facts">
        <dt>Name</dt><dd>{{ session.me.value.user.name }}</dd>
        <dt>Email</dt><dd>{{ session.me.value.user.email }}</dd>
        <dt>Member since</dt><dd>{{ new Date(session.me.value.user.created_at).toLocaleDateString() }}</dd>
        <template v-if="session.me.value.user.platform_admin"><dt>Role</dt><dd><span class="badge badge--orange">Platform administrator</span></dd></template>
      </dl>
      <h3>Organisations</h3>
      <ul class="plain">
        <li v-for="m in session.memberships.value" :key="m.org.id">{{ m.org.name }} <span class="badge">{{ m.role }}</span></li>
      </ul>
      <template v-if="shared.length">
        <h3>Shared with you</h3>
        <ul class="plain">
          <li v-for="r in shared" :key="r.org_id + r.graph"><NuxtLink :to="`/repos/${encodeURIComponent(r.graph)}`">{{ r.graph }}</NuxtLink> <span class="muted small">from {{ r.org }}, read-only</span></li>
        </ul>
      </template>
    </section>
    <section class="panel">
      <h2>Change password</h2>
      <p class="muted small">Changing it signs you out everywhere else.</p>
      <form class="stack--tight" @submit.prevent="change">
        <input type="email" autocomplete="username" :value="session.me.value.user.email" readonly hidden>
        <div class="field"><label for="cur">Current password</label><input id="cur" v-model="current" type="password" autocomplete="current-password" required></div>
        <div class="field"><label for="new">New password</label><input id="new" v-model="next" type="password" autocomplete="new-password" minlength="12" required><span class="hint">At least 12 characters.</span></div>
        <p v-if="msg" class="notice notice--ok" role="status">{{ msg }}</p>
        <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
        <div><button type="submit">Change password</button></div>
      </form>
    </section>
    <section v-if="!session.restricted.value" class="panel">
      <h2>Single sign-on</h2>
      <p class="muted small">Sign in through your organisation's identity provider. Linking needs the provider to confirm the same person; you can unlink unless it is your only way to sign in.</p>
      <ul v-if="idents?.identities.length" class="plain">
        <li v-for="i in idents.identities" :key="i.id" class="ident">
          <span><strong>{{ orgName(i.org_id) }}</strong> <span class="muted small mono">{{ i.issuer }}</span></span>
          <span class="muted small">{{ i.email }} · linked {{ relTime(i.linked_at) }}<template v-if="i.last_login_at"> · last used {{ relTime(i.last_login_at) }}</template></span>
          <button class="secondary tiny" @click="unlinking = i">Unlink</button>
        </li>
      </ul>
      <p v-else class="muted small">No identities linked.</p>
      <h3>Link an organisation's provider</h3>
      <div class="row">
        <button v-for="m in session.memberships.value" :key="m.org.id" class="secondary tiny" @click="link(m.org.slug)">{{ m.org.name }}</button>
      </div>
      <p v-if="ssoError" class="notice notice--danger" role="alert">{{ ssoError }}</p>
    </section>
    <ConfirmDialog :open="!!unlinking" title="Unlink this identity?" confirm-label="Unlink" danger @confirm="unlink" @cancel="unlinking = null">
      You will no longer sign in through {{ unlinking ? orgName(unlinking.org_id) : '' }}'s identity provider with this account.
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.facts { display: grid; grid-template-columns: max-content 1fr; gap: 6px 16px; margin: 0; }
.facts dt { color: var(--muted); }
.facts dd { margin: 0; }
.plain { list-style: none; padding: 0; display: grid; gap: 6px; }
.ident { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 12px; }
.ident > span:first-child { flex: 1 1 100%; }
</style>
