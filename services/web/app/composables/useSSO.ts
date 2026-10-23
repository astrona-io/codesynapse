// Starts a single sign-on: the API answers with the provider's authorization
// URL and sets the binder cookie (through the proxy), then the browser leaves
// for the provider. Only https URLs are followed.
export async function startSSO(path: string, next?: string): Promise<void> {
  const res = await apiPost<{ url: string }>(path, next ? { next } : {})
  const url = new URL(res.url)
  if (url.protocol !== 'https:') throw new Error('the identity provider must use https')
  await navigateTo(url.toString(), { external: true })
}

// safeNext keeps a return path inside the dashboard.
export function safeNext(n: unknown, fallback = '/overview'): string {
  return typeof n === 'string' && n.startsWith('/') && !n.startsWith('//') && !n.includes('\\') ? n : fallback
}
