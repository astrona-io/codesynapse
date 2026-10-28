<script setup lang="ts">
// Replace the administrator made at first start with the operator's own root
// account. The API creates the new account, deletes this one and ends this
// session in one transaction.
definePageMeta({ layout: 'auth' })
useHead({ title: 'Replace the admin account · CodeSynapse' })
const session = useSession()
const name = ref('')
const email = ref('')
const username = ref('admin')
const password = ref('')
const error = ref('')
const busy = ref(false)
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
    await apiPost('auth/replace-admin', { name: name.value, email: email.value, username: username.value.trim(), password: password.value })
    session.me.value = null
    await navigateTo('/login')
  } catch (e) {
    error.value = plainError(e, { 409: 'That email or username is taken. Choose another one.' })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>Replace the admin account</h1>
    <p class="muted">You are signed in as the account made automatically at first start. Its password was generated, so create your own root account now. When you continue, this account is deleted and you sign in again with the new one.</p>
  </div>
  <form class="stack--tight" @submit.prevent="submit">
    <div class="field"><label for="name">Name</label><input id="name" v-model="name" autocomplete="name" required autofocus></div>
    <div class="field"><label for="email">Email</label><input id="email" v-model="email" type="email" autocomplete="username" required></div>
    <div class="field">
      <label for="username">Username <span class="muted">(optional)</span></label>
      <input id="username" v-model="username" autocomplete="off" autocapitalize="none" spellcheck="false" pattern="[A-Za-z0-9][A-Za-z0-9._\-]{2,31}" maxlength="32" title="3 to 32 letters, digits, dots, dashes or underscores, starting with a letter or digit" aria-describedby="username-hint">
      <span id="username-hint" class="hint">You can keep <code>admin</code>; this account's name is free once it is deleted.</span>
    </div>
    <div class="field">
      <label for="password">Password</label>
      <input id="password" v-model="password" type="password" autocomplete="new-password" minlength="12" required aria-describedby="password-hint">
      <span id="password-hint" class="hint">At least 12 characters.</span>
    </div>
    <p v-if="error" ref="errorEl" class="notice notice--danger" role="alert" tabindex="-1">{{ error }}</p>
    <button type="submit" :disabled="busy">{{ busy ? 'Replacing…' : 'Create my account and delete the admin' }}</button>
  </form>
  <p class="muted small"><button class="link" type="button" @click="session.logout()">Sign out</button></p>
</template>
