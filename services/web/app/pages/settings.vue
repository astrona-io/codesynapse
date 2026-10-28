<script setup lang="ts">
useHead({ title: 'Settings · CodeSynapse' })
const session = useSession()
const route = useRoute()
if (route.path === '/settings') await navigateTo('/settings/profile', { replace: true })
// The sidebar link leads to /settings while a tab is open too; this page
// stays mounted then, so the redirect has to follow the route.
watch(() => route.path, (path) => { if (path === '/settings') navigateTo('/settings/profile', { replace: true }) })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Settings" title="Settings" lede="Your profile and API keys, and your organisation's members, teams, Git tokens and single sign-on." />
    <p v-if="!session.accounts.value" class="notice">This installation runs without accounts (no database configured), so there are no users, keys or organisations to manage. See <NuxtLink to="/help#accounts">Help</NuxtLink> to turn them on.</p>
    <template v-else>
      <nav class="tabs" aria-label="Settings">
        <NuxtLink to="/settings/profile">Profile</NuxtLink>
        <template v-if="!session.restricted.value">
          <NuxtLink to="/settings/keys">API keys</NuxtLink>
          <NuxtLink to="/settings/organisation">Organisation</NuxtLink>
          <NuxtLink to="/settings/teams">Teams</NuxtLink>
          <NuxtLink to="/settings/git-tokens">Git tokens</NuxtLink>
          <NuxtLink v-if="session.can('admin')" to="/settings/sso">Single sign-on</NuxtLink>
          <NuxtLink v-if="session.can('admin')" to="/settings/audit">Audit log</NuxtLink>
        </template>
      </nav>
      <NuxtPage />
    </template>
  </div>
</template>
