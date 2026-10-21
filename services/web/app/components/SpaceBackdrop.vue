<script setup lang="ts">
// Astrona's calm backdrop: violet haze at the top, pumpkin haze to the
// right, a few twinkling stars and faint planet outlines. Fixed and
// pointer-transparent; animations stop under reduced motion.
const stars = [
  { x: 8, y: 14, s: 2, d: 0 }, { x: 22, y: 64, s: 1.5, d: 1.2 }, { x: 37, y: 9, s: 1, d: 2.4 },
  { x: 51, y: 38, s: 1.5, d: 0.6 }, { x: 63, y: 81, s: 2, d: 1.8 }, { x: 74, y: 22, s: 1, d: 3 },
  { x: 86, y: 58, s: 2, d: 0.9 }, { x: 93, y: 12, s: 1.5, d: 2.1 }, { x: 15, y: 88, s: 1, d: 1.5 },
  { x: 45, y: 72, s: 1, d: 2.7 },
]
</script>

<template>
  <div class="backdrop" aria-hidden="true">
    <div class="haze haze--violet" />
    <div class="haze haze--orange" />
    <span
      v-for="(st, i) in stars" :key="i" class="star"
      :style="{ left: `${st.x}%`, top: `${st.y}%`, width: `${st.s * 2}px`, height: `${st.s * 2}px`, animationDelay: `${st.d}s` }"
    />
    <svg class="planet planet--a" viewBox="0 0 120 120"><circle cx="60" cy="60" r="38" /><ellipse cx="60" cy="60" rx="58" ry="14" /></svg>
    <svg class="planet planet--b" viewBox="0 0 60 60"><circle cx="30" cy="30" r="22" /></svg>
  </div>
</template>

<style scoped>
.backdrop { position: fixed; inset: 0; z-index: 0; pointer-events: none; overflow: hidden; }
.haze { position: absolute; border-radius: 50%; filter: blur(10px); }
.haze--violet { width: 1040px; height: 620px; top: -320px; left: 50%; transform: translateX(-50%); background: radial-gradient(closest-side, rgba(122, 46, 200, 0.3), transparent); }
.haze--orange { width: 720px; height: 720px; top: 18%; right: -320px; background: radial-gradient(closest-side, rgba(255, 122, 24, 0.16), transparent); }
.star { position: absolute; border-radius: 50%; background: radial-gradient(circle, #fff, rgba(199, 124, 245, 0.6) 60%, transparent); animation: twinkle 4.5s ease-in-out infinite; }
@keyframes twinkle { 50% { opacity: 0.25; transform: scale(0.7); } }
.planet { position: absolute; fill: none; stroke-width: 1; }
.planet--a { width: 220px; right: 6%; bottom: 8%; stroke: rgba(199, 124, 245, 0.5); opacity: 0.4; animation: drift 40s ease-in-out infinite; }
.planet--b { width: 70px; left: 31%; top: 12%; stroke: rgba(255, 122, 24, 0.5); opacity: 0.35; animation: drift 30s ease-in-out infinite reverse; }
@keyframes drift { 50% { transform: translate(-14px, 10px); } }
@media (prefers-reduced-motion: reduce) { .star, .planet { animation: none; } }
</style>
