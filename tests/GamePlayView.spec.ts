import { mount, flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GamePlayView from '@/views/GamePlayView.vue'
import { seedAuthSession } from './helpers/seedAuth'

vi.mock('@/composables/useUmami', () => ({ trackPageView: vi.fn(), trackEvent: vi.fn() }))

const Blank = { template: '<div />' }

function asset(id: string) {
  return {
    id,
    type: 'IMAGE',
    originalFileName: `${id}.jpg`,
    fileCreatedAt: '2024-01-01T00:00:00Z',
    localDateTime: '2024-01-01T00:00:00Z',
    status: 'pending',
    myVote: '',
  }
}

const summary = {
  id: 'g1',
  name: 'Spring cleaning',
  owner: 'alice',
  players: ['alice', 'bob'],
  isOwner: false,
  total: 2,
  progress: { alice: 0, bob: 0 },
  counts: { pending: 2, kept: 0, deleted: 0, disputed: 0, error: 0 },
  finished: false,
  createdAt: 1,
  source: 'random',
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

describe('GamePlayView', () => {
  let calls: { url: string; init?: RequestInit }[]

  beforeEach(() => {
    localStorage.clear()
    seedAuthSession('t', 'bob', 'http://immich.local')
    setActivePinia(createPinia())
    calls = []
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: () => 'blob:x', revokeObjectURL: () => {} }))
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url, init })
        if (url.startsWith('/api/game/g1?')) {
          return jsonResponse({ game: summary, next: [asset('a1'), asset('a2')] })
        }
        if (url === '/api/game/g1/vote') {
          return jsonResponse({
            outcome: 'disputed',
            asset: { ...asset('a1'), status: 'disputed' },
            game: { ...summary, progress: { alice: 1, bob: 1 } },
          })
        }
        if (url.includes('/thumbnail')) return new Response(new Blob(['x']))
        return jsonResponse({}, 404)
      })
    )
  })

  async function mountView() {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/game/:id', component: GamePlayView },
        { path: '/game', component: Blank },
        { path: '/login', component: Blank },
      ],
    })
    router.push('/game/g1')
    await router.isReady()
    const wrapper = mount(GamePlayView, { global: { plugins: [router] } })
    await flushPromises()
    return wrapper
  }

  it('loads media through the game-scoped endpoint', async () => {
    await mountView()
    expect(calls.some((c) => c.url === '/api/game/g1/assets/a1/thumbnail?size=preview')).toBe(true)
    expect(calls.some((c) => c.url.startsWith('/api/assets/'))).toBe(false)
  })

  it('sends the vote, advances, and announces a dispute', async () => {
    const wrapper = await mountView()
    await wrapper.get('button[aria-label="Keep"]').trigger('click')
    await flushPromises()
    const vote = calls.find((c) => c.url === '/api/game/g1/vote')
    expect(JSON.parse(String(vote?.init?.body))).toEqual({ assetId: 'a1', vote: 'keep' })
    expect(wrapper.text()).toContain('alice will decide')
    expect(calls.some((c) => c.url === '/api/game/g1/assets/a2/thumbnail?size=preview')).toBe(true)
  })
})
