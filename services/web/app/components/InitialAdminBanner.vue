<script setup lang="ts">
// Shown to whoever is signed in as the administrator made at first start:
// its password was generated, so it should not be the account you keep.
const session = useSession()
</script>

<template>
  <p v-if="session.autoCreated.value" class="notice notice--warn banner" role="status">
    <span>
      You are signed in as the <strong>admin account made at first start</strong>. Create your own account and delete this one.
      <NuxtLink to="/replace-admin">Replace it now</NuxtLink>
    </span>
  </p>
</template>

<style scoped>
.banner { margin: 12px var(--gutter) 0; }
</style>
