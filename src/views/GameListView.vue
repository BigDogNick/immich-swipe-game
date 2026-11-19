<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'
import { useGame, GameApiError } from '@/composables/useGame'
import type { GameSummary } from '@/types/game'
import type { ImmichAlbum } from '@/types/immich'

const router = useRouter()
const authStore = useAuthStore()
const uiStore = useUiStore()
const game = useGame()

const games = ref<GameSummary[]>([])
const loading = ref(true)
const loadError = ref<string | null>(null)

const showCreate = ref(false)
const creating = ref(false)
const createError = ref<string | null>(null)
const form = ref({ name: '', partner: '', count: 100, albumId: '', imagesOnly: false })
const albums = ref<ImmichAlbum[]>([])

const me = computed(() => authStore.currentUserName)

const card = computed(() =>
  uiStore.isDarkMode ? 'bg-gray-900 border-gray-800 text-gray-100' : 'bg-white border-gray-200 text-gray-900'
)
const muted = computed(() => (uiStore.isDarkMode ? 'text-gray-400' : 'text-gray-600'))
const input = computed(() =>
  uiStore.isDarkMode
    ? 'bg-gray-800 border-gray-700 text-gray-100 placeholder-gray-500'
    : 'bg-white border-gray-300 text-gray-900 placeholder-gray-400'
)

async function load() {
  loading.value = true
  loadError.value = null
  try {
    games.value = await game.listGames()
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : 'Failed to load games'
  } finally {
    loading.value = false
  }
}

async function loadAlbums() {
  if (albums.value.length > 0) return
  try {
    const headers = { Accept: 'application/json', ...authStore.authHeader }
    const [own, shared] = await Promise.all([
      fetch('/api/albums', { headers }).then((r) => (r.ok ? r.json() : [])),
      fetch('/api/albums?isShared=true', { headers }).then((r) => (r.ok ? r.json() : [])),
    ])
    const seen = new Set<string>()
    albums.value = [...own, ...shared]
      .filter((a: ImmichAlbum) => !seen.has(a.id) && seen.add(a.id))
      .sort((a: ImmichAlbum, b: ImmichAlbum) => a.albumName.localeCompare(b.albumName))
  } catch {
    // Albums are optional; random photos still work.
  }
}

function openCreate() {
  showCreate.value = true
  createError.value = null
  void loadAlbums()
}

async function submitCreate() {
  if (!form.value.partner.trim()) {
    createError.value = "Enter your partner's Swipe username"
    return
  }
  creating.value = true
  createError.value = null
  try {
    const created = await game.createGame({
      name: form.value.name.trim() || undefined,
      partner: form.value.partner.trim(),
      count: Number(form.value.count) || 100,
      albumId: form.value.albumId || undefined,
      imagesOnly: form.value.imagesOnly,
    })
    uiStore.toast(`Game started with ${created.total} photos`, 'success')
    router.push(`/game/${created.id}`)
  } catch (e) {
    if (e instanceof GameApiError && e.code === 'api_key_required') {
      createError.value = 'Add your Immich API key in Settings first, so the game can show and trash your photos.'
    } else {
      createError.value = e instanceof Error ? e.message : 'Could not create game'
    }
  } finally {
    creating.value = false
  }
}

function partnersOf(g: GameSummary) {
  return g.players.filter((p) => p !== me.value)
}

function myProgress(g: GameSummary) {
  return g.progress[me.value] ?? 0
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
        @click="router.push('/')"
      >
        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
        </svg>
      </button>
      <h1 class="text-lg font-semibold" :class="uiStore.isDarkMode ? 'text-white' : 'text-gray-900'">
        Swipe together
      </h1>
      <div class="flex-1" />
      <button
        class="px-4 py-2 rounded-full text-sm font-semibold text-white bg-pink-600 hover:bg-pink-700 transition-colors"
        @click="openCreate"
      >
        New game
      </button>
    </header>

    <main class="flex-1 overflow-y-auto px-4 pb-8 max-w-2xl mx-auto w-full">
      <p class="text-sm mb-4" :class="muted">
        You and your partner get the same photos in a different order. Both swipe right → kept.
        Both swipe left → moved to the Immich trash. Disagree → flagged for the game owner to decide.
      </p>

      <!-- Create form -->
      <form
        v-if="showCreate"
        class="rounded-2xl border p-4 mb-6 space-y-3"
        :class="card"
        @submit.prevent="submitCreate"
      >
        <h2 class="font-semibold">Start a game from your library</h2>
        <label class="block text-sm">
          <span :class="muted">Partner's Swipe username</span>
          <input
            v-model="form.partner"
            class="mt-1 w-full rounded-lg border px-3 py-2"
            :class="input"
            autocomplete="off"
            autocapitalize="off"
            placeholder="their username"
          />
        </label>
        <label class="block text-sm">
          <span :class="muted">Game name (optional)</span>
          <input v-model="form.name" class="mt-1 w-full rounded-lg border px-3 py-2" :class="input" maxlength="80" />
        </label>
        <div class="grid grid-cols-2 gap-3">
          <label class="block text-sm">
            <span :class="muted">Photos</span>
            <input
              v-model.number="form.count"
              type="number"
              min="1"
              max="2000"
              class="mt-1 w-full rounded-lg border px-3 py-2"
              :class="input"
            />
          </label>
          <label class="block text-sm">
            <span :class="muted">From</span>
            <select v-model="form.albumId" class="mt-1 w-full rounded-lg border px-3 py-2" :class="input">
              <option value="">Random from library</option>
              <option v-for="album in albums" :key="album.id" :value="album.id">
                {{ album.albumName }}
              </option>
            </select>
          </label>
        </div>
        <label class="flex items-center gap-2 text-sm">
          <input v-model="form.imagesOnly" type="checkbox" />
          <span :class="muted">Photos only (skip videos)</span>
        </label>
        <p v-if="createError" class="text-sm text-red-500">{{ createError }}</p>
        <div class="flex justify-end gap-2 pt-1">
          <button
            type="button"
            class="px-4 py-2 rounded-full text-sm border"
            :class="uiStore.isDarkMode ? 'border-gray-700 text-gray-200' : 'border-gray-300 text-gray-700'"
            @click="showCreate = false"
          >
            Cancel
          </button>
          <button
            type="submit"
            :disabled="creating"
            class="px-4 py-2 rounded-full text-sm font-semibold text-white bg-pink-600 hover:bg-pink-700 disabled:opacity-50"
          >
            {{ creating ? 'Picking photos…' : 'Start game' }}
          </button>
        </div>
      </form>

      <div v-if="loading" class="text-center py-10" :class="muted">Loading games…</div>
      <div v-else-if="loadError" class="text-center py-10 text-red-500">{{ loadError }}</div>
      <div v-else-if="games.length === 0 && !showCreate" class="text-center py-10" :class="muted">
        No games yet. Start one and invite your partner.
      </div>

      <ul class="space-y-3">
        <li v-for="g in games" :key="g.id" class="rounded-2xl border p-4" :class="card">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <p class="font-semibold truncate">{{ g.name }}</p>
              <p class="text-xs" :class="muted">
                with {{ partnersOf(g).join(', ') }} · {{ g.isOwner ? 'you own this game' : `owned by ${g.owner}` }}
              </p>
            </div>
            <span
              v-if="g.counts.disputed + g.counts.error > 0 && g.isOwner"
              class="shrink-0 px-2 py-0.5 rounded-full text-xs font-semibold bg-amber-500 text-white"
            >
              {{ g.counts.disputed + g.counts.error }} to review
            </span>
          </div>

          <div class="mt-3 space-y-1">
            <div v-for="p in g.players" :key="p" class="flex items-center gap-2 text-xs">
              <span class="w-20 truncate" :class="muted">{{ p === me ? 'You' : p }}</span>
              <div class="flex-1 h-1.5 rounded-full overflow-hidden" :class="uiStore.isDarkMode ? 'bg-gray-800' : 'bg-gray-200'">
                <div class="h-full bg-pink-500" :style="{ width: `${(100 * (g.progress[p] ?? 0)) / Math.max(1, g.total)}%` }" />
              </div>
              <span class="tabular-nums" :class="muted">{{ g.progress[p] ?? 0 }}/{{ g.total }}</span>
            </div>
          </div>

          <p class="mt-2 text-xs" :class="muted">
            Kept {{ g.counts.kept }} · Deleted {{ g.counts.deleted }} · Disputed {{ g.counts.disputed }}
          </p>

          <div class="mt-3 flex gap-2">
            <button
              class="flex-1 px-4 py-2 rounded-full text-sm font-semibold text-white bg-pink-600 hover:bg-pink-700 disabled:opacity-50"
              :disabled="myProgress(g) >= g.total"
              @click="router.push(`/game/${g.id}`)"
            >
              {{ myProgress(g) >= g.total ? 'You’re done' : myProgress(g) > 0 ? 'Keep swiping' : 'Play' }}
            </button>
            <button
              v-if="g.isOwner"
              class="px-4 py-2 rounded-full text-sm border"
              :class="uiStore.isDarkMode ? 'border-gray-700 text-gray-200' : 'border-gray-300 text-gray-700'"
              @click="router.push(`/game/${g.id}/review`)"
            >
              Review
            </button>
          </div>
        </li>
      </ul>
    </main>
  </div>
</template>
