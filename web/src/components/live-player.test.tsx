import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'

import { LivePlayer } from '@/components/live-player'
import type { LiveRecordingSource } from '@/lib/playback-timeline'
import { livePlaylistURL } from '@/lib/live'

const mockHls = vi.hoisted(() => ({
  instances: [] as Array<{
    on: ReturnType<typeof vi.fn>
    loadSource: ReturnType<typeof vi.fn>
    attachMedia: ReturnType<typeof vi.fn>
    destroy: ReturnType<typeof vi.fn>
    subtitleDisplay: boolean
    audioTrack: number
    audioTracks: unknown[]
    latency: number
    mainForwardBufferInfo: { len: number } | null
  }>,
  args: [] as unknown[][],
}))

vi.mock('hls.js', () => {
  class FakeHls {
    static Events = {
      ERROR: 'hlsError',
      MANIFEST_PARSED: 'manifestParsed',
      AUDIO_TRACKS_UPDATED: 'audioTracksUpdated',
      LEVEL_LOADED: 'levelLoaded',
    }
    static isSupported = () => true
    on = vi.fn()
    loadSource = vi.fn()
    attachMedia = vi.fn()
    destroy = vi.fn()
    subtitleDisplay = true
    audioTrack = 0
    audioTracks: unknown[] = []
    latency = 0
    mainForwardBufferInfo: { len: number } | null = null

    constructor(...args: unknown[]) {
      mockHls.instances.push(this)
      mockHls.args.push(args)
    }
  }
  return { default: FakeHls }
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  mockHls.instances.length = 0
  mockHls.args.length = 0
})

function okPlaylist(body = '#EXTM3U\n#EXT-X-ENDLIST\n'): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'application/vnd.apple.mpegurl' } })
}

describe('LivePlayer playback routes', () => {
  it('live mode probes and loads the live playlist', async () => {
    const fetchMock = vi.fn().mockResolvedValue(okPlaylist())
    vi.stubGlobal('fetch', fetchMock)
    const props = { mode: 'live' as const, site: 'default', networkId: 1, serviceId: 2 }

    render(<LivePlayer {...props} />)

    const url = livePlaylistURL('default', 1, 2)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(url, expect.any(Object)))
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(url))
    expect(screen.getByTestId('live-source-label')).toHaveTextContent('ライブ')
  })

  it('recording HLS uses the API master URL and the requested recording position', async () => {
    const fetchMock = vi.fn().mockResolvedValue(okPlaylist())
    vi.stubGlobal('fetch', fetchMock)
    const url = '/api/media/stores/media/recordings/42/hls/v3/hd/master.m3u8'

    render(
      <LivePlayer
        mode="recording-hls"
        site="default"
        recordingId={42}
        recordingDurationMs={60_000}
        recordingHlsUrl={url}
        recordingHlsStartSeconds={18}
        recordingSeekTilesUrl="/api/media/recordings/42/seek-tiles?fixture=1"
      />,
    )

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(url, expect.any(Object)))
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(url))
    expect(mockHls.args[0]).toEqual([{ startPosition: 18 }])
    // 録画詳細（onReturnLive なし）は再生元の表示を出さない。
    expect(screen.queryByText('● 録画 HLS')).not.toBeInTheDocument()
    expect(screen.queryByText('● 録画から再生中')).not.toBeInTheDocument()
  })

  it('profile URL changes reload the selected recording HLS generation URL', async () => {
    const fetchMock = vi.fn().mockResolvedValue(okPlaylist())
    vi.stubGlobal('fetch', fetchMock)
    const common = {
      mode: 'recording-hls' as const,
      site: 'default',
      recordingId: 42,
      recordingDurationMs: 60_000,
      recordingHlsStartSeconds: 0,
      recordingSeekTilesUrl: '/api/media/recordings/42/seek-tiles?fixture=1',
    }
    const first = '/api/media/stores/media/recordings/42/hls/v3/hd/master.m3u8'
    const second = '/api/media/stores/media/recordings/42/hls/v3/sd/master.m3u8'
    const { rerender } = render(<LivePlayer {...common} recordingHlsUrl={first} />)
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(first))

    rerender(<LivePlayer {...common} recordingHlsUrl={second} />)

    await waitFor(() => expect(mockHls.instances[1]?.loadSource).toHaveBeenCalledWith(second))
    const requested = fetchMock.mock.calls.map(([requestedUrl]) => String(requestedUrl))
    expect(requested).toEqual([first, second])
  })

  it('keeps the playing position and intent when a 410 handoff changes HLS generation', async () => {
    const fetchMock = vi.fn().mockResolvedValue(okPlaylist())
    vi.stubGlobal('fetch', fetchMock)
    const first = '/api/media/stores/media/recordings/42/hls/v1/hd/master.m3u8'
    const second = '/api/media/stores/media/recordings/42/hls/v2/hd/master.m3u8'
    let currentTime = 0
    let paused = true
    let recovery: { positionSeconds: number | undefined; wasPlaying: boolean } | null = null
    const play = vi.fn().mockImplementation(() => {
      paused = false
      return Promise.resolve()
    })
    const Harness = () => {
      const [source, setSource] = useState(first)
      const [startSeconds, setStartSeconds] = useState(0)
      const onRecordingPlaybackError = async (positionSeconds: number | undefined, wasPlaying: boolean) => {
        recovery = { positionSeconds, wasPlaying }
        setStartSeconds(positionSeconds ?? 0)
        setSource(second)
        return true
      }
      return (
        <LivePlayer
          mode="recording-hls"
          site="default"
          recordingId={42}
          recordingDurationMs={60_000}
          recordingHlsUrl={source}
          recordingHlsStartSeconds={startSeconds}
          recordingSeekTilesUrl="/api/media/recordings/42/seek-tiles?fixture=1"
          onRecordingPlaybackError={onRecordingPlaybackError}
        />
      )
    }

    render(<Harness />)
    const video = screen.getByLabelText('録画映像') as HTMLVideoElement
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => currentTime,
      set: (value: number) => { currentTime = value },
    })
    Object.defineProperty(video, 'paused', { configurable: true, get: () => paused })
    Object.defineProperty(video, 'play', { configurable: true, value: play })
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(first))

    fireEvent(video, new Event('canplay'))
    currentTime = 9.8
    paused = false
    fireEvent.playing(video)
    mockHls.instances[0]!.destroy.mockImplementation(() => {
      currentTime = 0
      paused = true
    })
    const onError = mockHls.instances[0]!.on.mock.calls.find(([event]) => event === 'hlsError')?.[1] as
      (event: string, data: { fatal: boolean; response: { code: number } }) => void

    onError('hlsError', { fatal: true, response: { code: 410 } })

    await waitFor(() => expect(mockHls.instances[1]?.loadSource).toHaveBeenCalledWith(second))
    expect(recovery).toEqual({ positionSeconds: 9.8, wasPlaying: true })
    expect(mockHls.args[1]).toEqual([{ startPosition: 9.8 }])
    fireEvent(video, new Event('canplay'))
    await waitFor(() => expect(play).toHaveBeenCalledOnce())
    expect(paused).toBe(false)
  })

  it('coalesces concurrent 410 errors before the parent accepts a generation change', async () => {
    const fetchMock = vi.fn().mockResolvedValue(okPlaylist())
    vi.stubGlobal('fetch', fetchMock)
    const first = '/api/media/stores/media/recordings/42/hls/v1/hd/master.m3u8'
    const second = '/api/media/stores/media/recordings/42/hls/v2/hd/master.m3u8'
    let currentTime = 0
    let paused = true
    let resolveRecovery: ((handled: boolean) => void) | undefined
    const play = vi.fn().mockImplementation(() => {
      paused = false
      return Promise.resolve()
    })
    const onRecordingPlaybackError = vi.fn((_positionSeconds: number | undefined, _wasPlaying: boolean) =>
      new Promise<boolean>((resolve) => { resolveRecovery = resolve }),
    )
    const props = {
      mode: 'recording-hls' as const,
      site: 'default',
      recordingId: 42,
      recordingDurationMs: 60_000,
      recordingSeekTilesUrl: '/api/media/recordings/42/seek-tiles?fixture=1',
      onRecordingPlaybackError,
    }
    const { rerender } = render(<LivePlayer {...props} recordingHlsUrl={first} recordingHlsStartSeconds={0} />)
    const video = screen.getByLabelText('録画映像') as HTMLVideoElement
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => currentTime,
      set: (value: number) => { currentTime = value },
    })
    Object.defineProperty(video, 'paused', { configurable: true, get: () => paused })
    Object.defineProperty(video, 'play', { configurable: true, value: play })
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(first))

    fireEvent(video, new Event('canplay'))
    currentTime = 9.8
    paused = false
    fireEvent.playing(video)
    const onError = mockHls.instances[0]!.on.mock.calls.find(([event]) => event === 'hlsError')?.[1] as
      (event: string, data: { fatal: boolean; response: { code: number } }) => void
    onError('hlsError', { fatal: true, response: { code: 410 } })
    onError('hlsError', { fatal: true, response: { code: 410 } })

    expect(onRecordingPlaybackError).toHaveBeenCalledTimes(1)
    expect(onRecordingPlaybackError).toHaveBeenCalledWith(9.8, true, true)
    resolveRecovery!(true)
    rerender(<LivePlayer {...props} recordingHlsUrl={second} recordingHlsStartSeconds={9.8} />)
    await waitFor(() => expect(mockHls.instances[1]?.loadSource).toHaveBeenCalledWith(second))
    expect(mockHls.args[1]).toEqual([{ startPosition: 9.8 }])
    fireEvent(video, new Event('canplay'))
    await waitFor(() => expect(play).toHaveBeenCalledOnce())
    expect(paused).toBe(false)
  })
})

describe('LivePlayer / ライブ画面の録画軸', () => {
  // 録画ファイルの先頭は番組開始の 20 分後（head 1200 秒）。録画済みは 25 分で、先端は番組開始から 45 分。
  // 軸は番組の予定 60 分（0〜3600 秒）。
  const liveRecording: LiveRecordingSource = { recordingHeadSeconds: 1200, plannedSeconds: 3600, recordedSeconds: 1500 }
  const url = '/api/media/stores/media/recordings/42/hls/v3/hd/master.m3u8'
  const rect600 = () => ({ x: 0, y: 0, left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24, toJSON: () => ({}) })

  async function renderLiveRecording(
    extra: { startSeconds?: number; onReturnLive?: () => void } = {},
  ) {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(okPlaylist()))
    render(
      <LivePlayer
        mode="recording-hls"
        site="default"
        recordingId={42}
        recordingDurationMs={1_500_000}
        recordingHlsUrl={url}
        recordingHlsStartSeconds={extra.startSeconds ?? 0}
        recordingSeekTilesUrl="/api/media/recordings/42/seek-tiles?fixture=1"
        liveRecording={liveRecording}
        onReturnLive={extra.onReturnLive}
      />,
    )
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(url))
    const video = document.querySelector('video')!
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    Object.defineProperty(slider, 'getBoundingClientRect', { value: rect600 })
    return { video, slider }
  }

  const seekable = (video: HTMLVideoElement, end: number) => {
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => end }, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
  }
  const dragTo = (slider: HTMLElement, clientX: number) => {
    fireEvent.pointerDown(slider, { pointerId: 1, pointerType: 'mouse', clientX, clientY: 12 })
    fireEvent.pointerUp(slider, { pointerId: 1, pointerType: 'mouse', clientX, clientY: 12 })
  }

  it('録画範囲の外にホバーすると「まだ録画されていません」を添える（先頭より前・先端より後ろ）', async () => {
    const { slider } = await renderLiveRecording()

    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'mouse', clientX: 60, clientY: 12 })
    expect(screen.getByTestId('live-recording-hover-label')).toHaveTextContent('6:00 · まだ録画されていません')
    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'mouse', clientX: 300, clientY: 12 })
    expect(screen.getByTestId('live-recording-hover-label').textContent).toBe('30:00')
    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'mouse', clientX: 500, clientY: 12 })
    expect(screen.getByTestId('live-recording-hover-label')).toHaveTextContent('50:00 · まだ録画されていません')
  })

  it('軸上の位置は録画先頭の位置に media の秒を足したもの', async () => {
    const { video, slider } = await renderLiveRecording({ startSeconds: 18 })
    fireEvent.canPlay(video)
    await waitFor(() => expect(slider).toHaveAttribute('aria-valuenow', '1218'))

    Object.defineProperty(video, 'currentTime', { value: 30, writable: true, configurable: true })
    fireEvent.timeUpdate(video)
    expect(slider).toHaveAttribute('aria-valuenow', '1230')
  })

  it('軸上の秒を録画先頭からの秒へ直してシークする（先頭より前は 0、先端より後ろは先端の 1 秒前、再生リストの終端でも丸める）', async () => {
    const { video, slider } = await renderLiveRecording()
    seekable(video, 1500)

    dragTo(slider, 300) // 軸 1800 秒 = 録画先頭から 600 秒
    expect(video.currentTime).toBe(600)
    expect(slider).toHaveAttribute('aria-valuenow', '1800')

    dragTo(slider, 60) // 軸 360 秒は録画先頭より前
    expect(video.currentTime).toBe(0)
    expect(slider).toHaveAttribute('aria-valuenow', '1200')

    dragTo(slider, 590) // 軸 3540 秒は先端（2700 秒）より後ろ。先端の 1 秒前 = 録画先頭から 1499 秒
    expect(video.currentTime).toBe(1499)

    seekable(video, 200)
    dragTo(slider, 300) // 再生リストが 200 秒までしか無いときはそこまで
    expect(video.currentTime).toBe(200)
  })

  it('ライブ位置の印を押すと onReturnLive を呼ぶ。印の title にライブ位置の時刻を出す', async () => {
    const onReturnLive = vi.fn()
    await renderLiveRecording({ onReturnLive })

    const edge = screen.getByRole('button', { name: 'ライブへ戻る' })
    expect(edge).toHaveAttribute('title', 'ライブ 45:00（押すとライブへ戻る）')
    fireEvent.click(edge)
    expect(onReturnLive).toHaveBeenCalledOnce()
  })

  it('「録画から再生中」はライブ画面（onReturnLive あり）だけに出す', async () => {
    await renderLiveRecording({ onReturnLive: () => {} })
    expect(screen.getByTestId('live-recording-source-label')).toHaveTextContent('● 録画から再生中')
    expect(screen.queryByText('● 録画 HLS')).not.toBeInTheDocument()
  })
})
