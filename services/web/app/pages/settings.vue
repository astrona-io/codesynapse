<script setup lang="ts">
useHead({ title: 'Settings · CodeSynapse' })
const session = useSession()
const route = useRoute()
if (route.path === '/settings') await navigateTo('/settings/profile', { replace: true })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Settings" title="Settings" lede="Your profile and API keys, and your organisation's members and Git tokens." />
    <p v-if="!session.accounts.value" class="notice">This installation runs without accounts (no database configured), so there are no users, keys or organisations to manage. See <NuxtLink to="/help#accounts">Help</NuxtLink> to turn them on.</p>
    <template v-else>
      <nav class="tabs" aria-label="Settings">
        <NuxtLink to="/settings/profile">Profile</NuxtLink>
        <NuxtLink to="/settings/keys">API keys</NuxtLink>
        <NuxtLink to="/settings/organisation">Organisation</NuxtLink>
        <NuxtLink to="/settings/git-tokens">Git tokens</NuxtLink>
      </nav>
      <NuxtPage />
    </template>
  </div>
</template>
