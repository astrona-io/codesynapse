<script setup lang="ts">
useHead({ title: 'Administration · CodeSynapse' })
const session = useSession()
const route = useRoute()
if (session.accounts.value && !session.isPlatformAdmin.value) throw createError({ statusCode: 404, statusMessage: 'Not found' })
if (route.path === '/admin') await navigateTo('/admin/overview', { replace: true })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Administration" title="This installation" lede="Organisations, people, system health and the audit log across everyone using CodeSynapse here. Only platform administrators see this." />
    <p v-if="!session.accounts.value" class="notice">This installation runs without accounts, so there is nothing to administer here.</p>
    <template v-else>
      <nav class="tabs" aria-label="Administration">
        <NuxtLink to="/admin/overview">Overview</NuxtLink>
        <NuxtLink to="/admin/orgs">Organisations</NuxtLink>
        <NuxtLink to="/admin/users">Users</NuxtLink>
        <NuxtLink to="/admin/audit">Audit log</NuxtLink>
      </nav>
      <NuxtPage />
    </template>
  </div>
</template>
