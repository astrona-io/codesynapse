<template>
  <div class="card" v-html="bio" />
</template>

<script setup lang="ts">
import { formatName } from '../util'

const props = defineProps<{ name: string; bio: string }>()
const title = computed(() => formatName(props.name))
function onClick() {
  console.log(title.value)
}
</script>
