<script setup lang="ts">
// The organisation's own audit log: changes, sign-ins and its agents' MCP
// requests (owners and admins; the API checks the role).
const session = useSession()
const org = computed(() => session.current.value?.org)
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <p v-else-if="!session.can('admin')" class="notice">Only owners and admins of {{ org.name }} can read its audit log.</p>
    <AuditLog v-else :key="org.slug" :base="`accounts/orgs/${org.slug}/audit`" />
  </div>
</template>
