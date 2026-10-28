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
// The same message for an unknown account and a wrong password, so the page
// never tells anyone whether an email is registered.
const LOGIN_ERRORS = {
  401: 'The email or username and password do not match. Check them and try again.',
  403: 'This account cannot sign in. Ask an administrator of your installation.',
  429: 'Too many sign-in attempts. Wait a minute and try again.',
}
const noSSO = 'Single sign-on is not available for that organisation. Check its short name or sign in with your password.'
const SSO_ERRORS = { 403: noSSO, 404: noSSO, 409: noSSO, 502: 'Your organisation\'s identity provider could not be reached. Try again in a moment.' }
// Focus moves to the message so keyboard and screen reader users hear it.
const errorEl = ref<HTMLElement | null>(null)
const ssoError = ref('')
const ssoErrorEl = ref<HTMLElement | null>(null)
watch([error, ssoError], async ([e, se]) => {
  if (!e && !se) return
  await nextTick()
  ;(e ? errorEl : ssoErrorEl).value?.focus()
})
onMounted(() => {
  // An administrator's sign-in link names the organisation: /login?sso=acme
  const linked = route.query.sso
  if (typeof linked === 'string') ssoOrg.value = linked
  else try { ssoOrg.value = localStorage.getItem('cs_sso_org') ?? '' } catch { /* storage blocked */ }
})
async function sso() {
  if (ssoBusy.value) return
  ssoBusy.value = true
  ssoError.value = ''
  const slug = ssoOrg.value.trim().toLowerCase()
  try {
    try { localStorage.setItem('cs_sso_org', slug) } catch { /* storage blocked */ }
    await startSSO(`auth/sso/${encodeURIComponent(slug)}/start`, next.value)
  } catch (e) {
    ssoError.value = plainError(e, SSO_ERRORS)
    ssoBusy.value = false
  }
}
async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  ssoError.value = ''
  try {
    await $fetch('/api/cg/auth/login', { method: 'POST', body: { login: email.value, password: password.value } })
    await session.load()
    await navigateTo(next.value)
  } catch (e) {
    error.value = plainError(e, LOGIN_ERRORS)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>Welcome back</h1>
    <p class="muted">Use the email or username and the password of your CodeSynapse account.</p>
  </div>
  <p v-if="session.status.value?.initial_admin" class="notice">
    <span>First start? Sign in as <strong>admin</strong>. With <code>just up</code> the password is <code>ADMIN_PASSWORD</code> in <code>.env</code>; on Kubernetes it is in the <code>&lt;release&gt;-admin</code> Secret. You replace this account right after.</span>
  </p>
  <form class="stack--tight" @submit.prevent="submit">
    <div class="field"><label for="email">Email or username</label><input id="email" v-model="email" type="text" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus></div>
    <div class="field"><label for="password">Password</label><input id="password" v-model="password" type="password" autocomplete="current-password" required></div>
    <p v-if="error" ref="errorEl" class="notice notice--danger" role="alert" tabindex="-1">{{ error }}</p>
    <button type="submit" class="accent" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
  </form>
  <details class="sso" :open="!!ssoOrg">
    <summary>Sign in with single sign-on</summary>
    <form class="stack--tight" @submit.prevent="sso">
      <div class="field"><label for="sso-org">Organisation</label><input id="sso-org" v-model="ssoOrg" autocomplete="organization" autocapitalize="none" spellcheck="false" placeholder="acme" pattern="[A-Za-z0-9][A-Za-z0-9\-]*" title="Letters, digits and dashes, such as acme" aria-describedby="sso-org-hint" required><span id="sso-org-hint" class="hint">Its short name, such as <code>acme</code>. Your administrators can send you a sign-in link that fills this in.</span></div>
      <p v-if="ssoError" ref="ssoErrorEl" class="notice notice--danger" role="alert" tabindex="-1">{{ ssoError }}</p>
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
