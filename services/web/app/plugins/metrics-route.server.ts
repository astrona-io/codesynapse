// Tells the server's metrics (server/plugins/metrics.ts) which page a
// server render matched: the route definition, such as
// /repos/:repo()/security, never the visited path.
export default defineNuxtPlugin((nuxtApp) => {
  const event = useRequestEvent()
  if (!event) return
  const router = useRouter()
  nuxtApp.hooks.hook('app:rendered', () => {
    const page = router.currentRoute.value.matched.at(-1)?.path
    if (page) event.context.metricsPage = page
  })
})
