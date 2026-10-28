<script setup lang="ts">
const session = useSession()
const org = computed(() => session.current.value?.org)
const canManage = computed(() => session.can('admin'))
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <GitTokensEditor :key="org.slug" :path="`accounts/orgs/${org.slug}/git-tokens`" :title="`Git tokens for ${org.name}`" :can-manage="canManage"
        empty-note="Your organisation can add public repositories. Add a token to add private ones." :remove-note="`become hidden from ${org.name} at their next sync.`">
        <p class="muted small">Public repositories need no token. For private ones, store a read-only token per Git host (a GitHub fine-grained token with Contents: read, a GitLab project or group token with read_repository). Tokens are encrypted, shown only as a preview, and used only to clone and sync your organisation's repositories, never the ones members add to their own space (those use each member's own token, under Profile). CodeSynapse checks at every sync that the token can still read each private repository; if it cannot, the repository is hidden from your organisation until a working token is saved.</p>
      </GitTokensEditor>
      <p v-if="!canManage" class="muted small">Only admins and owners can add or remove tokens.</p>
    </template>
  </div>
</template>
