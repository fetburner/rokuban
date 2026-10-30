import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type MutableRefObject,
  type RefObject,
} from 'react'

import type { ChapterSpan, RecordingChaptersSource } from '@/api/generated'
import { Button } from '@/components/ui/button'
import {
  RecordingChapterEditor,
  type ChapterEditorCommands,
  type ChapterEditorStatus,
} from '@/components/recording-chapter-editor'
import {
  RecordingPlaybackControls,
  type PlaybackAudioOption,
} from '@/components/recording-playback-controls'
import { Pause, Play, SkipBack } from 'lucide-react'
import {
  autoSkipSeekSeconds,
  chapterBoundaryMsToSeekSeconds,
  chapterJumpTarget,
  loadChapterSkip,
  PLAY_AROUND_SECONDS,
  saveChapterSkip,
  skipTarget,
} from '@/lib/chapters'
import type {
  LiveAudioChoice,
  LiveDiagnostics,
  LiveLoadError,
  StallHandling,
  StallTracker,
} from '@/lib/live'
import {
  chasePlaylistURL,
  createStallTracker,
  liveAudioTrackIndex,
  livePlaylistURL,
  originalVODPlaylistURL,
  observeStall,
  probeLivePlaylist,
  readSubtitleVisibility,
  sendPlaybackLeaveHint,
} from '@/lib/live'
import {
  applyPlaybackRate,
  loadChapterEditPlaybackRate,
  clearLegacyPlaybackPositions,
  effectivePlaybackRate,
  loadPlaybackRate,
  playbackPositionWrite,
  persistPlaybackPosition,
  saveChapterEditPlaybackRate,
  savePlaybackRate,
} from '@/lib/playback-position'
import { formatPlaybackTime } from '@/lib/format'
import { useDisplayedFrameSeconds } from '@/lib/use-displayed-frame'
import { useOffsetSession } from '@/lib/use-offset-session'
import type { OffsetSessionStart } from '@/lib/use-offset-session'
import { usePlayerFrame } from '@/lib/use-player-frame'
import { usePlaybackLeave } from '@/lib/use-playback-leave'
import { cn } from '@/lib/utils'
import type { RecordingTimeline } from '@/lib/recording-timeline'
import { playbackStrategy, recordingDurationSeconds } from '@/lib/playback-mode'
import type { PlaybackErrorMessages, PlaybackMode } from '@/lib/playback-mode'

const hlsSubtitleTracks = (video: HTMLVideoElement) =>
  Array.from(video.textTracks).filter((track) => track.kind === 'subtitles')

function mediaSeekableEndSeconds(video: HTMLVideoElement): number | undefined {
  try {
    const seekable = video.seekable
    if (seekable.length > 0) {
      const end = seekable.end(seekable.length - 1)
      if (Number.isFinite(end)) return end
    }
  } catch {
    // Some media implementations throw while the source is being replaced.
  }
  return undefined
}

const playbackAudioOptions: readonly PlaybackAudioOption[] = [
  { value: undefined, label: '標準' },
  { value: 'main', label: '主音声' },
  { value: 'sub', label: '副音声' },
]

type PlaybackAxisState = {
  identity: string
  positionSeconds: number
  previewSeconds: number | null
  hoverSeconds: number | null
}

function patchPlaybackAxisState(
  current: PlaybackAxisState,
  identity: string,
  initialPositionSeconds: number,
  patch: Partial<Omit<PlaybackAxisState, 'identity'>>,
): PlaybackAxisState {
  const base = current.identity === identity
    ? current
    : {
        identity,
        positionSeconds: initialPositionSeconds,
        previewSeconds: null,
        hoverSeconds: null,
      }
  return { ...base, ...patch, identity }
}

function createPlaybackAxisIdentity(
  mode: PlaybackMode,
  site: string,
  networkId: number | undefined,
  serviceId: number | undefined,
  recordingId: number | undefined,
  offsetSessionKey: string,
): string {
  return JSON.stringify([mode, site, networkId, serviceId, recordingId, offsetSessionKey])
}

/** HlsLike は hls.js の型を静的 import せずに使うための最小限の形。 */
type HlsLike = {
  destroy(): void
  loadSource(url: string): void
  attachMedia(media: HTMLMediaElement): void
  subtitleDisplay: boolean
  /** 選択中の音声トラック（`audioTracks` 内の位置）。代入で切り替わる。 */
  audioTrack: number
  /** 選択中の variant の音声グループに属するトラック（master の順）。 */
  audioTracks: readonly unknown[]
  on(
    event: string,
    callback: (event: string, data: {
      fatal: boolean
      details?: { live: boolean }
      response?: { code?: number }
    }) => void,
  ): void
  /**
   * hls.latency（秒）。`LatencyController.get latency()` の実装
   * （`node_modules/hls.js` 1.7.1）は `this._latency || 0` を返すため、
   * ライブ同期点が決まる前は `NaN` ではなく **`0`** になる（issue #476
   * レビュー指摘。当初の実装は `NaN` を前提にしており実ブラウザで
   * 「放送から約0秒」という偽の測定値を出していた）。
   */
  latency: number
  /** hls.mainForwardBufferInfo（アタッチ直後・バッファが無い間は `null`）。 */
  mainForwardBufferInfo: { len: number } | null
}

/**
 * readHlsDiagnostics は hls.js 経路の計器値を読む（issue #476）。
 *
 * **`hls.latency` は同期点が決まる前も `0` を返す（`NaN` にはならない）。**
 * `LatencyController.get latency()` が `this._latency || 0` を実装しており、
 * `_latency` は同期点が決まるまで `null` のまま（`node_modules/hls.js` 1.7.1
 * を実際に読んで確認済み。レビュー指摘）。`0` は「まだ計測できていない」と
 * 「実際に遅延ゼロ」を区別できないため、`0` 以下は欠損として扱う ---
 * このアプリの構成（2 秒セグメント、既定の `hold_back`）で実際の遅延が
 * 1 秒未満になることは実質無い。
 */
function readHlsDiagnostics(hls: HlsLike): LiveDiagnostics {
  return {
    latencySec: hls.latency > 0 ? hls.latency : null,
    bufferSec:
      hls.mainForwardBufferInfo && Number.isFinite(hls.mainForwardBufferInfo.len)
        ? hls.mainForwardBufferInfo.len
        : null,
  }
}

/** applyHlsAudioTrack は音声トラックを index に揃える（issue #870）。 */
function applyHlsAudioTrack(hls: HlsLike, index: number): void {
  if (index < hls.audioTracks.length && hls.audioTrack !== index) hls.audioTrack = index
}


type LivePlayerCommonProps = {
  site: string
  /**
   * chase playlist / live playlist の画質（`live.profiles` の名前）。省略時は
   * streamer の先頭プロファイル（既定）。
   *
   * 再開位置とは独立している。画質切替でも同じ再生位置を引き継ぐ（issue #874）。
   *
   * **切替はセッションを作り直さない。** 1 サービス / 1 録画 = ffmpeg 1 本が全
   * プロファイルを同時に出力しているので、替わるのは同じセッションのプレイリストの
   * URL だけである。再生位置は `lastChasePositionRef` で持ち越す（下記）。
   */
  profile?: string
  /**
   * 音声（二重音声の主 / 副。issue #870）。省略時は標準トラック。
   *
   * **`profile` と違って HLS playlist の URL を変えない。** streamer が live / chase /
   * original VOD に 3 本の音声レンディションを出すので、切替はプレイヤーが取る
   * トラックを替えるだけで、playlist の取り直しもセッションの作り直しも起きない。
   */
  audio?: LiveAudioChoice
  /** 各モードの画質メニュー。1 件以下ならセレクタを隠す。 */
  availableProfiles?: readonly { name: string; height?: number; label?: string }[]
  onProfileChange?: (profile: string) => void
  onAudioChange?: (audio: LiveAudioChoice | undefined) => void
  /**
   * 最初の読み込みが終わったら再生を始める（ポスターの ▶ や `#chase` で開いた、再生元を替えて続きを
   * 見る、など利用者が再生を求めた後に作られるプレイヤー用）。最初のセッションにだけ効き、
   * その後の画質切替では利用者が止めた状態を保つ。
   */
  autoPlay?: boolean
  /** 詳細ページで再生元の種類が変わっても残る、共有の全画面コンテナ。 */
  fullscreenContainerRef?: RefObject<HTMLElement | null>
  className?: string
  /**
   * onDiagnostics は遅延・バッファの計器（issue #476）の値を 1 秒ごとに
   * 呼び出し側へ渡す。表示位置は ON AIR バッジと同じ情報欄（`pages/live.tsx`）
   * なので、値そのものは `LivePlayer` の内部に閉じず親へ渡す。
   */
  onDiagnostics?: (diagnostics: LiveDiagnostics | null) => void
  /**
   * onStalled は「このプロファイルでは映像が `liveStallTimeoutMs` 進まなかった」を
   * 呼び出し側へ伝える（issue #871 の自動降格）。**`true` を返したら「呼び出し側が
   * 引き取った」**と見なし、`LivePlayer` はエラー表示に落ちない。
   *
   * **下げるかどうかを決めるのは呼び出し側である**（`pages/live.tsx`）。画質の
   * 一覧と明示選択（`?profile=`）を持っているのがあちらで、こちらは
   * 「進んでいない」という観測しか持たない --- ここでプロファイルを選ぶと、
   * `LivePlayer` が URL の意味（明示選択かどうか）を知ることになる。
   *
   * `onDiagnostics` と同じく ref 越しに読む（依存配列に入れると、呼び出し側が
   * 毎レンダー新しい関数を渡したときにプレイリストの再取得が起きる）。
   *
   * **`live.captions: true` のデプロイでは呼ばない**（probe が読んだ本文が
   * 全プロファイルを束ねた master かどうかで判断する。`lib/live.ts` の `bundlesProfiles`）。
   * そのとき `?profile=` は何も選ばないので、呼んでも嘘の「下げました」になる。
   *
   * **`'wait'` は「今は判断できない」**（呼び出し側の画質一覧がまだ届いていない）。
   * この再生では下げない、という `false` と同じ扱いにすると、一覧が遅れて届いた
   * 場合に**その再生では二度と試さない**（hls.js 経路の観測は一度判定すると
   * 止まる）ので、`'wait'` のときだけ観測を初期化して次の刻みで再判定する。
   */
  onStalled?: () => StallHandling
}

type RecordingPlaybackProps = {
  /** recordings.id。追っかけまたは原本 VOD の録画。 */
  recordingId: number
  /** true のあいだ、チャプター編集用の再生速度で再生する。追っかけでは編集 UI を開かない。 */
  chapterEditing?: boolean
  /** 録画 HLS セッションの開始意図。offset の変化後は LivePlayer 内で張り直す。 */
  offsetSessionStart?: OffsetSessionStart
  /** 録画先頭からの現在位置（秒）。再生元を選び直すとき親が持ち越す。 */
  onRecordingPositionChange?: (seconds: number) => void
  /**
   * 今の再生元の範囲の外へのシークを親に委ねる。true なら親が再生元を替えた（このプレイヤーは
   * 何もしない）。false なら、このプレイヤーが同じ再生元のまま張り直す。
   */
  onSourceRangeExit?: (recordingPositionSeconds: number, wasPlaying: boolean) => boolean
  /**
   * 再生元がエラーを返した。位置は一度も再生していないセッションでは undefined（0 秒を
   * 「明示の位置」として渡さない）。true なら親が再生元を選び直した（エラー表示に落ちない）。
   */
  onRecordingPlaybackError?: (
    recordingPositionSeconds: number | undefined,
    wasPlaying: boolean,
    allowSameSourceRetry?: boolean,
  ) => boolean | Promise<boolean>
}

type OriginalVODProps = {
  /**
   * 原本ファイルの実尺（呼び出し側で求めたファイル先頭から `endedAt` まで）。予定尺 `durationMs` ではない。
   * 固定タイムラインと視聴済み閾値に使う。映像より長いときの末尾は 416 の丸めが受ける。
   * `endedAt` が無い録画だけは呼び出し側が予定尺で代用する（0 だとシークバーが効かない）。
   */
  recordingDurationMs: number
  /** 原本の時間軸で保存されたチャプター（目盛り・一覧・自動スキップ・編集）。 */
  chapters?: ChapterSpan[]
  /** 原本 VOD の確認状態と編集操作。 */
  chapterSource?: RecordingChaptersSource
  chapterVersion?: string
  chapterDetectionPending?: boolean
  onEnterChapterEditing?: () => void
  chapterEditorCommandsRef?: MutableRefObject<ChapterEditorCommands | null>
  onChapterEditorStatusChange?: (status: ChapterEditorStatus) => void
  onSaveChapters?: (spans: ChapterSpan[], version: string) => Promise<unknown>
  onResetChapters?: () => Promise<unknown> | void
  chapterSavePending?: boolean
  watched?: boolean
  watchedPending?: boolean
  onPutWatched?: () => void
  onDeleteWatched?: () => void
  onWatched?: () => void
}

type RecordingHLSProps = {
  /** recordings.hlsPlayback の選択中プロファイルが返した master playlist URL。 */
  recordingHlsUrl: string
  /** 初回再生位置。profile 切替後は現在の currentTime を優先する。 */
  recordingHlsStartSeconds: number
  /** 録画中は視聴済み操作を出さない。 */
  showWatched?: boolean
}

type LivePlayerProps =
  | (LivePlayerCommonProps & {
      mode: 'live'
      /** SI の networkId。mirakc 合成 service id の組み立てに使う（issue #208）。 */
      networkId: number
      /** SI の serviceId。パスに載る前に networkId と合成する（issue #208）。 */
      serviceId: number
      liveProgram?: {
        startAt: string
        endAt: string
        nowMs: number
        recordingId?: number
        recordingStartedAt?: string
      }
      onLiveProgramSeek?: (programSeconds: number) => void
      onStartOver?: () => void
      onReturnLive?: never
      liveDiagnostics?: string
      liveNotice?: string
      recordingId?: never
      recordingHlsUrl?: never
      recordingHlsStartSeconds?: never
      recordingDurationMs?: never
      chaseTimeline?: never
      offsetSessionStart?: never
      onRecordingPositionChange?: never
      onSourceRangeExit?: never
      onRecordingPlaybackError?: never
      chapterEditing?: never
      chapters?: never
      chapterSource?: never
      chapterVersion?: never
      chapterDetectionPending?: never
      onEnterChapterEditing?: never
      chapterEditorCommandsRef?: never
      onChapterEditorStatusChange?: never
      onSaveChapters?: never
      onResetChapters?: never
      chapterSavePending?: never
      watched?: never
      watchedPending?: never
      showWatched?: never
      onPutWatched?: never
      onDeleteWatched?: never
      onWatched?: never
    })
  | (LivePlayerCommonProps & RecordingPlaybackProps & {
      mode: 'chase'
      networkId?: never
      serviceId?: never
      /** 番組開始と録画ファイル先頭を基準に追っかけバーを描く時間情報。 */
      chaseTimeline: Pick<RecordingTimeline, 'chaseHeadOffsetSeconds' | 'plannedSeconds' | 'recordedSeconds'>
      recordingHlsUrl?: never
      recordingHlsStartSeconds?: never
      showWatched?: never
      recordingDurationMs?: never
      liveProgram?: never
      onLiveProgramSeek?: never
      onStartOver?: never
      /**
       * 追っかけをやめてライブ配信へ戻す操作。渡さないと、ライブ端の操作は追っかけのまま
       * 録画済みの端へシークする。
       */
      onReturnLive?: () => void
      liveDiagnostics?: never
      liveNotice?: never
      chapters?: never
      chapterSource?: never
      chapterVersion?: never
      chapterDetectionPending?: never
      onEnterChapterEditing?: never
      chapterEditorCommandsRef?: never
      onChapterEditorStatusChange?: never
      onSaveChapters?: never
      onResetChapters?: never
      chapterSavePending?: never
      watched?: never
      watchedPending?: never
      onPutWatched?: never
      onDeleteWatched?: never
      onWatched?: never
    })
  | (LivePlayerCommonProps & RecordingPlaybackProps & OriginalVODProps & {
      mode: 'original-vod'
      networkId?: never
      serviceId?: never
      chaseTimeline?: never
      liveProgram?: never
      onLiveProgramSeek?: never
      onStartOver?: never
      onReturnLive?: never
      liveDiagnostics?: never
      liveNotice?: never
      recordingHlsUrl?: never
      recordingHlsStartSeconds?: never
      showWatched?: never
    })
  | (LivePlayerCommonProps & RecordingPlaybackProps & OriginalVODProps & RecordingHLSProps & {
      mode: 'recording-hls'
      networkId?: never
      serviceId?: never
      chaseTimeline?: never
      liveProgram?: never
      onLiveProgramSeek?: never
      onStartOver?: never
      onReturnLive?: never
      liveDiagnostics?: never
      liveNotice?: never
      offsetSessionStart?: never
      onSourceRangeExit?: never
    })

/**
 * observeMediaStall は `lib/live.ts` の `observeStall` に **DOM の観測を渡す**
 * 薄い層である（issue #871）。判定の本体（閾値との比較・`paused` / 非表示タブの
 * 抑止）は純関数側にあり、ここがやるのは 4 つの値の読み取りだけである。
 */
function observeMediaStall(
  tracker: StallTracker,
  media: HTMLVideoElement,
  nowMs: number,
  resumePending: boolean,
): boolean {
  return observeStall(
    tracker,
    {
      // **自動再開を待っている間の `paused` は「利用者が止めた」ではない**
      // （切替の cleanup が `load()` で止めた）。待っている間は数えないと、
      // 降格先も死んでいる場合に検出ごと止まる
      paused: media.paused && !resumePending,
      hidden: document.hidden,
      currentTime: media.currentTime,
    },
    nowMs,
  )
}

/**
 * LivePlayer はライブ視聴の HLS プレイリストを再生する（M4-4）。
 *
 * 再生に先立ち `probeLivePlaylist` で 1 回プレイリストを取得し、成功したときだけ
 * hls.js に URL を渡す（hls.js のエラーイベントは HTTP ステータス・本文を運ばない
 * ため、事前確認でしか区別できない）。ライブラリは動的 import にして、再生画面以外の
 * 初期バンドルに含めない。
 *
 * チャンネル切り替えは呼び出し側が `serviceId` を変えて渡す（`key` での再マウントを
 * 前提にしない --- effect の cleanup で確実に破棄する）。破棄すると即座にセグメント
 * 要求が止まり、あわせて**離脱のヒント**を送る（下の effect）。ヒントはセッションを
 * 止めるのではなく idle 期限を短い猶予まで詰めるだけなので、同じチャンネルを見て
 * いる別の視聴者がいれば何も起きない（`lib/live.ts` の `sendPlaybackLeaveHint`）。
 */
export function LivePlayer(props: LivePlayerProps) {
  const {
    mode,
    onLiveProgramSeek,
    onStartOver,
    onReturnLive,
    liveDiagnostics,
    liveNotice,
    site,
    networkId,
    serviceId,
    recordingId,
    offsetSessionStart,
    fullscreenContainerRef,
    profile,
    audio,
    autoPlay = false,
    onRecordingPositionChange,
    onSourceRangeExit,
    onRecordingPlaybackError,
    className,
    onDiagnostics,
    onStalled,
    recordingDurationMs,
    chapters,
    chapterSource = 'auto',
    chapterVersion,
    chapterDetectionPending = false,
    chapterEditing = false,
    onEnterChapterEditing,
    chapterEditorCommandsRef,
    onChapterEditorStatusChange,
    onSaveChapters,
    onResetChapters,
    chapterSavePending = false,
    availableProfiles,
    onProfileChange,
    onAudioChange,
    watched = false,
    watchedPending = false,
    onPutWatched,
    onDeleteWatched,
    onWatched,
    recordingHlsUrl,
    recordingHlsStartSeconds,
    showWatched: recordingHlsShowWatched,
  } = props
  const isChase = mode === 'chase'
  const isOriginalVOD = mode === 'original-vod'
  const isRecordingHLS = mode === 'recording-hls'
  const isLive = mode === 'live'
  const isOffsetRecordingPlayback = isChase || isOriginalVOD
  const isRecordingPlayback = isOffsetRecordingPlayback || isRecordingHLS
  const isOriginalPlayback = isOriginalVOD || isRecordingHLS
  const originalDurationSeconds = recordingDurationSeconds(recordingDurationMs)
  const strategy = playbackStrategy(props)
  const videoRef = useRef<HTMLVideoElement>(null)
  const frameRef = useRef<HTMLDivElement>(null)
  const hlsRef = useRef<HlsLike | null>(null)
  const isOriginalScrubbingRef = useRef(false)
  const leaveOffsetSession = (offsetSeconds: number) => {
    const url = strategy.leaveURL(offsetSeconds)
    if (url !== null) sendPlaybackLeaveHint(url)
  }
  const {
    offsetSeconds: offsetSessionSeconds,
    sessionStartSeconds,
    startPositionSeconds: sessionStartPositionSeconds,
    hasExplicitStart: hasExplicitSessionStart,
    sessionKey: offsetSessionKey,
    restartAtOffset,
    seek: seekOffsetSession,
    clearStartPosition,
    reportPosition,
    getAppliedRecordingSeconds,
    getStartPositionSeconds,
    resumePlaybackPendingRef,
    retry: retryOffsetSession,
  } = useOffsetSession({
    active: isOffsetRecordingPlayback,
    identity: mode,
    recordingId,
    start: isRecordingHLS ? { type: 'beginning' } : offsetSessionStart ?? { type: 'saved-position' },
    videoRef,
    originSeconds: strategy.originSeconds,
    onLeave: leaveOffsetSession,
    onSourceRangeExit,
    onRecordingPositionChange,
  })
  usePlaybackLeave(strategy.leaveURL(offsetSessionSeconds))
  const hasExplicitChaseStart = isChase && hasExplicitSessionStart
  // 原本 VOD で最後に playlist が取れた offset（録画ごと）。終端付近の 416 を丸める下限。
  const lastGoodOffsetRef = useRef<{ recordingId: number | undefined; offset: number }>({ recordingId, offset: 0 })
  // 416 のたびに手前へ戻す幅（秒）。成功したら 1 に戻す。
  const rangeStepRef = useRef(1)
  // 最初のセッションの自動再生が残っているか（上の autoPlay）。
  const autoPlayPendingRef = useRef(autoPlay)
  // この要素が一度でも再生を始めたか。始めていないセッションのエラーは「位置」を持たない。
  const playedRef = useRef(false)
  // fatal で hls を破棄した映像は currentTime 0 の timeupdate を出す。それを最後の位置にしない
  // （`live.mjs` の #1143 再読み込み）。次の再生元を始めるときに解く。
  const positionReportBlockedRef = useRef(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<LiveLoadError | null>(null)
  const [manualRetryGeneration, setManualRetryGeneration] = useState(0)
  const [playbackRate, setPlaybackRate] = useState(loadPlaybackRate)
  const [chapterEditPlaybackRate, setChapterEditPlaybackRate] = useState(loadChapterEditPlaybackRate)
  const activePlaybackRate = chapterEditing ? chapterEditPlaybackRate : playbackRate
  const saveActivePlaybackRate = chapterEditing ? saveChapterEditPlaybackRate : savePlaybackRate
  const setActivePlaybackRate = chapterEditing ? setChapterEditPlaybackRate : setPlaybackRate
  const playbackRateRef = useRef(playbackRate)
  const saveActivePlaybackRateRef = useRef(saveActivePlaybackRate)
  const setActivePlaybackRateRef = useRef(setActivePlaybackRate)
  const [originalVODProfileOverrideState, setOriginalVODProfileOverrideState] = useState<{
    recordingId: number | undefined
    value: string | undefined
  }>({ recordingId, value: undefined })
  const originalVODProfileOverride = originalVODProfileOverrideState.recordingId === recordingId
    ? originalVODProfileOverrideState.value
    : undefined
  const [originalVODAudioOverrideState, setOriginalVODAudioOverrideState] = useState<{
    recordingId: number | undefined
    set: boolean
    value: LiveAudioChoice | undefined
  }>({ recordingId, set: false, value: undefined })
  const originalVODAudioOverride = originalVODAudioOverrideState.recordingId === recordingId
    ? originalVODAudioOverrideState
    : { recordingId, set: false, value: undefined }
  const [liveAudioOverrideState, setLiveAudioOverrideState] = useState<{
    set: boolean
    value: LiveAudioChoice | undefined
  }>({ set: false, value: undefined })
  const playbackProfile = isOriginalPlayback
    ? onProfileChange ? profile : originalVODProfileOverride ?? profile
    : profile
  const displayedFrameVideoKey = `${recordingId}:${playbackProfile ?? ''}:${offsetSessionSeconds}`
  const getSessionDisplayedFrameSeconds = useDisplayedFrameSeconds(
    videoRef,
    chapterEditing && isOriginalPlayback,
    displayedFrameVideoKey,
  )
  const getDisplayedFrameSeconds = useCallback(() => {
    const mediaTime = getSessionDisplayedFrameSeconds()
    return mediaTime === null ? null : sessionStartSeconds + mediaTime
  }, [getSessionDisplayedFrameSeconds, sessionStartSeconds])
  const [editorSelected, setEditorSelected] = useState<number | null>(null)
  const localChapterEditorCommandsRef = useRef<ChapterEditorCommands | null>(null)
  const resolvedChapterEditorCommandsRef = chapterEditorCommandsRef ?? localChapterEditorCommandsRef
  const playbackAudio = isOriginalPlayback && !onAudioChange && originalVODAudioOverride.set
    ? originalVODAudioOverride.value
    : isLive && !onAudioChange && liveAudioOverrideState.set
      ? liveAudioOverrideState.value
      : audio
  const [playbackAxisState, setPlaybackAxisState] = useState<PlaybackAxisState>({
    identity: '',
    positionSeconds: 0,
    previewSeconds: null,
    hoverSeconds: null,
  })
  const playbackAxisIdentity = createPlaybackAxisIdentity(
    mode,
    site,
    networkId,
    serviceId,
    recordingId,
    offsetSessionKey,
  )
  const hasCurrentPlaybackAxisState = playbackAxisState.identity === playbackAxisIdentity
  const playbackHoverSeconds = hasCurrentPlaybackAxisState ? playbackAxisState.hoverSeconds : null
  const playbackAxis = strategy.axis(playbackHoverSeconds)
  const chaseAxis = playbackAxis?.kind === 'chase' ? playbackAxis : undefined
  const liveTimelineBar = playbackAxis?.kind === 'live-program' ? playbackAxis : undefined
  const chaseHeadOffsetSeconds = chaseAxis?.headSeconds ?? 0
  // 軸上の位置は全モードで head + sessionStartSeconds + media。chase の sessionStartSeconds は offset と
  // 同じ値、live は 3 項とも 0。
  const initialAxisPositionSeconds = chaseHeadOffsetSeconds + sessionStartSeconds +
    (isRecordingHLS ? recordingHlsStartSeconds ?? 0 : sessionStartPositionSeconds ?? 0)
  const playbackAxisForRender: PlaybackAxisState = hasCurrentPlaybackAxisState
    ? playbackAxisState
    : {
        identity: playbackAxisIdentity,
        positionSeconds: initialAxisPositionSeconds,
        previewSeconds: null,
        hoverSeconds: null,
      }
  if (!hasCurrentPlaybackAxisState) setPlaybackAxisState(playbackAxisForRender)
  const updatePlaybackAxisState = (patch: Partial<Omit<PlaybackAxisState, 'identity'>>) => {
    setPlaybackAxisState((current) => patchPlaybackAxisState(
      current,
      playbackAxisIdentity,
      initialAxisPositionSeconds,
      patch,
    ))
  }
  const [originalTilesAvailable, setOriginalTilesAvailable] = useState(false)
  const playAroundStopRef = useRef<number | null>(null)
  const playAroundStopBoundaryMsRef = useRef<number | null>(null)
  const playAroundTimerRef = useRef<number | undefined>(undefined)
  const pendingBoundarySeekMsRef = useRef<number | null>(null)
  const [originalSubtitlesEnabled, setOriginalSubtitlesEnabled] = useState(isRecordingPlayback)
  const [chapterSkipEnabled, setChapterSkipEnabled] = useState(loadChapterSkip)
  const chapterEditorStatusChange = onChapterEditorStatusChange ?? (() => {})
  const originalPreviousSecondsRef = useRef(0)
  const onWatchedRef = useRef(onWatched)
  useEffect(() => {
    onWatchedRef.current = onWatched
  }, [onWatched])
  const restorePending = useRef(true)
  // preservedState は画質（プロファイル）の切替・再読み込みを跨いで持ち越す
  // 視聴者の表示状態（issue #869 / #871）。字幕の表示と、切替前に再生中だったかを持つ。
  // 再生中に profile を切り替えると cleanup の `video.load()` が paused に戻すため、
  // 新しい playlist を張ったあとに再生を再開する必要がある。
  //
  // **字幕と再生状態は違う。** 字幕は利用者が切った/入れたという表示状態だが、
  // `playing` は不可逆な事実ではなく、毎回 cleanup の瞬間に読み直せる外部状態である。
  // 再生位置や音量を保存する永続状態にはしない。
  //
  // **音量とミュートは持ち越す必要が無い（実測）。** cleanup は `load()` を行うが、
  // Chromium と WebKit の両方で音量・ミュートは既定に戻らない。
  //
  // **字幕は違う。** hls.js は新しいマニフェストを読むと字幕トラックの選択を
  // 既定に戻す（下の effect のコメント参照）。effect の cleanup（= 切替の直前）で
  // 読み、次の setup で戻す。
  const preservedState = useRef<{ subtitles: boolean | null; playing: boolean; recordingId?: number } | null>(null)
  // 追っかけの再生位置を画質の切替で持ち越す（issue #874）。
  //
  // **画質の切替は位置の基準を変えない。** 追っかけのセッション鍵は
  // `(recordingID, offset)` でプロファイルを含まず、サーバーの ffmpeg 1 本が
  // 全プロファイルを同時に出力している（`internal/streamer/hls_args.go` の
  // `buildChaseFFmpegArgs`）ので、プロファイル間でメディア時刻の座標は同じである。
  // それでも `src` を差し替える以上 `<video>` の位置は 0 に戻るので、切替の
  // 直前の位置を控えて戻す --- **画質の切替が再生位置の巻き戻りに見える実装に
  // しない**。
  //
  // **プロファイル以外の入力が変わったときは持ち越さない。** `offset` は新しい
  // セッションの先頭からの秒数で位置の基準そのものが変わるし、`offsetSessionKey` は
  // 「保存位置からやり直す」が正しい（既存の復元規則に任せる）。判定は effect の
  // setup で行う --- cleanup の時点では次に何が変わるかが分からない。
  const lastChasePositionRef = useRef<number | null>(null)
  const lastChaseInputsRef = useRef<string | null>(null)
  // 持ち越した位置へまだ戻し終えていない間の、その位置。**この間は位置を保存
  // しない** --- ソースの張り直し中に位置 0 の `timeupdate` が届くと、保存位置を
  // 0（offset 付きなら offset）で上書きするため、再生可能になるまでは書き込みを止める。
  const chaseResumePending = useRef<number | null>(null)
  // 今の読み込みが `loadedmetadata` に届いたか。**届く前の要素の位置（0）は
  // 持ち越さない** --- probe 中（ffmpeg の起動待ちで最長 15 秒）に切り替えると、
  // 0 を持ち越して既存の復元を潰し、保存位置が消える。jsdom の `readyState`
  // は動かないので、要素の状態ではなく自前で持つ。
  const chaseMetadataLoaded = useRef(false)
  const watchedRequestPending = useRef(false)
  /**
   * 原本 VOD の playlist が ENDLIST まで書かれたか。変換中の EVENT playlist の
   * `video.duration` は変換の先端でしかないので、これが true になるまで
   * 「終端付近」の判定に duration を渡さない（渡すと先端付近で位置が消える）。
   * 信号は hls.js の LEVEL_LOADED の `details.live === false`（ENDLIST あり）と
   * `ended` イベントである。
   */
  const originalVODFinalized = useRef(false)
  const saveCurrentPosition = useCallback((video: HTMLVideoElement, keepalive = false) => {
    if (mode === 'live' || chaseResumePending.current !== null) return
    const globalPosition = video.currentTime + sessionStartSeconds
    const knownFinalDuration = originalVODFinalized.current
      ? originalDurationSeconds || (video.duration + sessionStartSeconds)
      : video.duration
    const write = playbackPositionWrite(
      globalPosition,
      knownFinalDuration,
      isOriginalPlayback && originalVODFinalized.current,
    )
    if (write.kind === 'watched') {
      if (watchedRequestPending.current) return
      watchedRequestPending.current = true
      void persistPlaybackPosition(recordingId, write, keepalive).then((saved) => {
        if (saved) onWatchedRef.current?.()
        if (!saved) watchedRequestPending.current = false
      })
      return
    }
    void persistPlaybackPosition(recordingId, write, keepalive)
  }, [isOriginalPlayback, mode, originalDurationSeconds, recordingId, sessionStartSeconds])

  // Shared frame behavior lives here. The playback source supplies its own seek semantics and
  // HLS subtitle-track classification; live has no seek callbacks and chase has no ratio seek.
  const frame = usePlayerFrame(videoRef, frameRef, undefined, {
    fullscreenContainerRef,
    // commitRecordingSeek は後方の const。usePlayerFrame がコールバックを ref に入れ直し、
    // キー押下（render 後）にだけ呼ぶので、宣言前でも TDZ にならない。
    onSeekBy: strategy.recording
      ? (seconds) => {
          const video = videoRef.current
          if (!video) return
          commitRecordingSeek(chaseHeadOffsetSeconds + sessionStartSeconds + video.currentTime + seconds)
        }
      : undefined,
    onSeekToFraction: isOriginalPlayback && originalDurationSeconds > 0
      ? (fraction) => {
          seekRecording(originalDurationSeconds * fraction)
          return true
        }
      : undefined,
    onSavePosition: isRecordingPlayback ? saveCurrentPosition : undefined,
    savePositionKey: `${mode}:${recordingId}:${sessionStartSeconds}:${offsetSessionKey}`,
    getSubtitleTracks: hlsSubtitleTracks,
    subtitleState: originalSubtitlesEnabled,
  })
  const { setMediaPlaying } = frame
  // onDiagnostics は ref 越しに読む。probe / hls.js のセットアップを担う
  // メイン effect の依存配列に関数 prop をそのまま入れると、呼び出し側が
  // 毎レンダー新しい関数を渡した場合にプレイリストの再取得・hls インスタンスの
  // 再生成が起きてしまう --- ref なら常に最新の関数を呼びつつ、メイン effect の
  // 再実行条件からは切り離せる。
  const onDiagnosticsRef = useRef(onDiagnostics)
  // onStalled も同じ理由で ref 越しに読む（issue #871）。加えて、こちらは
  // **依存配列に置くと意味が壊れる** --- 呼び出し側は下げた後 `autoProfile` を
  // 変えるので、依存させると「下げた結果」が effect を張り直す経路が 2 本になる
  const onStalledRef = useRef(onStalled)
  // 親へ委ねるコールバックも ref 越しに読む（依存に入れるとセッションを張り直す）。
  const onRecordingPlaybackErrorRef = useRef(onRecordingPlaybackError)

  useEffect(() => {
    restorePending.current = true
    watchedRequestPending.current = false
  }, [
    mode,
    recordingId,
    // **`profile` を依存に入れない（issue #874）。** 入れると画質の切替のたびに
    // 復元が立ち直り、切替が「保存位置まで巻き戻る」操作になる（`offset` 付きなら
    // 先頭へ戻る）。位置の持ち越しは下の effect が `lastChasePositionRef` で行う。
    // 画質の切替は再生位置の基準を変えないので、復元をやり直す理由が無い。
    site,
    networkId,
    serviceId,
    offsetSessionKey,
    sessionStartSeconds,
    hasExplicitSessionStart,
    hasExplicitChaseStart,
    isOriginalPlayback,
  ])

  useEffect(() => {
    clearLegacyPlaybackPositions()
  }, [])

  useEffect(() => () => window.clearTimeout(playAroundTimerRef.current), [])

  useEffect(() => {
    onDiagnosticsRef.current = onDiagnostics
  }, [onDiagnostics])

  useEffect(() => {
    onStalledRef.current = onStalled
  }, [onStalled])

  useEffect(() => {
    onRecordingPlaybackErrorRef.current = onRecordingPlaybackError
  }, [onRecordingPlaybackError])

  // 音声トラックの選択（issue #870）。**メイン effect の依存に入れない** ---
  // 入れると切替のたびにプレイリストを取り直し、hls.js を作り直す。ここは今ある
  // 再生器のトラックを替えるだけで、トラックが後から届く分（読み込み直後・画質の
  // 切替後）はメイン effect が audioRef を読んで揃える。メイン effect より先に
  // 宣言して、初回の読み込みが最新の値を読むようにする。
  const audioRef = useRef(playbackAudio)
  useEffect(() => {
    audioRef.current = playbackAudio
    const index = liveAudioTrackIndex(playbackAudio)
    if (hlsRef.current) applyHlsAudioTrack(hlsRef.current, index)
  }, [playbackAudio])

  useEffect(() => {
    playbackRateRef.current = activePlaybackRate
    saveActivePlaybackRateRef.current = saveActivePlaybackRate
    setActivePlaybackRateRef.current = setActivePlaybackRate
  }, [activePlaybackRate, chapterEditing, saveActivePlaybackRate, setActivePlaybackRate])

  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const rate = effectivePlaybackRate(activePlaybackRate, isRecordingPlayback)
    const appliedRate = applyPlaybackRate(video, rate, saveActivePlaybackRate)
    if (isRecordingPlayback && appliedRate !== activePlaybackRate) {
      playbackRateRef.current = appliedRate
      setActivePlaybackRate(appliedRate)
    }
  }, [activePlaybackRate, chapterEditing, isRecordingPlayback, saveActivePlaybackRate, setActivePlaybackRate, videoRef])

  useEffect(() => {
    let cancelled = false
    positionReportBlockedRef.current = false
    // 切り替え・破棄が起きたら probe の fetch 自体を中断する。
    // `playlistStartupTimeout`（streamer 側、15 秒）ぶん in-flight のまま
    // 残さないため（レビュー #190 の指摘）。
    const controller = new AbortController()
    // effect の設定時点で 1 回だけ読む。cleanup で `videoRef.current` を直接
    // 読むと「クリーンアップが走る時点でもまだ同じノードを指しているか」が
    // 保証できない（react-hooks/exhaustive-deps が指摘する形）ため、同じ
    // effect の中で捕まえた変数を setup・cleanup の両方から使う。
    const video = videoRef.current
    // 前回の切替で持ち越した表示状態を戻す（issue #869）。`<video>` 要素自体は
    // 作り直さない（このコンポーネントは unmount しない）が、`load()` を挟む以上
    // 要素の状態に頼らず明示的に戻す。
    const preserved = preservedState.current
    // 画質（プロファイル）だけが変わった再実行か（`lastChasePositionRef` の
    // コメント参照）。プロファイルを含めない入力の同一性で判定する。
    const recordingInputs = `${mode}|${site}|${recordingId}|${sessionStartSeconds}|${hasExplicitSessionStart}|${offsetSessionKey}`
    const resumePosition =
      isRecordingPlayback && lastChaseInputsRef.current === recordingInputs
        ? lastChasePositionRef.current
        : null
    lastChaseInputsRef.current = recordingInputs
    originalVODFinalized.current = false
    // 原本 VOD は開始位置を明示する（続きから・画質切替の持ち越し・張り直しの端数の
    // どれも無ければ 0）。offset のセッションも同じ hls.js の設定で開始位置を揃える。
    // 追っかけも開始位置を明示したセッション（offset 付き・明示 0 秒）は同じ経路で 0 を明示する。
    const startAt = isRecordingHLS
      ? (resumePosition ?? recordingHlsStartSeconds ?? 0)
      : isOriginalVOD || hasExplicitChaseStart
        ? (resumePosition ?? getStartPositionSeconds() ?? 0)
        : isChase
          ? (resumePosition ?? getStartPositionSeconds())
          : null
    chaseResumePending.current = startAt
    if (startAt !== null) {
      // 持ち越しは既存の復元より優先する。サーバーの再開位置が更新されて復元が
      // 立ち直っていると、`onLoadedMetadata` が保存位置へ戻してしまう。
      restorePending.current = false
    }
    // 同じ録画の中で張り直した（セッション外へのシーク・画質の切替・416 の丸め）ときは再生を
    // 引き継ぐ。`load()` は pause を発火しないので、引き継がないときは操作バーの状態を戻す。
    // 別の録画へ替わったときは勝手に再生を始めない。ライブ（`recordingId` を持たない）から
    // 追っかけへ替えたとき（ライブページの「最初から」・軸のシーク）は同じ番組なので引き継ぐ
    // （`live.mjs` の「frame c)」。引き継がないと 0:00 で止まったままになる）。
    // 自動再生の指定は最初のセッションにだけ効く。張り直し前に消費しても、再開待ちの
    // `resumePlaybackPendingRef` が次のセッションへ持ち越す（下の `preservedState`）。
    const autoPlayNow = isRecordingPlayback && autoPlayPendingRef.current
    autoPlayPendingRef.current = false
    const resumePlaying =
      (preserved?.playing === true &&
        (!isRecordingPlayback || preserved.recordingId === undefined || preserved.recordingId === recordingId)) ||
      autoPlayNow
    resumePlaybackPendingRef.current = resumePlaying
    if (isRecordingPlayback && !resumePlaying) setMediaPlaying(false)
    // video / hls の外部再生状態と UI の loading/error 表示を同期する effect。
    // render 中に導出すると、再生開始・失敗イベントの境界を表現できない。
    // oxlint-disable-next-line react/set-state-in-effect -- 別ソースの読込状態を setup ごとに初期化する
    setLoading(true)
    setError(null)
    onDiagnosticsRef.current?.(null)

    const url = mode === 'chase'
      ? chasePlaylistURL(site, recordingId, playbackProfile, offsetSessionSeconds)
      : mode === 'original-vod'
        ? originalVODPlaylistURL(site, recordingId, playbackProfile, offsetSessionSeconds)
        : mode === 'recording-hls'
          ? recordingHlsUrl!
          : livePlaylistURL(site, networkId, serviceId, playbackProfile)

    /**
     * handOffError は録画再生のエラーを親に渡し、親が再生元を選び直したら true を返す
     * （このプレイヤーはエラー表示に落ちない）。一度も再生していないセッションのエラーは
     * 位置を渡さない --- 0 秒を「明示の位置」にすると、親が持つ保存位置・先頭からの意図を上書きする。
     * 要求した開始位置が適用中の間も currentTime は位置として使わない。原本 HLS と追っかけの
     * seek 失敗で誤った秒を作り直しへ渡さない（`recording-original-vod.mjs` ⑪-b、`chase.mjs` ⑭-b）。
     * 再生の意図は、始まる前なら再開待ち（▶ の autoPlay・張り直しの持ち越し）だけで決める。
     * 「再生していない」ことを再生の意図にすると、止めたまま移った先のエラーで勝手に再生が始まる
     * （両方向は `live-player.test.tsx` の「再生前のエラー」）。
     */
    const handOffError = (
      media: HTMLVideoElement | null,
      allowSameSourceRetry = true,
    ): boolean | Promise<boolean> => {
      if (!isRecordingPlayback) return false
      const recordingHlsPositionApplied = !isRecordingHLS || chaseResumePending.current === null
      const position = playedRef.current && media
        ? (recordingHlsPositionApplied ? getAppliedRecordingSeconds(media.currentTime) ?? undefined : undefined)
        : undefined
      const wasPlaying = media ? !media.paused || resumePlaybackPendingRef.current : true
      return onRecordingPlaybackErrorRef.current?.(position, wasPlaying, allowSameSourceRetry) ?? false
    }

    // teardown はこの effect が張ったものを外す手続き（メディアイベントの
    // リスナと stall 監視のタイマー）。cleanup から呼ぶ
    const teardown: Array<() => void> = []

    chaseMetadataLoaded.current = false
    const markLoaded = () => {
      chaseMetadataLoaded.current = true
    }
    if (video && isRecordingPlayback) {
      video.addEventListener('loadedmetadata', markLoaded, { once: true })
      teardown.push(() => video.removeEventListener('loadedmetadata', markLoaded))
    }

    // 読み込み直しで持ち越した位置は、loadedmetadata の後に確認する。canplay で
    // 位置を確定して保存を再開する前に、hls.js の startPosition とずれた場合だけ戻す。
    if (video && startAt !== null) {
      const apply = () => {
        if (!cancelled && Math.abs(video.currentTime - startAt) > 0.5) {
          video.currentTime = startAt
        }
      }
      const settle = () => {
        if (chaseResumePending.current !== null) apply()
        if (cancelled) return
        chaseResumePending.current = null
        clearStartPosition()
        if (isOriginalPlayback) {
          // cancelled を見たあとなので、state の identity はこの effect のもの。
          const positionSeconds = sessionStartSeconds + startAt
          setPlaybackAxisState((current) => ({ ...current, positionSeconds }))
          if (isRecordingHLS) reportPosition(startAt)
        }
      }
      video.addEventListener('loadedmetadata', apply, { once: true })
      video.addEventListener('canplay', settle, { once: true })
      teardown.push(() => {
        video.removeEventListener('loadedmetadata', apply)
        video.removeEventListener('canplay', settle)
      })
    }

    /**
     * watchLiveDiagnostics は「放送から n 秒 / 先読み n 秒」の計器を 1 秒ごとに
     * 更新する（issue #476。「副調整室の計器盤」--- ON AIR・録画中バッジと同じ
     * 「いま電波に乗っているものとの距離」を言う表示）。
     *
     * **「測り直す」ボタンは置かない。** 値はこのポーリングで毎秒最新に
     * 更新されるため、手動での再計測に意味を持たせられない（denpa の
     * 「測り直す」は WHEP 側の再ネゴシエーションの都合であり、hls.js の
     * ポーリングにはそれに対応する操作が無い）。
     *
     * 停止関数を返すのは、fatal エラー・メディア失敗で読む理由が無くなった
     * 直後にも呼び出し側から止められるようにするため。**実 hls.js は
     * `destroy()` 後に `latency` / `mainForwardBufferInfo` を読んでも例外は
     * 投げない**（`Hls.prototype.destroy` は `LatencyController.destroy()` で
     * 内部の `hls` 参照を `null` にするだけで、`_latency` はそのまま残る ---
     * `get latency()` は直前値を返し続ける。`Hls.prototype.latency` 側も
     * `latencyController?.latency || 0` で、`Hls.destroy` は
     * `latencyController` を `null` にしない。`node_modules/hls.js` 1.7.1 を
     * 読んで確認済み）。ここで止めるのは例外対策ではなく、意味の無くなった
     * 値を毎秒読み続けない衛生。
     *
     * **停止と同時に `onDiagnostics(null)` を出す。** 呼び出し側
     * （`pages/live.tsx`）はエラー表示自体を知らず `isPlaying && diagnostics`
     * だけで出し分けているため、値を消さずに止めるだけだと最後の測定値が
     * 凍ったまま ON AIR バッジの隣に残り続ける --- fatal エラーでプレイヤーが
     * 「エラーが発生しました」を出している間も「放送から約5秒」等の偽の
     * 値が居座る（レビュー指摘。表示位置を `pages/live.tsx` へ戻した際に
     * 入り込んだ回帰）。
     *
     * `onTick` は同じ 1 秒の刻みで呼ばれる（issue #871 の停滞の観測）。
     * **停滞の観測のために別のタイマーを作らない** --- 同じ「いまどうなっているか」
     * を 2 つの周期で見ても精度は上がらず、止め忘れの経路だけが増える。
     * 世代付き録画 HLS の世代切替は hls.js の 410 応答で再取得する。
     */
    function watchLiveDiagnostics(read: () => LiveDiagnostics, onTick?: () => void): () => void {
      const tick = () => {
        if (cancelled) return
        onDiagnosticsRef.current?.(read())
        onTick?.()
      }
      tick()
      const timer = setInterval(tick, 1000)
      const stop = () => {
        clearInterval(timer)
        onDiagnosticsRef.current?.(null)
      }
      teardown.push(stop)
      return stop
    }

    async function start() {
      let probe: Awaited<ReturnType<typeof probeLivePlaylist>>
      try {
        probe = await probeLivePlaylist(url, controller.signal, strategy.classifyLoadError)
      } catch (err) {
        // 中断（チャンネル切り替え・破棄）は無視する。エラー表示にはしない ---
        // 単に「もう見たいものが変わった」だけで、失敗ではない
        if (err instanceof DOMException && err.name === 'AbortError') return
        throw err
      }
      if (cancelled) return
      const lastGood = lastGoodOffsetRef.current.recordingId === recordingId ? lastGoodOffsetRef.current.offset : 0
      if (!probe.ok) {
        // 終端付近の 416（docs/api/media.md: 映像の終端 - 0.5 秒より後ろの offset）。シークバーの
        // 長さは DB の実尺で、映像の長さより長いことがある。映像の長さは分からないので、最後に
        // 取れた offset を下限に 1, 2, 4… 秒ずつ手前へ丸めて張り直す（下限は取れたので必ず止まる）。
        if (
          isOriginalVOD &&
          probe.error.kind === 'other' &&
          probe.error.status === 416 &&
          offsetSessionSeconds > lastGood
        ) {
          const next = Math.max(lastGood, offsetSessionSeconds - rangeStepRef.current)
          rangeStepRef.current *= 2
          restartAtOffset(next)
          return
        }
        // 入力失敗の cooldown 応答でも詳細を取り直し、並行した ingest commit があれば
        // 別の再生元へ移る。同じ追っかけの再試行は cooldown 中なので許可しない。
        const handedOff = await handOffError(video, probe.error.kind !== 'chase-input')
        if (cancelled || handedOff) return
        // 世代付き録画 HLS は、親が次の世代の URL を渡すとこの effect が読み直す。再生の意図を
        // 残し、利用者の操作なしで再生を続ける（`recording-detail.test.tsx` の「エラー表示に落ちた後でも」）。
        if (!isRecordingHLS) resumePlaybackPendingRef.current = false
        if (isRecordingPlayback) setMediaPlaying(false)
        setError(probe.error)
        setLoading(false)
        return
      }
      if (isOriginalVOD) {
        lastGoodOffsetRef.current = { recordingId, offset: offsetSessionSeconds }
        rangeStepRef.current = 1
      }

      if (!video) return

      // **自動再開を待っている間は `paused` を利用者の停止として扱わない。**
      // `load()` が pause を立てるため、待たずに検出を止めると、降格先も止まった
      // 場合に停滞の検出ごと止まって黒いままになる。
      let resumePending = resumePlaying

      // **切替前に再生中だったなら、新しいソースが再生可能になってから再開する。**
      // 画質切替の cleanup は `video.load()` を呼ぶので、そのままでは paused に
      // 戻ったまま誰も再開しない（実測: 自動降格の直後は paused=true で、
      // セグメントを復旧させても `currentTime` は 0 のまま）。
      //
      // **`play()` を `src` の代入や `attachMedia` の直後に呼んではならない。**
      // その後に行われる load algorithm（`src` の代入・hls.js の MediaSource
      // アタッチ）が `paused` を true に戻すので競争に負ける（実測: hls.js 経路で
      // `play()` は呼ばれたのに `paused=true` のままだった）。`canplay` は
      // 「再生できるだけのデータが載った」ことを表すので、ここなら上書きされない。
      //
      // **初回のマウント（`preserved` が `null`）では呼ばない** --- 「再生」ボタンで
      // マウントしただけで再生を始めると、同意の分離（issue #234）が壊れる。
      // 拒否（自動再生のポリシー）は握り潰す --- 利用者は既存の controls から
      // 再生できる。
      if (resumePlaying) {
        const resume = () => {
          resumePending = false
          if (cancelled || !resumePlaybackPendingRef.current) return
          resumePlaybackPendingRef.current = false
          // 上の effect が canplay で開始位置を確定してから再開する。
          void video.play().catch(() => {
            if (isRecordingPlayback) setMediaPlaying(false)
          })
        }
        video.addEventListener('canplay', resume, { once: true })
        teardown.push(() => video.removeEventListener('canplay', resume))
      }

      // 自動降格を試してよいか（issue #871）。**全プロファイルを束ねた master では
      // 試さない。** そのとき streamer は `?profile=` に関わらず同じ master を返すので
      // （`live.captions: true`）、下げても何も変わらないのに「下げました」と
      // 表示することになる。判定は probe が読んだ本文に基づく
      //（`lib/live.ts` の `bundlesProfiles` --- API の一覧ではなく本文が権威）。
      // プロファイルごとの master（音声レンディション入り）では試す。
      //
      // **追っかけ再生（`isChase`）でも試さない。** 下げ先（`?liveProfile=`）は
      // 追っかけの画面にもあるが、この降格は `pages/live.tsx` の `autoProfile` に
      // 配線されており、追っかけのセレクタへ同じ下げ方を広げるのは別の判断である
      // （呼び出し側が `onStalled` を渡さないことでも止まる）。
      const canDowngrade = !probe.bundlesProfiles && !isRecordingPlayback
      const { default: Hls } = await import('hls.js')
      if (cancelled) return
      if (!Hls.isSupported()) {
        setError({
          kind: 'other',
          status: 0,
          message: strategy.errorMessages.unsupported,
        })
        setLoading(false)
        return
      }
      // master playlist の EXT-X-MEDIA subtitles rendition を**既定で表示**する
      // （issue #430）。subtitleDisplay は HlsConfig ではなく Hls インスタンスの
      // プロパティである。
      //
      // **これは「トグルを出す」設定ではない。** 字幕の入切は Chrome
      // ネイティブコントロールの `⋮` → 「Captions」で、textTrack が 1 本でも
      // あれば rokuban が何もしなくても現れる（実測: 実 Chromium で `⋮` を
      // 開いて "Captions / Off" の項目を確認）。この行が決めるのは既定の
      // 状態だけで、true なら `TextTrack.mode === "showing"` で始まる（実測:
      // 実 mirakc のライブで確認）。
      //
      // **VOD 側（recording-player.tsx の `<track>`）とは既定が逆で、それが正しい。**
      // あちらは `default` 属性を持たないので `mode === "disabled"` で始まり、
      // ユーザーが `⋮` から入れるまで `.vtt` を fetch すらしない（実測）。
      // VOD で `default` を付けると、**字幕サイドカーを持たないプロファイルでも
      // 再生ごとに必ず 1 本 404 が出る** --- クライアントはサイドカーの有無を
      // 知る手段を持たないので（`docs/api/media.md` の案 (b) の帰結）、
      // 付ける/付けないを録画ごとに選べない。ライブは ffmpeg が字幕ストリームを
      // 実際に map できたときだけ rendition が master に載るので、この問題が無い
      // --- 既定 ON にできるのはライブ側だけ、という非対称である。
      // hls.js otherwise chooses the live edge for an EVENT playlist. Chase
      // playback must begin at the first segment of the selected session;
      // the streamer has already applied any recording-relative offset.
      const hls = new Hls(
        isRecordingPlayback ? { startPosition: startAt ?? 0 } : undefined,
      ) as unknown as HlsLike
      hls.subtitleDisplay = true
      // 字幕の表示状態を持ち越す（issue #869）。**hls.js は新しいマニフェストを
      // 読むと字幕トラックの選択を既定に戻す** --- `SubtitleTrackController` の
      // `onManifestLoading` が `tracks = []` / `trackId = -1` /
      // `selectDefaultTrack = true` にする（`node_modules/hls.js` 1.7.1 で確認済み）。
      // そのため素朴に作り直すと、ネイティブコントロールの `⋮` → Captions で
      // 「切り」にした字幕が「入」に戻る。
      //
      // **`MANIFEST_PARSED` の時点では既定トラックはまだ選ばれていない**
      // （同コントローラの `onManifestParsed` は `tracks` を代入するだけで、
      // 既定トラックの選択はその後の level 更新経路の `setSubtitleTrack` →
      // `toggleTrackModes`。だから `subtitleDisplay` セッターの
      // `if (this.trackId > -1) this.toggleTrackModes()` はここでは発火しない）。
      // それでもここで `_subtitleDisplay` を書くのは、**後の選択のときに
      // `toggleTrackModes` がその値を読む**ためである。
      // **この経路が効くことは実ブラウザで実測済みである。** 利用者が字幕を切ってから
      // 画質を切り替えると `mode` は `hidden` のまま（= 再表示されない）。
      // この 1 行を外すと `showing` に戻ることを `web/e2e/live.mjs` の ⑩ が
      // 実 Chromium で捕まえる。
      if (preserved?.subtitles != null) {
        const visible = preserved.subtitles
        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          if (!cancelled) hls.subtitleDisplay = visible
        })
      }
      // 音声の選択を揃える（issue #870）。この effect は画質の切替・再読み込みの
      // たびに hls.js を作り直し、新しいインスタンスは master の既定（標準）から
      // 始まるので、トラック一覧が届いたら選択を適用する。前に聴いたトラックへ
      // 戻る切替は、ライブの playlist に EXT-X-PROGRAM-DATE-TIME が無いと止まる
      // （streamer の hlsFlags。`web/e2e/live-audio.mjs` の ①）。
      hls.on(Hls.Events.AUDIO_TRACKS_UPDATED, () => {
        if (!cancelled) applyHlsAudioTrack(hls, liveAudioTrackIndex(audioRef.current))
      })
      hlsRef.current = hls
      // 停滞の観測（issue #871）は**計器と同じ 1 秒の刻みに相乗りする**。
      // hls.js 経路では `stalled` / `waiting` を失敗判定に使えない
      // （MSE は正常時にも `waiting` を何度も出す）ので、`currentTime` が
      // 進んだかを計器と同じ 1 秒の刻みで見る。
      // **MSE でも観測できる唯一の信号がこれである** --- hls.js の
      // 非 fatal `bufferStalledError` は使わない（正常時にも出るうえ、
      // 頻度を測っていない）。
      let tracker = createStallTracker()
      const stopDiagnostics = watchLiveDiagnostics(
        () => readHlsDiagnostics(hls),
        () => {
          if (!canDowngrade || !observeMediaStall(tracker, video, Date.now(), resumePending)) return
          // 下げられたら、この effect ごと張り直されて計測も 0 に戻る。
          // 下げられない（段が尽きた / 明示選択）ときは**何もしない** ---
          // 現行の hls.js 経路は停滞を失敗として扱っていないので、ここで
          // エラーを新設すると**プロファイルが 1 件しかないデプロイの挙動まで
          // 変わる**（`docs/frontend/live.md` §フロントエンド実装）。
          // 一覧がまだ届いていないときだけ `wait` で観測を初期化し、一覧到着後の
          // 次の刻みで再判定できるようにする。
          if (onStalledRef.current?.() === 'wait') tracker = createStallTracker()
        },
      )
      if (isOriginalPlayback) {
        hls.on(Hls.Events.LEVEL_LOADED, (_event, data) => {
          if (data.details?.live === false) originalVODFinalized.current = true
        })
      }
      hls.on(Hls.Events.ERROR, (_event, data) => {
        const expiredRecordingHls = isRecordingHLS && data.response?.code === 410
        if ((!data.fatal && !expiredRecordingHls) || cancelled) return
        const finishFatalError = (handled: boolean) => {
          if (cancelled) return
          positionReportBlockedRef.current = true
          hls.destroy()
          hlsRef.current = null
          stopDiagnostics()
          if (handled) return
          // fatal のまま放置すると hls.js が内部でリトライを続け、エラー画面の
          // 裏でセグメント要求が続く（= idle GC も効かない。レビュー #190 の
          // 指摘）。表示するエラーは「壊れて止まった」なので、実際に止める
          setError({
            kind: 'other',
            status: 0,
            message: strategy.errorMessages.fatalTitle,
          })
        }
        const handedOff = handOffError(video)
        if (typeof handedOff === 'boolean') finishFatalError(handedOff)
        else void handedOff.then(finishFatalError, () => finishFatalError(false))
      })
      hls.loadSource(url)
      hls.attachMedia(video)
      setLoading(false)
    }

    void start()

    return () => {
      cancelled = true
      controller.abort()
      // 画質切替・再読み込みを跨いで持ち越す表示状態を、壊す前の要素から読む
      // （issue #869）。unmount のときも走るが、ref ごと捨てられるので無害。
      if (video) {
        preservedState.current = {
          subtitles: readSubtitleVisibility(Array.from(video.textTracks)),
          // 切替の直前（= cleanup の時点）の再生状態。`load()` は paused を
          // true に戻すので、これを見ないと切替のたびに利用者が押し直すことになる
          // 再開を待っている間（canplay の前）に張り直したときも、再生の意図を引き継ぐ。
          playing: !video.paused || resumePlaybackPendingRef.current,
          recordingId,
        }
        // 画質の切替で持ち越す再生位置（issue #874）。**`src` を外す前に読む。**
        // 前の持ち越しを戻し終える前にもう一度切り替えたときは、要素の位置
        // （まだ 0）ではなく持ち越し中の位置を引き継ぐ。
        lastChasePositionRef.current = isRecordingPlayback
          ? (chaseResumePending.current ??
            (chaseMetadataLoaded.current ? video.currentTime : null))
          : null
      }
      // メディアイベントのリスナと stall タイマーを外す。
      //
      // **実際に効いている防御は `failed()` の `cancelled` チェックの方である。**
      // この行を `src` の解除より前に置いているのは「解除自体が出しうる `error` を
      // 拾わないため」だが、**その効き目は測れていない** --- 順序を入れ替えても、
      // さらに `cancelled` チェックを外しても、WebKit ではチャンネル切替で
      // `error` が出ず判定に差が出なかった（`web/e2e/live.mjs` で実測）。
      // 順序は無害な保険として残す。効くと分かっている主張ではない
      for (const fn of teardown) fn()
      hlsRef.current?.destroy()
      hlsRef.current = null
      if (video) {
        video.removeAttribute('src')
        video.load()
      }
    }
  }, [
    isChase,
    isOriginalVOD,
    isOriginalPlayback,
    isRecordingHLS,
    isRecordingPlayback,
    mode,
    recordingHlsUrl,
    recordingHlsStartSeconds,
    manualRetryGeneration,
    playbackProfile,
    recordingId,
    site,
    networkId,
    serviceId,
    offsetSessionKey,
    offsetSessionSeconds,
    sessionStartSeconds,
    hasExplicitChaseStart,
    hasExplicitSessionStart,
    clearStartPosition,
    getAppliedRecordingSeconds,
    getStartPositionSeconds,
    reportPosition,
    resumePlaybackPendingRef,
    restartAtOffset,
    setMediaPlaying,
    strategy.classifyLoadError,
    strategy.errorMessages.fatalTitle,
    strategy.errorMessages.unsupported,
  ])

  const currentAxisSeconds = liveTimelineBar?.liveEdgeSeconds ??
    playbackAxisForRender.previewSeconds ?? playbackAxisForRender.positionSeconds
  const originalPlayedFraction = originalDurationSeconds > 0
    ? Math.max(0, Math.min(1, playbackAxisForRender.positionSeconds / originalDurationSeconds))
    : 0
  // ドラッグ中・キー操作中だけ位置のプレビューでつまみと時刻を動かす。マウスのホバーは吹き出しだけ。
  const playbackDurationSeconds = liveTimelineBar?.plannedEndSeconds ?? playbackAxis?.maxSeconds ?? 0
  const updateRecordingPosition = (video: HTMLVideoElement) => {
    if (mode === 'live') return
    if (positionReportBlockedRef.current) return
    if (isRecordingHLS && chaseResumePending.current !== null) return
    const seconds = chaseHeadOffsetSeconds + sessionStartSeconds + video.currentTime
    if (isOriginalPlayback) originalPreviousSecondsRef.current = seconds
    updatePlaybackAxisState({ positionSeconds: seconds })
    reportPosition(video.currentTime)
  }
  const isSelectableLiveProgramPoint = (seconds: number | null): seconds is number => {
    const range = liveTimelineBar?.selectableRange
    return seconds !== null && range !== undefined && range !== null &&
      seconds >= range.startSeconds && seconds <= range.endSeconds
  }
  const commitLiveProgramSeek = (seconds: number) => {
    if (!isSelectableLiveProgramPoint(seconds)) return
    updatePlaybackAxisState({ hoverSeconds: null })
    onLiveProgramSeek?.(seconds)
  }
  /**
   * seekRecording は時間軸上の秒（追っかけは番組開始から、原本は録画先頭から）へ移る。範囲は strategy が丸める。
   * 今のセッションの seekable の中ならセッション内をシークし、外なら、その秒を offset にして
   * セッションを張り直す（古いセッションへの leave ヒントは offset が変わったときの effect の
   * cleanup が送る）。範囲はバッファ済みではなく seekable で判定する（`seekOffsetSession`。追っかけも同じ）。
   * 原本 HLS の EVENT playlist は、セッション内の seekable の外へ出るときに
   * offset を指定して張り直す。バッファ済み範囲だけで判定すると、読める範囲外への
   * seek を見逃すため seekable を使う。
   */
  const seekRecording = (seconds: number) => {
    const video = videoRef.current
    const playlistEndSeconds = isRecordingHLS && video ? mediaSeekableEndSeconds(video) : undefined
    if (isRecordingHLS && playlistEndSeconds === undefined) return
    const target = strategy.recording?.seekTarget(seconds, playlistEndSeconds)
    if (target === undefined) return
    pendingBoundarySeekMsRef.current = null
    originalPreviousSecondsRef.current = target
    if (isRecordingHLS) {
      if (!video) return
      video.currentTime = target
      chaseResumePending.current = null
      clearStartPosition()
      reportPosition(target)
      updatePlaybackAxisState({ positionSeconds: target })
      return
    }
    const result = seekOffsetSession(target)
    if (result.type === 'source-changed' || result.type === 'unavailable') return
    updatePlaybackAxisState({ positionSeconds: chaseHeadOffsetSeconds + target })
  }
  /** commitRecordingSeek はシークバー・キー操作の確定。位置のプレビューを消してから移る。 */
  const commitRecordingSeek = (seconds: number) => {
    updatePlaybackAxisState({ previewSeconds: null })
    seekRecording(seconds)
  }
  const seekToOriginalBoundary = (boundaryMs: number) => {
    const media = videoRef.current
    if (!media) return
    if (media.seeking) {
      pendingBoundarySeekMsRef.current = boundaryMs
      return
    }
    pendingBoundarySeekMsRef.current = null
    seekRecording(chapterBoundaryMsToSeekSeconds(boundaryMs))
  }
  const clearPlayAround = () => {
    window.clearTimeout(playAroundTimerRef.current)
    playAroundTimerRef.current = undefined
    playAroundStopRef.current = null
    playAroundStopBoundaryMsRef.current = null
  }
  const selectChapterBoundary = (seconds: number) => {
    const media = videoRef.current
    if (!media) return
    clearPlayAround()
    // Boundary selection is an explicit stop, including while a restarted
    // original-HLS session is waiting for its canplay auto-resume.
    resumePlaybackPendingRef.current = false
    if (!media.paused) media.pause()
    seekToOriginalBoundary(Math.round(seconds * 1000))
  }
  const finishPlayAround = (media: HTMLVideoElement) => {
    const boundaryMs = playAroundStopBoundaryMsRef.current
    clearPlayAround()
    media.pause()
    if (boundaryMs !== null) seekToOriginalBoundary(boundaryMs)
  }
  const schedulePlayAroundStop = (media: HTMLVideoElement) => {
    const stop = playAroundStopRef.current
    if (stop === null || media.paused) return
    window.clearTimeout(playAroundTimerRef.current)
    const remainingSeconds = Math.max(0, stop - (sessionStartSeconds + media.currentTime))
    // セッション起動前に時計を進めると、offset HLS の起動待ちだけで境界が失われる。
    // playing/timeupdate 後に残り時間を測り、タイマー発火時にも位置を再確認する。
    playAroundTimerRef.current = window.setTimeout(() => {
      if (playAroundStopRef.current !== stop) return
      if (sessionStartSeconds + media.currentTime >= stop) {
        finishPlayAround(media)
      } else {
        schedulePlayAroundStop(media)
      }
    }, (remainingSeconds * 1000) / Math.max(media.playbackRate, 0.1) + 2000)
  }
  const playAround = (seconds: number, mode: 'around' | 'to' | 'from' = 'around') => {
    const media = videoRef.current
    if (!media) return
    clearPlayAround()
    const boundaryMs = Math.round(seconds * 1000)
    const boundarySeconds = boundaryMs / 1000
    const start = mode === 'from'
      ? chapterBoundaryMsToSeekSeconds(boundaryMs)
      : Math.max(0, boundarySeconds - PLAY_AROUND_SECONDS)
    const stop = mode === 'to' ? boundarySeconds : boundarySeconds + PLAY_AROUND_SECONDS
    playAroundStopRef.current = stop
    playAroundStopBoundaryMsRef.current = mode === 'to' ? boundaryMs : null
    seekRecording(start)
    void media.play().catch(() => {})
  }
  const jumpOriginalChapter = (direction: 'next' | 'prev') => {
    const target = chapterJumpTarget(chapters ?? [], playbackAxisForRender.positionSeconds, direction)
    if (target !== undefined) seekRecording(chapterBoundaryMsToSeekSeconds(Math.round(target * 1000)))
  }
  // 再開位置は原本時間軸で API に保存する。`profile` は映像品質の選択だけに使う。
  const video = (
    <video
      ref={videoRef}
      controls={false}
      playsInline
      aria-label={isRecordingPlayback ? (isChase ? '追っかけ映像' : '録画映像') : undefined}
      {...frame.video}
      className={cn(
        'absolute inset-0 size-full rounded object-contain',
        (loading || error) && 'invisible',
      )}
      onLoadedMetadata={(event) => {
        frame.onVolumeChange(event.currentTarget)
        // 追っかけは保存位置へ戻す前の位置を報告しない（再試行の上限が効かなくなる）。
        if (isOriginalPlayback) updateRecordingPosition(event.currentTarget)
        if (mode === 'live' || !restorePending.current) return
        restorePending.current = false
        event.currentTarget.currentTime = sessionStartPositionSeconds ?? 0
        updateRecordingPosition(event.currentTarget)
      }}
      onSeeked={(event) => {
        saveCurrentPosition(event.currentTarget)
        updateRecordingPosition(event.currentTarget)
        const pendingBoundaryMs = pendingBoundarySeekMsRef.current
        if (pendingBoundaryMs !== null) seekToOriginalBoundary(pendingBoundaryMs)
      }}
      onSeeking={(event) => {
        if (!isOriginalPlayback) return
        // seeking → timeupdate → seeked の順に来る環境では、保存位置からの再開が
        // cut 区間の途中でも timeupdate が「0 秒から区間へ入った」と誤認しないよう
        // seek 開始時に原本時間軸の直前位置を更新する。
        originalPreviousSecondsRef.current = sessionStartSeconds + event.currentTarget.currentTime
      }}
      onTimeUpdate={(event) => {
        if (mode === 'live' || chaseResumePending.current !== null) return
        const media = event.currentTarget
        const previousSeconds = originalPreviousSecondsRef.current
        updateRecordingPosition(media)
        if (!isOriginalPlayback) return
        const playAroundStop = playAroundStopRef.current
        if (playAroundStop !== null) {
          if (sessionStartSeconds + media.currentTime >= playAroundStop) {
            finishPlayAround(media)
          } else {
            schedulePlayAroundStop(media)
          }
        }
        if (chapterSkipEnabled && !chapterEditing && !isOriginalScrubbingRef.current && !media.paused) {
          const target = skipTarget(
            chapters ?? [],
            previousSeconds,
            sessionStartSeconds + media.currentTime,
            originalDurationSeconds,
          )
          if (target !== undefined) {
            seekRecording(autoSkipSeekSeconds(target, originalDurationSeconds))
            return
          }
        }
        // EVENT duration is only the current conversion edge. Auto-watch is valid
        // only after ENDLIST or `ended` finalizes the VOD.
        const finalLength = originalDurationSeconds || (media.duration + sessionStartSeconds)
        if (
          originalVODFinalized.current &&
          finalLength > 0 &&
          sessionStartSeconds + media.currentTime >= finalLength * 0.9
        ) saveCurrentPosition(media)
      }}
      onPlaying={(event) => {
        playedRef.current = true
        if (isOriginalPlayback) schedulePlayAroundStop(event.currentTarget)
      }}
      onPlay={() => frame.onPlay()}
      onPause={(event) => {
        saveCurrentPosition(event.currentTarget)
        frame.onPause()
      }}
      onVolumeChange={(event) => frame.onVolumeChange(event.currentTarget)}
      onEnded={(event) => {
        if (!isOriginalPlayback || (isRecordingHLS && !recordingHlsShowWatched)) return
        if (playAroundStopRef.current !== null) finishPlayAround(event.currentTarget)
        // ended は ENDLIST 済みの終端でだけ発火する前提で位置を消す。
        originalVODFinalized.current = true
        saveCurrentPosition(event.currentTarget)
      }}
      onRateChange={(event) => {
        if (!isRecordingPlayback) return
        const rate = event.currentTarget.playbackRate
        playbackRateRef.current = rate
        setActivePlaybackRateRef.current(rate)
        saveActivePlaybackRateRef.current(rate)
      }}
    />
  )

  const playerOverlay = (
    <>
      {loading && !error && (
        <div
          role="status"
          className="absolute inset-0 flex items-center justify-center text-sm text-muted-foreground"
        >
          読み込み中…
        </div>
      )}
      {error && (
        // 原本 VOD では操作の幕（z-10）より上に出す。幕の下だと 400px で「再読み込み」を押せない。
        <div
          className={cn(
            'absolute inset-0 flex flex-col items-center justify-center gap-2 px-4 text-center',
            isOriginalPlayback && 'z-20 bg-black',
          )}
        >
          <LiveErrorMessage error={error} errorMessages={strategy.errorMessages} />
          <button
            type="button"
            onClick={() => {
              if (isRecordingHLS) setManualRetryGeneration((generation) => generation + 1)
              else retryOffsetSession()
            }}
            className={cn(
              'rounded-md border px-3 py-1.5 text-sm transition-colors',
              // 原本 VOD は黒い枠の上に出すので、テーマに依らず映像の上の配色にする。
              isOriginalPlayback
                ? 'border-white/40 text-white hover:bg-white/15'
                : 'border-border text-foreground hover:bg-muted',
            )}
          >
            再読み込み
          </button>
        </div>
      )}
    </>
  )

  const menuProfile = playbackProfile ?? availableProfiles?.[0]?.name ?? ''
  const profileOptions = availableProfiles?.map(({ name, height, label }) => ({
    name,
    label: label ?? (height !== undefined && height > 0 ? `${name}（${height}p）` : name),
  }))
  const toolbarSlot = isLive ? (
    <>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-8 shrink-0 text-white hover:bg-white/15 hover:text-white"
        aria-label={frame.controls.isPlaying ? '一時停止' : '再生'}
        onClick={frame.controls.onTogglePlay}
      >
        {frame.controls.isPlaying ? <Pause className="size-4" /> : <Play className="size-4" />}
      </Button>
      {liveTimelineBar?.canStartOver && (
        <Button
          type="button"
          variant="ghost"
          size="icon"
          data-testid="live-start-over"
          aria-label="最初から"
          className="size-10 shrink-0 rounded-full bg-white/15 p-0 text-white hover:bg-white/20 md:h-8 md:w-auto md:px-3"
          onClick={onStartOver}
        >
          <SkipBack className="size-4" aria-hidden />
          <span className="hidden md:inline">最初から</span>
        </Button>
      )}
      <span data-testid="live-source-label" className="shrink-0 rounded bg-red-600 px-2 py-1 text-xs font-medium text-white">
        ● ライブ
      </span>
      {liveDiagnostics && (
        <span data-testid="live-diagnostics" className="hidden shrink-0 whitespace-nowrap text-xs text-white/80 md:inline">
          {liveDiagnostics}
        </span>
      )}
    </>
  ) : isChase && onReturnLive ? (
    <span data-testid="chase-source-label" className="shrink-0 rounded bg-white/15 px-2 py-1 text-xs font-medium text-white">
      ● 録画から再生中
    </span>
  ) : undefined
  const playbackControls = (
    <RecordingPlaybackControls
      recordingId={recordingId}
      profile={menuProfile}
      encodedAssets={[]}
      timeline={playbackAxis}
      className={className}
      liveNotice={isLive ? liveNotice : undefined}
      liveEdgeAction={chaseAxis && onReturnLive ? {
        label: 'ライブへ戻る',
        title: `ライブ ${formatPlaybackTime(chaseAxis.recordedEndSeconds, false)}（押すとライブへ戻る）`,
      } : undefined}
      toolbarSlot={toolbarSlot}
      showCenterControls={!isLive}
      mobileActionsPlacement={isLive ? 'row' : 'overlay'}
      settingsLabel={isLive ? 'ライブ設定' : '再生設定'}
      canChangePlaybackRate={!isLive}
      profileOptions={profileOptions}
      audioOptions={playbackAudioOptions}
      audioChoice={playbackAudio}
      onSelectAudio={(choice) => {
        if (onAudioChange) onAudioChange(choice)
        else if (isLive) setLiveAudioOverrideState({ set: true, value: choice })
        else if (isOriginalPlayback) setOriginalVODAudioOverrideState({ recordingId, set: true, value: choice })
      }}
      {...frame.controls}
      video={<>{video}{playerOverlay}</>}
      currentSeconds={currentAxisSeconds}
      durationSeconds={playbackDurationSeconds}
      playedFraction={originalPlayedFraction}
      chapters={chapterEditing ? [] : (chapters ?? [])}
      playingCut={false}
      chapterEditing={chapterEditing}
      canEditChapters={
        isOriginalPlayback && !chapterEditing && !chapterDetectionPending && chapterVersion !== undefined &&
        onSaveChapters !== undefined && onResetChapters !== undefined
      }
      onEnterChapterEditing={onEnterChapterEditing}
      onPlayAround={editorSelected === null ? undefined : () => playAround(editorSelected)}
      onPlayToBoundary={editorSelected === null ? undefined : () => playAround(editorSelected, 'to')}
      onPlayFromBoundary={editorSelected === null ? undefined : () => playAround(editorSelected, 'from')}
      seekTilesEnabled={isOriginalPlayback}
      requestSeekTilesOnMount={chapterEditing && isOriginalPlayback}
      onTileImageLoad={() => setOriginalTilesAvailable(true)}
      onTileImageError={() => {
        setOriginalTilesAvailable(false)
      }}
      commitLastDisplayedPreview={isChase}
      onScrubPreview={(seconds) => {
        updatePlaybackAxisState({ previewSeconds: seconds })
      }}
      onTimelineHoverChange={(seconds) => {
        updatePlaybackAxisState({ hoverSeconds: seconds })
      }}
      onScrubbingChange={(scrubbing) => {
        if (isOriginalPlayback) isOriginalScrubbingRef.current = scrubbing
      }}
      onSeek={(seconds) => {
        if (isLive) {
          commitLiveProgramSeek(seconds)
        } else {
          commitRecordingSeek(seconds)
        }
      }}
      onLiveEdgeSeek={chaseAxis ? onReturnLive ?? (() => commitRecordingSeek(chaseAxis.liveEdgeSeconds)) : undefined}
      deferKeyboardSeek
      onSeekPreview={(seconds) => {
        if (isLive) {
          updatePlaybackAxisState({ hoverSeconds: isSelectableLiveProgramPoint(seconds) ? seconds : null })
        } else updatePlaybackAxisState({ previewSeconds: seconds })
      }}
      onSelectProfile={(nextProfile) => {
        if (onProfileChange) onProfileChange(nextProfile)
        else if (isOriginalPlayback) setOriginalVODProfileOverrideState({ recordingId, value: nextProfile })
      }}
      onPreviousChapter={() => jumpOriginalChapter('prev')}
      onNextChapter={() => jumpOriginalChapter('next')}
      playbackRate={activePlaybackRate}
      subtitlesEnabled={originalSubtitlesEnabled}
      skipEnabled={chapterSkipEnabled}
      showWatched={isOriginalVOD || (isRecordingHLS && recordingHlsShowWatched === true)}
      watched={watched}
      watchedPending={watchedPending}
      onPutWatched={onPutWatched}
      onDeleteWatched={onDeleteWatched}
      onRateChange={(rate) => {
        const media = videoRef.current
        if (!media) return
        const applied = applyPlaybackRate(media, rate, saveActivePlaybackRate)
        playbackRateRef.current = applied
        setActivePlaybackRate(applied)
        saveActivePlaybackRate(applied)
      }}
      onToggleSubtitles={() => {
        const enabled = !originalSubtitlesEnabled
        if (hlsRef.current) hlsRef.current.subtitleDisplay = enabled
        setOriginalSubtitlesEnabled(enabled)
      }}
      onToggleSkip={(enabled) => {
        setChapterSkipEnabled(enabled)
        saveChapterSkip(enabled)
      }}
    />
  )
  const editorOpen =
    isOriginalPlayback && chapterEditing && chapterVersion !== undefined &&
    onSaveChapters !== undefined && onResetChapters !== undefined

  // 元 HLS の編集では保存時刻は録画先頭からの ms、編集画面の終端も recordingDurationMs
  // に固定する。変換中に <video>.duration が伸びても、既存境界や編集可能範囲は動かない。
  // controls と video は常に同じ木の位置に置き、編集開始で HLS セッションを張り直さない。
  return (
    <section
      className={editorOpen ? 'min-w-0' : 'contents'}
      aria-label={editorOpen ? 'チャプターを直す' : '再生'}
    >
      <div
        data-testid={editorOpen ? 'chapter-edit-layout' : undefined}
        className={editorOpen
          ? 'grid h-[calc(100dvh-var(--page-header-height,72px)-var(--sticky-banners-height,0px)-var(--bottom-nav-height,0px)-1rem)] min-h-0 grid-cols-1 grid-rows-[auto_auto_minmax(0,1fr)] gap-3 overflow-hidden md:h-auto md:grid-cols-[minmax(0,1.65fr)_minmax(20rem,0.9fr)] md:grid-rows-[auto_auto] md:overflow-visible'
          : 'contents'}
      >
        <div
          data-testid={editorOpen ? 'chapter-edit-player' : undefined}
          className={editorOpen ? 'min-w-0 md:col-start-1 md:row-start-1' : 'contents'}
        >
          {playbackControls}
        </div>
        {isOriginalPlayback && editorOpen && (
          <RecordingChapterEditor
            key={recordingId}
            spans={chapters ?? []}
            version={chapterVersion}
            detectionPending={false}
            source={chapterSource}
            recordingId={recordingId}
            currentSeconds={currentAxisSeconds}
            getDisplayedFrameSeconds={getDisplayedFrameSeconds}
            isPlaying={frame.mediaPlaying}
            durationSeconds={originalDurationSeconds}
            tilesAvailable={originalTilesAvailable}
            jumpTo={seekRecording}
            onBoundaryAction={selectChapterBoundary}
            onSelectedBoundaryChange={setEditorSelected}
            onSave={onSaveChapters}
            onReset={async () => await onResetChapters()}
            pending={chapterSavePending}
            commandsRef={resolvedChapterEditorCommandsRef}
            onStatusChange={chapterEditorStatusChange}
          />
        )}
      </div>
    </section>
  )
}

/**
 * LiveErrorMessage は再生 strategy が選んだエラー分類と文言を表示する。
 *
 * `unreachable` は赤（destructive）にしない --- ハイブリッド構成では
 * 自宅（streamer）が落ちているだけの正常状態でありうる（docs/overview.md
 * §サーバーレスデプロイ）。`capacity` / `other` の本文はそのまま見せる
 * （docs/frontend.md「エラーの本文も UI まで運ぶ」。400 を黙って隠さない、と同じ規律）。
 */
function LiveErrorMessage({
  error,
  errorMessages,
}: {
  error: LiveLoadError
  errorMessages: PlaybackErrorMessages
}) {
  if (error.kind === 'unreachable') {
    return (
      <p className="text-sm text-muted-foreground">
        録画サーバーの自宅側に接続できません。自宅サーバーが起動しているか、
        ネットワークが繋がっているかを確認してください。
      </p>
    )
  }
  if (error.kind === 'capacity') {
    return (
      <div className="text-sm text-destructive">
        <p>{errorMessages.capacity.title}</p>
        <p className="text-muted-foreground">{errorMessages.capacity.guidance}</p>
        {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
      </div>
    )
  }
  if (error.kind === 'chase-input') {
    return (
      <div className="text-sm text-destructive">
        <p>{error.title}</p>
        {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
      </div>
    )
  }
  return (
    <div className="text-sm text-destructive">
      <p>{errorMessages.otherTitle}</p>
      {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
    </div>
  )
}
