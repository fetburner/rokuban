import { classifyLiveLoadError } from '@/lib/live'
import type { LiveLoadErrorClassifier } from '@/lib/live'
import {
  fixedPlaybackTimeline,
  liveProgramPlaybackTimeline,
  liveRecordingPlaybackTimeline,
} from '@/lib/playback-timeline'
import type { LiveRecordingSource, PlaybackTimeline } from '@/lib/playback-timeline'

/** PlaybackMode は LivePlayer が切り替える再生経路を表す。 */
export type PlaybackMode = 'live' | 'recording-hls'

type LiveProgramInput = Omit<Parameters<typeof liveProgramPlaybackTimeline>[0], 'hoverSeconds'>

/**
 * PlaybackStrategyInput は LivePlayer の props のうち strategy が読む欄を、mode で判別できる形のまま持つ。
 * 欄の取り違えはコンパイル時に落ちる。
 */
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
      /**
       * ライブ画面が録画 HLS を再生しているときだけ渡す。軸は番組基準（左端が番組開始）になり、
       * 軸上の秒は録画ファイル先頭からの秒に headSeconds を足したものになる。
       * 渡さない録画詳細の軸は、録画ファイル先頭を 0 とする固定尺の軸である。
       */
      liveRecording?: LiveRecordingSource
    }

/** recordingDurationSeconds は録画の実尺（ミリ秒）を秒へ直す。不正な値は 0（シーク不可）にする。 */
export function recordingDurationSeconds(durationMs: number | undefined): number {
  return durationMs !== undefined && Number.isFinite(durationMs) && durationMs > 0 ? durationMs / 1000 : 0
}

/** PlaybackErrorMessages は再生方式ごとのエラー文言。分類は classifyLoadError が持つ。 */
export type PlaybackErrorMessages = {
  capacity: { title: string; guidance: string }
  otherTitle: string
  fatalTitle: string
  /** HLS を再生できないブラウザ。 */
  unsupported: string
}

/** PlaybackStrategy はモードごとの時間軸・エラー文言・録画シークをまとめる。 */
export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  classifyLoadError: LiveLoadErrorClassifier
  errorMessages: PlaybackErrorMessages
  /**
   * 録画 HLS の strategy だけが持つ。axisSeconds は軸上の秒で、返す値は media の秒（録画先頭からの秒）。
   * シークできないとき undefined を返す。
   */
  recording?: {
    seekTarget: (axisSeconds: number, playlistEndSeconds?: number) => number | undefined
  }
}

/** playbackStrategy はモードごとの時間軸・エラー文言・録画シークの strategy を作る。 */
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
          // 待てば直ることが読めないと「壊れている」と誤解される。直前まで別の
          // チャンネルを見ていた場合は、そのセッションの解放待ちである可能性が高い。
          // 切り替え時に離脱ヒントを送るので通常は猶予（既定 8 秒 = 3 × segment_seconds
          // + 2 秒）で解放されるが、ヒントが届かなかった場合は live.idle_timeout
          // （既定 30 秒）まで伸びる。ここでは長い方を案内する --- 短い方を書くと
          // 「待ったのに直らない」になる。
          guidance: 'チャンネルを切り替えた直後は、前のチャンネルの解放待ちの可能性があります。30 秒ほど待って再読み込みしてください。',
        },
        otherTitle: 'ライブ視聴でエラーが発生しました。',
        fatalTitle: 'ライブ再生中にエラーが発生しました',
        unsupported: 'このブラウザはライブ視聴（HLS）に対応していません',
      },
    }
  }

  const { liveRecording } = input
  const durationSeconds = recordingDurationSeconds(input.recordingDurationMs)
  const errorMessages: PlaybackErrorMessages = {
    capacity: {
      title: 'いま録画を再生できません。',
      guidance: 'しばらく待ってから再読み込みしてください。',
    },
    otherTitle: '録画 HLS の再生でエラーが発生しました。',
    fatalTitle: '録画 HLS の再生中にエラーが発生しました',
    unsupported: 'このブラウザは録画 HLS に対応していません',
  }
  if (liveRecording) {
    return {
      axis: (hoverSeconds) => liveRecordingPlaybackTimeline(liveRecording, hoverSeconds),
      classifyLoadError: classifyLiveLoadError,
      errorMessages,
      recording: {
        seekTarget: (axisSeconds, playlistEndSeconds) => {
          const { headSeconds, liveEdgeSeconds } = liveRecordingPlaybackTimeline(liveRecording, null)
          const requested = Number.isFinite(axisSeconds) ? axisSeconds : 0
          const target = Math.round(Math.max(0, Math.min(liveEdgeSeconds, requested) - headSeconds))
          return playlistEndSeconds !== undefined && Number.isFinite(playlistEndSeconds)
            ? Math.min(target, Math.max(0, playlistEndSeconds))
            : target
        },
      },
    }
  }
  return {
    axis: () => fixedPlaybackTimeline(durationSeconds),
    classifyLoadError: classifyLiveLoadError,
    errorMessages,
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
