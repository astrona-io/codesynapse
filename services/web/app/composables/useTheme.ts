export type ThemeChoice = 'system' | 'light' | 'dark'

// The theme lives in a cookie so the server renders the right one (no
// flash); "system" follows prefers-color-scheme in CSS.
export function useTheme() {
  const choice = useCookie<ThemeChoice>('cs_theme', { default: () => 'system', sameSite: 'lax', path: '/', maxAge: 60 * 60 * 24 * 365 })
  const attr = computed(() => (choice.value === 'light' || choice.value === 'dark' ? choice.value : undefined))
  return { choice, attr }
}
