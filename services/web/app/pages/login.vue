<script setup lang="ts">
definePageMeta({ layout: 'auth' })
useHead({ title: 'Sign in · CodeSynapse' })
const route = useRoute()
const session = useSession()
const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)
// Only paths inside this app: never an absolute or protocol-relative URL.
const next = computed(() => safeNext(route.query.next))
// Single sign-on goes through the organisation's own provider; the slug is
// remembered on this device only as a convenience.
const ssoOrg = ref('')
const ssoBusy = ref(false)
onMounted(() => {
  // An administrator's sign-in link names the organisation: /login?sso=acme
  const linked = route.query.sso
  if (typeof linked === 'string') ssoOrg.value = linked
  else try { ssoOrg.value = localStorage.getItem('cs_sso_org') ?? '' } catch { /* storage blocked */ }
})
async function sso() {
  ssoBusy.value = true
  error.value = ''
  const slug = ssoOrg.value.trim().toLowerCase()
  try {
    try { localStorage.setItem('cs_sso_org', slug) } catch { /* storage blocked */ }
    await startSSO(`auth/sso/${encodeURIComponent(slug)}/start`, next.value)
  } catch (e) {
    error.value = apiErrorMessage(e)
    ssoBusy.value = false
  }
}
async function submit() {
  busy.value = true
  error.value = ''
  try {
    await $fetch('/api/cg/auth/login', { method: 'POST', body: { email: email.value, password: password.value } })
    await session.load()
    await navigateTo(next.value)
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>Welcome back</h1>
    <p class="muted">Use the email and password of your CodeSynapse account.</p>
  </div>
  <form class="stack--tight" @submit.prevent="submit">
    <div class="field"><label for="email">Email</label><input id="email" v-model="email" type="email" autocomplete="username" required autofocus></div>
    <div class="field"><label for="password">Password</label><input id="password" v-model="password" type="password" autocomplete="current-password" required></div>
    <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    <button type="submit" class="accent" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
  </form>
  <details class="sso" :open="!!ssoOrg">
    <summary>Sign in with single sign-on</summary>
    <form class="stack--tight" @submit.prevent="sso">
      <div class="field"><label for="sso-org">Organisation</label><input id="sso-org" v-model="ssoOrg" autocomplete="organization" placeholder="acme" pattern="[a-z0-9][a-z0-9-]*" required><span class="hint">Its short name, such as <code>acme</code>. Your administrators can send you a sign-in link that fills this in.</span></div>
      <button type="submit" class="secondary" :disabled="ssoBusy || !ssoOrg.trim()">{{ ssoBusy ? 'Opening your identity provider…' : 'Continue' }}</button>
    </form>
  </details>
  <p v-if="session.status.value?.signup_open" class="muted small">No account yet? <NuxtLink to="/signup">Create one</NuxtLink></p>
  <p v-else class="muted small">No account yet? Ask an administrator of your installation to add you.</p>
</template>

<style scoped>
.sso summary { cursor: pointer; color: var(--muted); }
.sso[open] summary { margin-bottom: 10px; }
</style>
