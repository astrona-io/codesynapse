<script setup lang="ts">
definePageMeta({ layout: 'auth' })
useHead({ title: 'Sign in · CodeSynapse' })
const route = useRoute()
const session = useSession()
const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)
const next = computed(() => {
  const n = route.query.next
  // Only paths inside this app: never an absolute or protocol-relative URL.
  return typeof n === 'string' && n.startsWith('/') && !n.startsWith('//') ? n : '/overview'
})
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
    <h1>Sign in</h1>
    <p class="muted">Use the email and password of your CodeSynapse account.</p>
  </div>
  <form class="stack--tight" @submit.prevent="submit">
    <div class="field"><label for="email">Email</label><input id="email" v-model="email" type="email" autocomplete="username" required autofocus></div>
    <div class="field"><label for="password">Password</label><input id="password" v-model="password" type="password" autocomplete="current-password" required></div>
    <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    <button type="submit" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
  </form>
  <p v-if="session.status.value?.signup_open" class="muted small">No account yet? <NuxtLink to="/signup">Create one</NuxtLink></p>
  <p v-else class="muted small">No account yet? Ask an administrator of your installation to add you.</p>
</template>
