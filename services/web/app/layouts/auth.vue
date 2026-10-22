<template>
  <div class="auth">
    <div class="auth__card">
      <p class="brand"><img src="/favicon.svg" alt="" width="28" height="28"> <span>CodeSynapse</span></p>
      <slot />
    </div>
    <p class="muted small foot">Code intelligence for your team and your AI agents · <NuxtLink to="/help">Help</NuxtLink></p>
  </div>
</template>

<style scoped>
.auth { min-height: 100vh; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 16px; padding: 24px var(--gutter); }
.auth__card { width: min(420px, 100%); display: flex; flex-direction: column; gap: 18px; background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius); box-shadow: var(--shadow); padding: 28px; }
.brand { display: flex; align-items: center; gap: 10px; font-size: 17px; font-weight: 650; }
.foot { text-align: center; }
</style>
