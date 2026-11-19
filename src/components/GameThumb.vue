<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{ src: string; alt?: string }>()
const authStore = useAuthStore()
const blobUrl = ref<string | null>(null)
const failed = ref(false)

async function load() {
  failed.value = false
  if (blobUrl.value) URL.revokeObjectURL(blobUrl.value)
  blobUrl.value = null
  try {
    const res = await fetch(props.src, { headers: authStore.authHeader })
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    blobUrl.value = URL.createObjectURL(await res.blob())
  } catch {
    failed.value = true
  }
}

watch(() => props.src, load, { immediate: true })
onBeforeUnmount(() => {
  if (blobUrl.value) URL.revokeObjectURL(blobUrl.value)
})
</script>

<template>
  <div class="w-full h-full bg-gray-500/20 flex items-center justify-center overflow-hidden">
    <img v-if="blobUrl" :src="blobUrl" :alt="alt" class="w-full h-full object-cover" draggable="false" />
    <span v-else-if="failed" class="text-xs opacity-60">No preview</span>
    <span v-else class="w-6 h-6 border-2 border-t-transparent rounded-full animate-spin opacity-50" />
  </div>
</template>
