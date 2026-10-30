<script setup lang="ts">
import type { InvitationPreview, Membership } from '~/utils/types'

// The accept page of an invitation link. Signed-out visitors are sent to
// sign in (or create an account) first and come back here. The token goes to
// the API in a POST body, never in an API path, so it stays out of logs.
useHead({ title: 'Invitation · CodeSynapse' })
const route = useRoute()
const session = useSession()
const token = computed(() => String(route.params.token ?? ''))
const GONE = 'This invitation has expired, been used or been revoked. Ask whoever sent it for a new link.'

const preview = ref<InvitationPreview | null>(null)
const error = ref('')
const busy = ref(false)
const joined = ref<Membership | null>(null)

async function load() {
  error.value = ''
  try {
    preview.value = await apiPost<InvitationPreview>('invitations/preview', { token: token.value })
  } catch (err) {
    error.value = plainError(err, { 404: GONE })
  }
}
onMounted(load)

async function accept() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    joined.value = await apiPost<Membership>('invitations/accept', { token: token.value })
    await session.load()
    await session.switchOrg(joined.value.org.slug)
  } catch (err) {
    error.value = plainError(err, { 404: GONE })
  } finally {
    busy.value = false
  }
}

// Signs out and back in as the invited account, keeping this page as the
// place to return to.
async function switchAccount() {
  await $fetch('/api/cg/auth/logout', { method: 'POST' }).catch(() => undefined)
  await session.load()
  await navigateTo({ path: '/login', query: { next: route.fullPath } })
}

const expires = computed(() => (preview.value ? new Date(preview.value.expires_at).toLocaleString() : ''))
</script>

<template>
  <div class="invite stack">
    <h1>Invitation</h1>
    <section class="panel stack--tight">
      <template v-if="joined">
        <h2>You joined {{ joined.org.name }}</h2>
        <p class="muted">Your role is <span class="badge">{{ joined.role }}</span>. Repositories follow the organisation's default and the grants you are given.</p>
        <p><NuxtLink to="/overview">Go to the overview</NuxtLink></p>
      </template>
      <template v-else-if="preview">
        <h2>Join {{ preview.org.name }}</h2>
        <p class="muted">You are invited as <span class="badge">{{ preview.role }}</span>. If the person who invited you now has a lower role, you join with theirs. The link works until {{ expires }}.</p>
        <template v-if="preview.for_you">
          <button type="button" class="accent" :disabled="busy" @click="accept">{{ busy ? 'Joining…' : `Join ${preview.org.name}` }}</button>
        </template>
        <template v-else>
          <p class="notice">You are signed in as {{ session.me.value?.user.email }}, and this invitation is for a different email address. Sign in with the account it was sent to.</p>
          <button type="button" class="secondary" @click="switchAccount">Sign in with another account</button>
        </template>
      </template>
      <p v-else-if="!error" class="muted small" role="status">Reading the invitation…</p>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    </section>
  </div>
</template>

<style scoped>
.invite { max-width: 560px; }
</style>
