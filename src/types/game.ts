export type GameVote = 'keep' | 'delete'
export type GameAssetStatus = 'pending' | 'kept' | 'deleted' | 'disputed' | 'error'

export interface GameSummary {
  id: string
  name: string
  owner: string
  players: string[]
  isOwner: boolean
  total: number
  /** Number of photos each player has finished (voted or decided). */
  progress: Record<string, number>
  counts: Record<GameAssetStatus, number>
  finished: boolean
  createdAt: number
  source: string
}

export interface GameAsset {
  id: string
  type: string
  originalFileName: string
  fileCreatedAt: string
  localDateTime: string
  status: GameAssetStatus
  myVote: GameVote | ''
  /** Everyone's votes; only present once the photo is decided. */
  votes?: Record<string, GameVote>
  decidedBy?: '' | 'agreement' | 'admin'
}

export interface GameReviewItem {
  id: string
  type: string
  originalFileName: string
  fileCreatedAt: string
  localDateTime: string
  status: GameAssetStatus
  votes: Record<string, GameVote>
  error?: string
}

export interface GameStateResponse {
  game: GameSummary
  next: GameAsset[]
}

export interface GameVoteResponse {
  outcome: 'waiting' | 'kept' | 'deleted' | 'disputed' | 'error'
  asset: GameAsset
  game: GameSummary
}

export interface GameCreateRequest {
  name?: string
  partner: string
  count?: number
  albumId?: string
  imagesOnly?: boolean
}
