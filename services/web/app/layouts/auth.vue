<template>
  <div class="auth">
    <!-- The brand panel keeps its candy colours in both themes. -->
    <section class="auth__brand" aria-label="About CodeSynapse">
      <div class="auth__mark">
        <BrandMark :size="120" />
        <p class="auth__name">CodeSynapse</p>
        <p class="auth__tag">Agentic AI · intelligent development</p>
      </div>
      <p class="auth__lede">Your agent asks the graph, not the files.</p>
      <div class="auth__chat" aria-hidden="true">
        <span class="auth__ask">callers(ReplaceRepo)</span>
        <span class="auth__answer">
          <span class="auth__chip auth__chip--yellow">3 callers</span>
          <span class="auth__chip auth__chip--green">2 tests</span>
          <span class="auth__chip auth__chip--blue">1 doc</span>
          <span class="auth__chip auth__chip--mono">312 tok</span>
        </span>
      </div>
    </section>
    <section class="auth__form">
      <div class="auth__card">
        <slot />
        <p class="muted small foot">Open source code intelligence for your team and your AI agents · <NuxtLink to="/help">Help</NuxtLink></p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.auth { min-height: 100vh; display: flex; flex-wrap: wrap; gap: 16px; padding: 16px; background: var(--bg); }
.auth__brand {
  flex: 1 1 480px; min-width: 0; min-height: 520px; display: flex; flex-direction: column; gap: 32px;
  padding: clamp(28px, 4vw, 44px); border-radius: 36px; background: #ff5fa2; color: #1b1530;
}
.auth__mark { display: flex; flex-direction: column; gap: 16px; }
.auth__name { font: 800 clamp(48px, 7vw, 88px)/0.9 var(--font-display); letter-spacing: -0.05em; }
.auth__tag { align-self: flex-start; padding: 8px 16px; border-radius: 999px; background: #1b1530; color: #ffffff; font-weight: 700; }
.auth__lede { font: 700 clamp(24px, 2.6vw, 32px)/1.1 var(--font-display); letter-spacing: -0.03em; max-width: 520px; }
.auth__chat { margin-top: auto; display: flex; flex-direction: column; gap: 12px; max-width: 520px; }
.auth__ask { align-self: flex-start; background: #1b1530; color: #ffffff; border-radius: 18px 18px 18px 6px; padding: 12px 16px; font: 500 14px var(--font-mono); }
.auth__answer { align-self: flex-end; background: #ffffff; border-radius: 18px 18px 6px 18px; padding: 14px; display: flex; flex-wrap: wrap; gap: 8px; font-size: 14px; font-weight: 600; }
.auth__chip { padding: 6px 12px; border-radius: 999px; }
.auth__chip--yellow { background: #ffd84d; }
.auth__chip--green { background: #3ddc97; }
.auth__chip--blue { background: #5cc8ff; }
.auth__chip--mono { background: #f1ecff; font-family: var(--font-mono); font-weight: 500; }
.auth__form { flex: 1 1 420px; min-width: 0; display: flex; align-items: center; justify-content: center; padding: 32px 8px; }
.auth__card { width: min(420px, 100%); display: flex; flex-direction: column; gap: 20px; }
.foot { text-align: center; }
@media (max-width: 640px) {
  .auth__brand { min-height: 0; }
  .auth__chat { display: none; }
}
</style>
