import {
  chaseLeaveURL,
  classifyLiveLoadError,
  liveLeaveURL,
  originalVODLeaveURL,
  originalVODSessionOriginSeconds,
} from '@/lib/live'
import type { LiveLoadError, LiveLoadErrorInput } from '@/lib/live'
import {
  chasePlaybackTimeline,
  fixedPlaybackTimeline,
  liveProgramPlaybackTimeline,
} from '@/lib/playback-timeline'
import type { PlaybackTimeline } from '@/lib/playback-timeline'
import type { RecordingTimeline } from '@/lib/recording-timeline'

/** PlaybackMode は LivePlayer が切り替える再生経路を表す。 */
export type PlaybackMode = 'live' | 'chase' | 'original-vod'

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

/** recordingDurationSeconds は原本の実尺（ミリ秒）を秒へ直す。不正な値は 0（シーク不可）にする。 */
export function recordingDurationSeconds(durationMs: number | undefined): number {
  return durationMs !== undefined && Number.isFinite(durationMs) && durationMs > 0 ? durationMs / 1000 : 0
}

/** PlaybackStrategy はモードごとの時間軸・エラー・録画シーク・離脱先・セッション起点をまとめる。 */
export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  classifyLoadError: (result: LiveLoadErrorInput) => LiveLoadError
  errorMessages: {
    capacity: { title: string; guidance: string }
    otherTitle: string
    fatalTitle: string
    mediaError: string
    chaseInputTitle?: string
  }
  /** 録画再生の strategy だけが持つ。seekTarget はシークできないとき undefined を返す。 */
  recording?: {
    seekTarget: (axisSeconds: number) => number | undefined
  }
  leaveURL: (offsetSeconds: number) => string
  originSeconds: (offsetSeconds: number) => number
}

const identityOriginSeconds = (offsetSeconds: number): number => offsetSeconds

const classifyChaseLoadError = (result: LiveLoadErrorInput): LiveLoadError => {
  // streamer の cooldown 応答だけが 502 に Retry-After を付ける。Retry-After
  // の無い 502 はプロキシ由来なので、共通分類で other にする。
  if (result.kind === 'http' && result.status === 502 && result.retryAfter) {
    return { kind: 'chase-input', message: result.body.trim() }
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
            // 直前まで別のチャンネルを見ていた場合は、そのセッションの解放待ちである可能性がある。
            // idle_timeout まで伸びる場合もあるため、切り替えから 30 秒ほど待つよう案内する。
            guidance: 'チャンネルを切り替えた直後は、前のチャンネルの解放待ちの可能性があります。30 秒ほど待って再読み込みしてください。',
          },
          otherTitle: 'ライブ視聴でエラーが発生しました。',
          fatalTitle: 'ライブ再生中にエラーが発生しました',
          mediaError: 'ライブ映像を再生できませんでした（映像データを読み込めません）',
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
          chaseInputTitle: '追っかけ再生の入力に失敗したため、一時停止しています。',
          otherTitle: '追っかけ再生でエラーが発生しました。',
          fatalTitle: '追っかけ再生中にエラーが発生しました',
          mediaError: '追っかけ映像を再生できませんでした（映像データを読み込めません）',
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
          mediaError: '録画映像を再生できませんでした（映像データを読み込めません）',
        },
        recording: {
          seekTarget: (axisSeconds) =>
            durationSeconds > 0 ? Math.max(0, Math.min(durationSeconds, axisSeconds)) : undefined,
        },
        leaveURL: (offsetSeconds) => originalVODLeaveURL(site, recordingId, offsetSeconds),
        originSeconds: originalVODSessionOriginSeconds,
      }
    }
  }
}
