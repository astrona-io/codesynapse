<script setup lang="ts">
// The identity provider sends the browser here with code and state; the API
// checks them against the binder cookie, verifies the ID token and signs in
// (or links the identity to the signed-in account).
definePageMeta({ layout: 'auth' })
useHead({ title: 'Single sign-on · CodeSynapse' })
const route = useRoute()
const session = useSession()
const error = ref('')

onMounted(async () => {
  const q = route.query
  if (typeof q.error === 'string') {
    // The provider's own explanation, as it sent it; shown as text only.
    const detail = typeof q.error_description === 'string' ? q.error_description : q.error
    error.value = `The identity provider refused the sign-in: ${detail.slice(0, 300)}`
    return
  }
  if (typeof q.code !== 'string' || typeof q.state !== 'string') {
    error.value = 'This page is opened by your identity provider after you sign in there.'
    return
  }
  try {
    const res = await apiPost<{ next: string }>('auth/sso/callback', { code: q.code, state: q.state })
    await session.load()
    await navigateTo(safeNext(res.next), { replace: true })
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
})
</script>

<template>
  <div class="stack--tight">
    <h1>Single sign-on</h1>
    <p v-if="!error" class="muted" role="status">Signing you in…</p>
    <template v-else>
      <p class="notice notice--danger" role="alert">{{ error }}</p>
      <p class="muted small"><NuxtLink :to="session.me.value ? '/settings/profile' : '/login'">{{ session.me.value ? 'Back to your profile' : 'Back to sign-in' }}</NuxtLink></p>
    </template>
  </div>
</template>
