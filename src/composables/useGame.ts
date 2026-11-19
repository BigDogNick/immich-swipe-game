import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'
import router from '@/router'
import type { ImmichAsset } from '@/types/immich'
import type {
  GameAsset,
  GameCreateRequest,
  GameReviewItem,
  GameStateResponse,
  GameSummary,
  GameVoteResponse,
} from '@/types/game'

export class GameApiError extends Error {
  constructor(message: string, public status: number, public code?: string) {
    super(message)
  }
}

/**
 * Thin client for the backend's couples-game endpoints (`/api/game/...`).
 * These are served by the Swipe backend itself and never reach Immich.
 */
export function useGame() {
  const authStore = useAuthStore()
  const uiStore = useUiStore()

  async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
      ...authStore.authHeader,
      ...(options.headers as Record<string, string> | undefined),
    }
    if (typeof options.body === 'string') headers['Content-Type'] = 'application/json'
    const response = await fetch(`/api/game${path}`, { ...options, headers })
    if (response.status === 401) {
      const remaining = authStore.removeActiveSession()
      if (!remaining) {
        authStore.autoLoginBlocked = true
        router.push('/login')
      } else {
        uiStore.toast('Session expired', 'info')
      }
      throw new GameApiError('Session expired', 401)
    }
    if (response.status === 204) return undefined as T
    const data = await response.json().catch(() => ({}))
    if (!response.ok) {
      throw new GameApiError(data.error || `HTTP ${response.status}`, response.status, data.code)
    }
    return data as T
  }

  const listGames = () =>
    request<{ games: GameSummary[] }>('').then((r) => r.games)

  const createGame = (body: GameCreateRequest) =>
    request<GameSummary>('', { method: 'POST', body: JSON.stringify(body) })

  const getState = (id: string, limit = 5) =>
    request<GameStateResponse>(`/${encodeURIComponent(id)}?limit=${limit}`)

  const vote = (id: string, assetId: string, vote: 'keep' | 'delete') =>
    request<GameVoteResponse>(`/${encodeURIComponent(id)}/vote`, {
      method: 'POST',
      body: JSON.stringify({ assetId, vote }),
    })

  const undo = (id: string, assetId: string) =>
    request<{ asset: GameAsset; game: GameSummary }>(`/${encodeURIComponent(id)}/undo`, {
      method: 'POST',
      body: JSON.stringify({ assetId }),
    })

  const getReview = (id: string) =>
    request<{ game: GameSummary; items: GameReviewItem[] }>(`/${encodeURIComponent(id)}/review`)

  const resolve = (id: string, assetId: string, decision: 'keep' | 'delete') =>
    request<{ status: string; game: GameSummary }>(`/${encodeURIComponent(id)}/resolve`, {
      method: 'POST',
      body: JSON.stringify({ assetId, decision }),
    })

  const endGame = (id: string) =>
    request<void>(`/${encodeURIComponent(id)}`, { method: 'DELETE' })

  return { listGames, createGame, getState, vote, undo, getReview, resolve, endGame }
}

/** Base path for a game's media endpoints, as used by SwipeCard's `mediaBase`. */
export function gameMediaBase(gameId: string): string {
  return `/api/game/${encodeURIComponent(gameId)}/assets`
}

/** Adapt a game asset to the ImmichAsset shape SwipeCard expects. */
export function toImmichAsset(a: Pick<GameAsset, 'id' | 'type' | 'originalFileName' | 'fileCreatedAt' | 'localDateTime'>): ImmichAsset {
  return {
    id: a.id,
    ownerId: '',
    type: (a.type as ImmichAsset['type']) || 'IMAGE',
    originalPath: '',
    originalFileName: a.originalFileName || '',
    fileCreatedAt: a.fileCreatedAt || '',
    fileModifiedAt: a.fileCreatedAt || '',
    localDateTime: a.localDateTime || a.fileCreatedAt || '',
    updatedAt: '',
    isFavorite: false,
    isArchived: false,
    isTrashed: false,
    isOffline: false,
    hasMetadata: false,
  }
}
