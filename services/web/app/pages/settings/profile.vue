<script setup lang="ts">
import type { ReachView } from '~/utils/types'

const session = useSession()
const { data: access } = useApiGet<{ repos: ReachView[] }>('me/access')
const shared = computed(() => (access.value?.repos ?? []).filter((r) => r.shared))
const current = ref('')
const next = ref('')
const msg = ref('')
const error = ref('')
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
  </div>
</template>

<style scoped>
.facts { display: grid; grid-template-columns: max-content 1fr; gap: 6px 16px; margin: 0; }
.facts dt { color: var(--muted); }
.facts dd { margin: 0; }
.plain { list-style: none; padding: 0; display: grid; gap: 6px; }
</style>
