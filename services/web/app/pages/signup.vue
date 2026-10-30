<script setup lang="ts">
definePageMeta({ layout: 'auth' })
const session = useSession()
const route = useRoute()
// An invitation link sends people here and back again once they are in.
const next = computed(() => safeNext(route.query.next))
const setup = computed(() => !!session.status.value?.needs_setup)
useHead({ title: () => (setup.value ? 'Set up CodeSynapse' : 'Create an account · CodeSynapse') })
const name = ref('')
const email = ref('')
// The root account is usually called "admin"; the field stays editable.
const username = ref(setup.value ? 'admin' : '')
const password = ref('')
const error = ref('')
const busy = ref(false)
// The API answers a sign-up the same way whether or not the email already
// has an account, so the page cannot tell either. A 409 is a taken username,
// which is checked before the email.
const SIGNUP_ERRORS = {
  403: 'Sign-up is closed on this installation. Ask an administrator to add you.',
  409: 'That username is taken. Pick another one or leave it empty.',
  429: 'Too many attempts. Wait a minute and try again.',
}
// Signing in after the sign-up works only for an account just created with
// this password; anything else reads the same for a registered email.
const SIGNIN_AFTER_SIGNUP = 'We could not sign you in with these details. If you already have an account with this email, sign in with its password.'
const errorEl = ref<HTMLElement | null>(null)
watch(error, async (e) => {
  if (!e) return
  await nextTick()
  errorEl.value?.focus()
})
async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await $fetch('/api/cg/auth/signup', { method: 'POST', body: { name: name.value, email: email.value, username: username.value.trim(), password: password.value } })
  } catch (e) {
    error.value = plainError(e, SIGNUP_ERRORS)
    busy.value = false
    return
  }
  try {
    await $fetch('/api/cg/auth/login', { method: 'POST', body: { login: email.value, password: password.value } })
    await session.load()
    await navigateTo(setup.value ? '/setup' : next.value)
  } catch (e) {
    error.value = plainError(e, { 401: SIGNIN_AFTER_SIGNUP, 429: SIGNUP_ERRORS[429] })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>{{ setup ? 'Set up CodeSynapse' : 'Create an account' }}</h1>
    <p v-if="setup" class="muted">This is the installation's root account. It administers the platform (settings, health, people and the audit log) and never works in an organisation. Next you create the first organisation and the separate account that owns it.</p>
    <p v-else class="muted">You get an organisation of your own. To join an existing one, ask its owner for an invitation link.</p>
  </div>
  <template v-if="setup || session.status.value?.signup_open">
    <form class="stack--tight" @submit.prevent="submit">
      <div class="field"><label for="name">Name</label><input id="name" v-model="name" autocomplete="name" required autofocus></div>
      <div class="field"><label for="email">Email</label><input id="email" v-model="email" type="email" autocomplete="username" required></div>
      <div class="field">
        <label for="username">Username <span class="muted">(optional)</span></label>
        <input id="username" v-model="username" type="text" autocomplete="off" autocapitalize="none" spellcheck="false" pattern="[A-Za-z0-9][A-Za-z0-9._\-]{2,31}" maxlength="32" title="3 to 32 letters, digits, dots, dashes or underscores, starting with a letter or digit" aria-describedby="username-hint">
        <span id="username-hint" class="hint">{{ setup ? 'Sign in with this or your email.' : 'A second way to sign in.' }} 3 to 32 letters, digits, dots, dashes or underscores.</span>
      </div>
      <div class="field">
        <label for="password">Password</label>
        <input id="password" v-model="password" type="password" autocomplete="new-password" minlength="12" required aria-describedby="password-hint">
        <span id="password-hint" class="hint">At least 12 characters. A sentence of a few words works well.</span>
      </div>
      <p v-if="error" ref="errorEl" class="notice notice--danger" role="alert" tabindex="-1">{{ error }}</p>
      <button type="submit" :disabled="busy">{{ busy ? 'Creating…' : setup ? 'Create the root account' : 'Create account' }}</button>
    </form>
    <p v-if="!setup" class="muted small">Already have an account? <NuxtLink :to="{ path: '/login', query: route.query.next ? { next } : {} }">Sign in</NuxtLink></p>
  </template>
  <p v-else class="notice">Sign-up is closed on this installation. Ask an administrator for an invitation, then <NuxtLink :to="{ path: '/login', query: route.query.next ? { next } : {} }">sign in</NuxtLink>.</p>
</template>
