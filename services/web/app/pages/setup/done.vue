<script setup lang="ts">
// Shown once root has handed over; the setup session has already ended, so
// reloading this page restores nothing.
definePageMeta({ layout: 'auth' })
useHead({ title: 'Setup complete · CodeSynapse' })
</script>

<template>
  <div class="stack--tight">
    <h1>Setup complete</h1>
    <p>Setup is complete. Sign in with the standard account for organisation work.</p>
    <p class="muted">The root account still signs in for administration only.</p>
  </div>
  <NuxtLink to="/login" class="btn">Sign in</NuxtLink>
</template>
