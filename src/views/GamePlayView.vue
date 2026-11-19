<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'
import { useGame, gameMediaBase, toImmichAsset } from '@/composables/useGame'
import type { GameAsset, GameSummary } from '@/types/game'
import SwipeCard from '@/components/SwipeCard.vue'

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const uiStore = useUiStore()
const api = useGame()

const gameId = computed(() => String(route.params.id))
const summary = ref<GameSummary | null>(null)
const queue = ref<GameAsset[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
const busy = ref(false)
/** Asset IDs voted on in this visit, newest last, for undo. */
const history = ref<string[]>([])
const flash = ref<{ text: string; tone: 'match' | 'dispute' | 'info' } | null>(null)
let flashTimer: ReturnType<typeof setTimeout> | null = null

const me = computed(() => authStore.currentUserName)
const current = computed(() => queue.value[0] ?? null)
const currentAsset = computed(() => (current.value ? toImmichAsset(current.value) : null))
const mediaBase = computed(() => gameMediaBase(gameId.value))
const myDone = computed(() => (summary.value ? summary.value.progress[me.value] ?? 0 : 0))
const total = computed(() => summary.value?.total ?? 0)
const partners = computed(() => summary.value?.players.filter((p) => p !== me.value) ?? [])
const muted = computed(() => (uiStore.isDarkMode ? 'text-gray-400' : 'text-gray-600'))

function showFlash(text: string, tone: 'match' | 'dispute' | 'info') {
  flash.value = { text, tone }
  if (flashTimer) clearTimeout(flashTimer)
  flashTimer = setTimeout(() => (flash.value = null), 1800)
}

async function refill() {
  const state = await api.getState(gameId.value, 6)
  summary.value = state.game
  const known = new Set(queue.value.map((a) => a.id))
  for (const a of state.next) {
    if (!known.has(a.id)) queue.value.push(a)
  }
}

async function load() {
  loading.value = true
  error.value = null
  queue.value = []
  try {
    await refill()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Could not load the game'
  } finally {
    loading.value = false
  }
}

async function cast(choice: 'keep' | 'delete') {
  const asset = current.value
  if (!asset || busy.value) return
  busy.value = true
  // Advance immediately so swiping feels instant.
  queue.value.shift()
  try {
    const res = await api.vote(gameId.value, asset.id, choice)
    summary.value = res.game
    history.value.push(asset.id)
    const others = partners.value.join(' & ') || 'your partner'
    switch (res.outcome) {
      case 'kept':
        showFlash(`It's a match! You and ${others} both kept it`, 'match')
        break
      case 'deleted':
        showFlash(`It's a match! Both said goodbye, moved to trash`, 'match')
        break
      case 'disputed':
        showFlash(
          summary.value?.isOwner
            ? `You and ${others} disagree, it's in your review pile`
            : `You and ${others} disagree, ${summary.value?.owner} will decide`,
          'dispute'
        )
        break
      case 'error':
        uiStore.toast('You both agreed to delete, but Immich refused. The owner can retry from review.', 'error', 4000)
        break
    }
  } catch (e) {
    // Put the card back so the vote can be retried.
    queue.value.unshift(asset)
    uiStore.toast(e instanceof Error ? e.message : 'Vote failed', 'error')
  } finally {
    busy.value = false
  }
  if (queue.value.length < 3) {
    refill().catch(() => {})
  }
}

async function undoLast() {
  const assetId = history.value[history.value.length - 1]
  if (!assetId || busy.value) return
  busy.value = true
  try {
    const res = await api.undo(gameId.value, assetId)
    history.value.pop()
    summary.value = res.game
    queue.value = [res.asset, ...queue.value.filter((a) => a.id !== res.asset.id)]
    showFlash('Vote taken back', 'info')
  } catch (e) {
    uiStore.toast(e instanceof Error ? e.message : 'Undo failed', 'error')
  } finally {
    busy.value = false
  }
}

function handleKeydown(e: KeyboardEvent) {
  const active = document.activeElement as HTMLElement | null
  if (active && ['INPUT', 'TEXTAREA', 'SELECT'].includes(active.tagName)) return
  if (e.key === 'ArrowRight') {
    e.preventDefault()
    void cast('keep')
  } else if (e.key === 'ArrowLeft') {
    e.preventDefault()
    void cast('delete')
  } else if (e.key === 'ArrowUp' || ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z')) {
    e.preventDefault()
    void undoLast()
  }
}

onMounted(() => {
  window.addEventListener('keydown', handleKeydown)
  void load()
})
onUnmounted(() => {
  window.removeEventListener('keydown', handleKeydown)
  if (flashTimer) clearTimeout(flashTimer)
})
</script>

<template>
  <div class="h-dvh viewport-fit flex flex-col overflow-hidden" :class="uiStore.isDarkMode ? 'bg-black text-white' : 'bg-white text-black'">
    <header class="flex items-center gap-3 px-4 py-3">
      <button
        class="p-2 rounded-full transition-colors"
        :class="uiStore.isDarkMode ? 'hover:bg-gray-800' : 'hover:bg-gray-200'"
        aria-label="Back to games"
        @click="router.push('/game')"
      >
        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
        </svg>
      </button>
      <div class="min-w-0 flex-1">
        <p class="font-semibold truncate">{{ summary?.name ?? 'Swipe together' }}</p>
        <p v-if="summary" class="text-xs truncate" :class="muted">
          with {{ partners.join(', ') }}
          <template v-for="p in partners" :key="p"> · {{ p }} {{ summary.progress[p] ?? 0 }}/{{ total }}</template>
        </p>
      </div>
      <button
        v-if="summary?.isOwner"
        class="relative px-3 py-1.5 rounded-full text-xs border"
        :class="uiStore.isDarkMode ? 'border-gray-700' : 'border-gray-300'"
        @click="router.push(`/game/${gameId}/review`)"
      >
        Review
        <span
          v-if="summary.counts.disputed + summary.counts.error > 0"
          class="absolute -top-1.5 -right-1.5 min-w-5 h-5 px-1 rounded-full bg-amber-500 text-white text-[10px] font-bold flex items-center justify-center"
        >
          {{ summary.counts.disputed + summary.counts.error }}
        </span>
      </button>
    </header>

    <!-- My progress -->
    <div v-if="summary" class="px-4 pb-2 max-w-4xl mx-auto w-full flex items-center gap-2">
      <div class="flex-1 h-1.5 rounded-full overflow-hidden" :class="uiStore.isDarkMode ? 'bg-gray-800' : 'bg-gray-200'">
        <div class="h-full bg-pink-500 transition-all duration-300" :style="{ width: `${(100 * myDone) / Math.max(1, total)}%` }" />
      </div>
      <span class="text-xs tabular-nums" :class="muted">{{ myDone }} / {{ total }}</span>
    </div>

    <main class="flex-1 flex flex-col px-4 safe-area-bottom min-h-0 gap-3 overflow-hidden">
      <div v-if="loading" class="flex-1 flex items-center justify-center" :class="muted">Loading…</div>

      <div v-else-if="error" class="flex-1 flex flex-col items-center justify-center gap-4 text-center">
        <p :class="muted">{{ error }}</p>
        <button class="px-6 py-2 rounded-lg" :class="uiStore.isDarkMode ? 'bg-white text-black' : 'bg-black text-white'" @click="load">
          Try again
        </button>
      </div>

      <!-- Finished -->
      <div v-else-if="!currentAsset" class="flex-1 flex flex-col items-center justify-center gap-3 text-center px-6">
        <p class="text-4xl">🎉</p>
        <p class="text-lg font-semibold">You're all done!</p>
        <p v-if="summary" class="text-sm" :class="muted">
          Kept {{ summary.counts.kept }} · Deleted {{ summary.counts.deleted }} · Disputed {{ summary.counts.disputed }}
          <template v-if="summary.counts.pending > 0">
            <br />Waiting on {{ partners.join(' & ') }} for {{ summary.counts.pending }} photos.
          </template>
        </p>
        <div class="flex gap-2 mt-2">
          <button v-if="history.length" class="px-4 py-2 rounded-full text-sm border" :class="uiStore.isDarkMode ? 'border-gray-700' : 'border-gray-300'" @click="undoLast">
            Undo last
          </button>
          <button
            v-if="summary?.isOwner && summary.counts.disputed + summary.counts.error > 0"
            class="px-4 py-2 rounded-full text-sm font-semibold text-white bg-amber-500"
            @click="router.push(`/game/${gameId}/review`)"
          >
            Review disputes
          </button>
        </div>
      </div>

      <template v-else>
        <div class="relative flex-1 min-h-0 flex items-center justify-center p-1">
          <div class="w-full h-full max-w-4xl max-h-full">
            <SwipeCard
              :key="currentAsset.id"
              :asset="currentAsset"
              :media-base="mediaBase"
              :hide-open-in-immich="!summary?.isOwner"
              @keep="cast('keep')"
              @delete="cast('delete')"
            />
          </div>

          <!-- Match / dispute flash -->
          <transition
            enter-from-class="opacity-0 scale-90"
            enter-active-class="transition duration-200"
            leave-to-class="opacity-0"
            leave-active-class="transition duration-300"
          >
            <div
              v-if="flash"
              class="pointer-events-none absolute top-6 left-1/2 -translate-x-1/2 z-20 px-4 py-2 rounded-full text-sm font-semibold shadow-lg text-white text-center max-w-[90%]"
              :class="flash.tone === 'match' ? 'bg-pink-600' : flash.tone === 'dispute' ? 'bg-amber-500' : 'bg-gray-700'"
            >
              {{ flash.tone === 'match' ? '💞 ' : flash.tone === 'dispute' ? '⚖️ ' : '' }}{{ flash.text }}
            </div>
          </transition>
        </div>

        <div class="shrink-0 flex items-center justify-center gap-6 py-3">
          <button
            class="w-16 h-16 rounded-full flex items-center justify-center shadow-lg bg-red-500 text-white active:scale-90 transition disabled:opacity-50"
            aria-label="Delete"
            :disabled="busy"
            @click="cast('delete')"
          >
            <svg class="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
          <button
            class="w-12 h-12 rounded-full flex items-center justify-center shadow border active:scale-90 transition disabled:opacity-30"
            :class="uiStore.isDarkMode ? 'bg-gray-800 border-gray-700' : 'bg-white border-gray-300'"
            aria-label="Undo"
            :disabled="busy || history.length === 0"
            @click="undoLast"
          >
            <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 10h10a5 5 0 015 5v2M3 10l5-5M3 10l5 5" />
            </svg>
          </button>
          <button
            class="w-16 h-16 rounded-full flex items-center justify-center shadow-lg bg-green-500 text-white active:scale-90 transition disabled:opacity-50"
            aria-label="Keep"
            :disabled="busy"
            @click="cast('keep')"
          >
            <svg class="w-8 h-8" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
            </svg>
          </button>
        </div>
        <p class="hidden sm:block text-center text-xs pb-2" :class="muted">
          ← delete · → keep · ↑ or Ctrl+Z undo. Your partner can't see your vote until they've voted too.
        </p>
      </template>
    </main>
  </div>
</template>
