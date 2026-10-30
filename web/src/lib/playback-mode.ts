import { classifyLiveLoadError } from '@/lib/live'
import type { LiveLoadErrorClassifier } from '@/lib/live'
import { fixedPlaybackTimeline, liveProgramPlaybackTimeline } from '@/lib/playback-timeline'
import type { PlaybackTimeline } from '@/lib/playback-timeline'

/** PlaybackMode は LivePlayer が切り替える再生経路を表す。 */
export type PlaybackMode = 'live' | 'recording-hls'

type LiveProgramInput = Omit<Parameters<typeof liveProgramPlaybackTimeline>[0], 'hoverSeconds'>

export type PlaybackStrategyInput =
  | {
      mode: 'live'
      site: string
      networkId: number
      serviceId: number
      liveProgram?: LiveProgramInput
    }
  | {
      mode: 'recording-hls'
      recordingDurationMs: number
    }

export function recordingDurationSeconds(durationMs: number | undefined): number {
  return durationMs !== undefined && Number.isFinite(durationMs) && durationMs > 0 ? durationMs / 1000 : 0
}

export type PlaybackErrorMessages = {
  capacity: { title: string; guidance: string }
  otherTitle: string
  fatalTitle: string
  unsupported: string
}

export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  classifyLoadError: LiveLoadErrorClassifier
  errorMessages: PlaybackErrorMessages
  recording?: {
    seekTarget: (axisSeconds: number, playlistEndSeconds?: number) => number | undefined
  }
}

export function playbackStrategy(input: PlaybackStrategyInput): PlaybackStrategy {
  if (input.mode === 'live') {
    const { liveProgram } = input
    return {
      axis: (hoverSeconds) => liveProgram
        ? liveProgramPlaybackTimeline({ ...liveProgram, hoverSeconds }) ?? undefined
        : undefined,
      classifyLoadError: classifyLiveLoadError,
      errorMessages: {
        capacity: {
          title: 'いま視聴できません（チューナー不足または同時視聴数の上限）。',
          guidance: 'チャンネルを切り替えた直後は、前のチャンネルの解放待ちの可能性があります。30 秒ほど待って再読み込みしてください。',
        },
        otherTitle: 'ライブ視聴でエラーが発生しました。',
        fatalTitle: 'ライブ再生中にエラーが発生しました',
        unsupported: 'このブラウザはライブ視聴（HLS）に対応していません',
      },
    }
  }

  const durationSeconds = recordingDurationSeconds(input.recordingDurationMs)
  return {
    axis: () => fixedPlaybackTimeline(durationSeconds),
    classifyLoadError: classifyLiveLoadError,
    errorMessages: {
      capacity: {
        title: 'いま録画を再生できません。',
        guidance: 'しばらく待ってから再読み込みしてください。',
      },
      otherTitle: '録画 HLS の再生でエラーが発生しました。',
      fatalTitle: '録画 HLS の再生中にエラーが発生しました',
      unsupported: 'このブラウザは録画 HLS に対応していません',
    },
    recording: {
      seekTarget: (axisSeconds, playlistEndSeconds) => {
        if (durationSeconds <= 0) return undefined
        const requested = Number.isFinite(axisSeconds) ? Math.max(0, axisSeconds) : 0
        const recordingEnd = playlistEndSeconds !== undefined && Number.isFinite(playlistEndSeconds)
          ? Math.min(durationSeconds, Math.max(0, playlistEndSeconds))
          : durationSeconds
        return Math.min(requested, recordingEnd)
      },
    },
  }
}
