<script setup lang="ts">
definePageMeta({ layout: 'auth' })
const session = useSession()
const setup = computed(() => !!session.status.value?.needs_setup)
useHead({ title: () => (setup.value ? 'Set up CodeSynapse' : 'Create an account · CodeSynapse') })
const name = ref('')
const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)
async function submit() {
  busy.value = true
  error.value = ''
  try {
    await $fetch('/api/cg/auth/signup', { method: 'POST', body: { name: name.value, email: email.value, password: password.value } })
    await session.load()
    await navigateTo(setup.value ? '/setup' : '/overview')
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>{{ setup ? 'Set up CodeSynapse' : 'Create an account' }}</h1>
    <p v-if="setup" class="muted">This is the installation's root account. It administers the platform (settings, health, people and the audit log) and never works in an organisation. Next you create the first organisation and the separate account that owns it.</p>
    <p v-else class="muted">You get an organisation of your own. To join an existing one, ask its owner to add your email.</p>
  </div>
  <template v-if="setup || session.status.value?.signup_open">
    <form class="stack--tight" @submit.prevent="submit">
      <div class="field"><label for="name">Name</label><input id="name" v-model="name" autocomplete="name" required autofocus></div>
      <div class="field"><label for="email">Email</label><input id="email" v-model="email" type="email" autocomplete="username" required></div>
      <div class="field">
        <label for="password">Password</label>
        <input id="password" v-model="password" type="password" autocomplete="new-password" minlength="12" required>
        <span class="hint">At least 12 characters. A sentence of a few words works well.</span>
      </div>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
      <button type="submit" :disabled="busy">{{ busy ? 'Creating…' : setup ? 'Create the root account' : 'Create account' }}</button>
    </form>
    <p v-if="!setup" class="muted small">Already have an account? <NuxtLink to="/login">Sign in</NuxtLink></p>
  </template>
  <p v-else class="notice">Sign-up is closed on this installation. Ask an administrator to add you, then <NuxtLink to="/login">sign in</NuxtLink>.</p>
</template>
