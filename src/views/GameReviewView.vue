<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useUiStore } from '@/stores/ui'
import { useGame, gameMediaBase } from '@/composables/useGame'
import type { GameReviewItem, GameSummary } from '@/types/game'
import GameThumb from '@/components/GameThumb.vue'

const route = useRoute()
const router = useRouter()
const uiStore = useUiStore()
const api = useGame()

const gameId = computed(() => String(route.params.id))
const summary = ref<GameSummary | null>(null)
const items = ref<GameReviewItem[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
const working = ref<Record<string, boolean>>({})
const confirmEnd = ref(false)

const card = computed(() =>
  uiStore.isDarkMode ? 'bg-gray-900 border-gray-800 text-gray-100' : 'bg-white border-gray-200 text-gray-900'
)
const muted = computed(() => (uiStore.isDarkMode ? 'text-gray-400' : 'text-gray-600'))

function thumb(id: string) {
  return `${gameMediaBase(gameId.value)}/${encodeURIComponent(id)}/thumbnail?size=preview`
}

async function load() {
  loading.value = true
  error.value = null
  try {
    const res = await api.getReview(gameId.value)
    summary.value = res.game
    items.value = res.items
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Could not load review'
  } finally {
    loading.value = false
  }
}

async function decide(item: GameReviewItem, decision: 'keep' | 'delete') {
  working.value[item.id] = true
  try {
    const res = await api.resolve(gameId.value, item.id, decision)
    summary.value = res.game
    items.value = items.value.filter((i) => i.id !== item.id)
    uiStore.toast(decision === 'keep' ? 'Kept' : 'Moved to Immich trash', 'success', 1500)
  } catch (e) {
    uiStore.toast(e instanceof Error ? e.message : 'Could not save decision', 'error')
    void load()
  } finally {
    working.value[item.id] = false
  }
}

async function endGame() {
  try {
    await api.endGame(gameId.value)
    uiStore.toast('Game ended', 'info')
    router.push('/game')
  } catch (e) {
    uiStore.toast(e instanceof Error ? e.message : 'Could not end game', 'error')
  }
}

function voteLine(item: GameReviewItem) {
  return Object.entries(item.votes)
    .map(([who, v]) => `${who}: ${v === 'keep' ? 'keep' : 'delete'}`)
    .join(' · ')
}

onMounted(load)
</script>

<template>
  <div class="h-dvh viewport-fit flex flex-col overflow-hidden" :class="uiStore.isDarkMode ? 'bg-black' : 'bg-gray-100'">
    <header class="flex items-center gap-3 px-4 py-3">
      <button
        class="p-2 rounded-full transition-colors"
        :class="uiStore.isDarkMode ? 'hover:bg-gray-800 text-white' : 'hover:bg-gray-200 text-gray-700'"
        aria-label="Back"
        @click="router.push(`/game/${gameId}`)"
      >
        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
        </svg>
      </button>
      <div class="min-w-0 flex-1" :class="uiStore.isDarkMode ? 'text-white' : 'text-gray-900'">
        <p class="font-semibold truncate">Review disputes</p>
        <p v-if="summary" class="text-xs truncate" :class="muted">{{ summary.name }}</p>
      </div>
    </header>

    <main class="flex-1 overflow-y-auto px-4 pb-8 max-w-4xl mx-auto w-full">
      <div v-if="loading" class="text-center py-10" :class="muted">Loading…</div>
      <div v-else-if="error" class="text-center py-10 text-red-500">{{ error }}</div>
      <template v-else>
        <p v-if="summary" class="text-sm mb-4" :class="muted">
          Kept {{ summary.counts.kept }} · Deleted {{ summary.counts.deleted }} · Still being swiped {{ summary.counts.pending }}
        </p>

        <div v-if="items.length === 0" class="text-center py-10" :class="muted">
          Nothing to settle. 🕊️
        </div>

        <div class="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <div v-for="item in items" :key="item.id" class="rounded-2xl border overflow-hidden flex flex-col" :class="card">
            <div class="aspect-square">
              <GameThumb :src="thumb(item.id)" :alt="item.originalFileName" />
            </div>
            <div class="p-2 space-y-1 text-xs">
              <p class="truncate">{{ item.originalFileName }}</p>
              <p :class="muted">{{ voteLine(item) }}</p>
              <p v-if="item.error" class="text-red-500">{{ item.error }}</p>
            </div>
            <div class="grid grid-cols-2 gap-1 p-2 pt-0 mt-auto">
              <button
                class="py-1.5 rounded-lg text-xs font-semibold text-white bg-red-500 hover:bg-red-600 disabled:opacity-50"
                :disabled="working[item.id]"
                @click="decide(item, 'delete')"
              >
                {{ item.status === 'error' ? 'Retry delete' : 'Delete' }}
              </button>
              <button
                class="py-1.5 rounded-lg text-xs font-semibold text-white bg-green-500 hover:bg-green-600 disabled:opacity-50"
                :disabled="working[item.id]"
                @click="decide(item, 'keep')"
              >
                Keep
              </button>
            </div>
          </div>
        </div>

        <div class="mt-10 text-center">
          <button v-if="!confirmEnd" class="text-xs underline" :class="muted" @click="confirmEnd = true">
            End this game
          </button>
          <div v-else class="inline-flex items-center gap-2 text-xs">
            <span :class="muted">Remove the game for everyone? Photos already in trash stay there.</span>
            <button class="px-3 py-1 rounded-full bg-red-600 text-white" @click="endGame">End game</button>
            <button class="px-3 py-1 rounded-full border" :class="uiStore.isDarkMode ? 'border-gray-700 text-gray-200' : 'border-gray-300'" @click="confirmEnd = false">Cancel</button>
          </div>
        </div>
      </template>
    </main>
  </div>
</template>
