import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useState } from 'react'

import { LivePlayer } from '@/components/live-player'
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
      />,
    )

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(url, expect.any(Object)))
    await waitFor(() => expect(mockHls.instances[0]?.loadSource).toHaveBeenCalledWith(url))
    expect(mockHls.args[0]).toEqual([{ startPosition: 18 }])
    expect(screen.getByTestId('recording-hls-source-label')).toHaveTextContent('録画 HLS')
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

  it('offers a return-to-live action only when the caller supplies it', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(okPlaylist()))
    const props = {
      mode: 'recording-hls' as const,
      site: 'default',
      recordingId: 42,
      recordingDurationMs: 60_000,
      recordingHlsUrl: '/api/media/stores/media/recordings/42/hls/v3/hd/master.m3u8',
      recordingHlsStartSeconds: 0,
      onReturnLive: vi.fn(),
    }

    render(<LivePlayer {...props} />)

    expect(screen.getByTestId('recording-hls-return-live')).toHaveTextContent('ライブへ戻る')
  })
})
