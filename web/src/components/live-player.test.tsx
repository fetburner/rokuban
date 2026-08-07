import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LivePlayer } from '@/components/live-player'

/**
 * jsdom は `HTMLMediaElement.canPlayType` を実装していない（常に `''`）ため、
 * ネイティブ HLS 対応（Safari）を明示的に模擬するにはテスト側から差し替える必要がある。
 * 差し替えのタイミングは「effect が `probeLivePlaylist` の `await` に入った後・
 * 継続処理で読むより前」でなければならない --- コンポーネントは probe 成功後に
 * `canPlayType` を読むので、fetch を deferred にして制御する。
 */
function deferredFetch() {
  let resolve!: (response: Response) => void
  const promise = new Promise<Response>((r) => {
    resolve = r
  })
  vi.stubGlobal('fetch', vi.fn(() => promise))
  return { resolve }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('LivePlayer の状態遷移', () => {
  it('読み込み中は "読み込み中…" を出し、video は invisible', async () => {
    deferredFetch()
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(screen.getByText('読み込み中…')).toBeInTheDocument()
    const video = document.querySelector('video')!
    expect(video.className).toContain('invisible')
  })

  it('fetch が reject すると streamer 不在の文言を出す（destructive にしない）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    const message = await screen.findByText(/自宅サーバーが起動しているか/)
    expect(message).toBeInTheDocument()
    expect(message.className).not.toContain('text-destructive')
    // 読み込み中の表示は消えている
    expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument()
  })

  it('503 は本文をそのまま見せる（同時セッション上限 / チューナー枯渇）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(new Response('too many concurrent live sessions on this process', { status: 503 })),
      ),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(
      await screen.findByText('too many concurrent live sessions on this process'),
    ).toBeInTheDocument()
    expect(screen.getByText(/チューナー不足または同時視聴数の上限/)).toBeInTheDocument()
  })

  it('想定外のステータスも本文をそのまま見せる', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('internal error', { status: 500 }))),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(await screen.findByText('internal error')).toBeInTheDocument()
    expect(screen.getByText('ライブ視聴でエラーが発生しました。')).toBeInTheDocument()
  })

  it('再読み込みボタンで probe をやり直す', async () => {
    const user = userEvent.setup()
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response('live stream unavailable', { status: 503 }))
      .mockResolvedValueOnce(new Response('live stream unavailable', { status: 503 }))
    vi.stubGlobal('fetch', fetchMock)

    render(<LivePlayer site="default" serviceId={1024} />)
    await screen.findByText('live stream unavailable')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await user.click(screen.getByRole('button', { name: '再読み込み' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })

  it('ネイティブ HLS 対応（Safari 相当）なら video.src に直接プレイリスト URL を渡す', async () => {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" serviceId={1024} />)

    const video = document.querySelector('video')!
    // probe が解決する前に canPlayType を差し替える（Safari 相当）
    vi.spyOn(video, 'canPlayType').mockReturnValue('probably')

    resolve(new Response('', { status: 200 }))

    await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    expect(video.src).toContain('/api/sites/default/services/1024/live/playlist.m3u8')
    expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
  })

  it('serviceId が変わると新しい URL で probe をやり直す', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    const { rerender } = render(<LivePlayer site="default" serviceId={1024} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0]?.[0]).toContain('/services/1024/')

    rerender(<LivePlayer site="default" serviceId={2048} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(fetchMock.mock.calls[1]?.[0]).toContain('/services/2048/')
  })
})
