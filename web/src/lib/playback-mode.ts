import {
  chaseLeaveURL,
  classifyLiveLoadError,
  liveLeaveURL,
  originalVODLeaveURL,
  originalVODSessionOriginSeconds,
} from '@/lib/live'
import type { LiveLoadError, LiveLoadErrorClassifier, LiveLoadErrorInput } from '@/lib/live'
import {
  chasePlaybackTimeline,
  fixedPlaybackTimeline,
  liveProgramPlaybackTimeline,
} from '@/lib/playback-timeline'
import type { PlaybackTimeline } from '@/lib/playback-timeline'
import type { RecordingTimeline } from '@/lib/recording-timeline'

/** PlaybackMode は LivePlayer が切り替える再生経路を表す。 */
export type PlaybackMode = 'live' | 'chase' | 'original-vod' | 'recording-hls'

type LiveProgramInput = Omit<Parameters<typeof liveProgramPlaybackTimeline>[0], 'hoverSeconds'>
type ChaseTimelineInput = Pick<RecordingTimeline, 'chaseHeadOffsetSeconds' | 'plannedSeconds' | 'recordedSeconds'>

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
      mode: 'chase'
      site: string
      recordingId: number
      chaseTimeline: ChaseTimelineInput
    }
  | {
      mode: 'original-vod'
      site: string
      recordingId: number
      recordingDurationMs: number
    }
  | {
      mode: 'recording-hls'
      recordingDurationMs: number
    }

/** recordingDurationSeconds は原本の実尺（ミリ秒）を秒へ直す。不正な値は 0（シーク不可）にする。 */
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

/** PlaybackStrategy はモードごとの時間軸・エラー・録画シーク・離脱先・セッション起点をまとめる。 */
export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  classifyLoadError: LiveLoadErrorClassifier
  errorMessages: PlaybackErrorMessages
  /** 録画再生の strategy だけが持つ。seekTarget はシークできないとき undefined を返す。 */
  recording?: {
    seekTarget: (axisSeconds: number, playlistEndSeconds?: number) => number | undefined
  }
  leaveURL: (offsetSeconds: number) => string | null
  originSeconds: (offsetSeconds: number) => number
}

const identityOriginSeconds = (offsetSeconds: number): number => offsetSeconds

const classifyChaseLoadError = (result: LiveLoadErrorInput): LiveLoadError => {
  // streamer の cooldown 応答だけが 502 に Retry-After を付ける。Retry-After
  // の無い 502 はプロキシ由来なので、共通分類で other にする。
  if (result.kind === 'http' && result.status === 502 && result.retryAfter) {
    return {
      kind: 'chase-input',
      title: '追っかけ再生の入力に失敗したため、一時停止しています。',
      message: result.body.trim(),
    }
  }
  return classifyLiveLoadError(result)
}

/** playbackStrategy はモードごとの時間軸の strategy を作る。 */
export function playbackStrategy(input: PlaybackStrategyInput): PlaybackStrategy {
  switch (input.mode) {
    case 'live': {
      const { site, networkId, serviceId, liveProgram } = input
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
        leaveURL: () => liveLeaveURL(site, networkId, serviceId),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'chase': {
      const { site, recordingId, chaseTimeline } = input
      return {
        axis: (hoverSeconds) => chasePlaybackTimeline(chaseTimeline, hoverSeconds),
        classifyLoadError: classifyChaseLoadError,
        errorMessages: {
          capacity: {
            title: 'いま追っかけ再生を始められません。',
            guidance: 'しばらく待ってから再読み込みしてください。',
          },
          otherTitle: '追っかけ再生でエラーが発生しました。',
          fatalTitle: '追っかけ再生中にエラーが発生しました',
          unsupported: 'このブラウザは追っかけ再生（HLS）に対応していません',
        },
        recording: {
          seekTarget: (axisSeconds) => {
            const { headSeconds, liveEdgeSeconds } = chasePlaybackTimeline(chaseTimeline, null)
            return Math.round(Math.max(0, Math.min(liveEdgeSeconds, axisSeconds) - headSeconds))
          },
        },
        leaveURL: (offsetSeconds) => chaseLeaveURL(site, recordingId, offsetSeconds),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'original-vod': {
      const { site, recordingId } = input
      const durationSeconds = recordingDurationSeconds(input.recordingDurationMs)
      return {
        axis: () => fixedPlaybackTimeline(durationSeconds),
        classifyLoadError: classifyLiveLoadError,
        errorMessages: {
          capacity: {
            title: 'いま再生を始められません（同時再生数の上限、または streamer の再起動中）。',
            guidance: 'しばらく待ってから再読み込みしてください。',
          },
          otherTitle: '原本 TS の再生でエラーが発生しました。',
          fatalTitle: '原本 TS の再生中にエラーが発生しました',
          unsupported: 'このブラウザは原本 TS の再生（HLS）に対応していません',
        },
        recording: {
          seekTarget: (axisSeconds) =>
            durationSeconds > 0 ? Math.max(0, Math.min(durationSeconds, axisSeconds)) : undefined,
        },
        leaveURL: (offsetSeconds) => originalVODLeaveURL(site, recordingId, offsetSeconds),
        originSeconds: originalVODSessionOriginSeconds,
      }
    }
    case 'recording-hls': {
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
        leaveURL: () => null,
        originSeconds: identityOriginSeconds,
      }
    }
  }
}
