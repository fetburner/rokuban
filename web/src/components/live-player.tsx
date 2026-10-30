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
  createStallTracker,
  liveAudioTrackIndex,
  liveLeaveURL,
  livePlaylistURL,
  observeStall,
  probeLivePlaylist,
  readSubtitleVisibility,
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
import { useDisplayedFrameSeconds } from '@/lib/use-displayed-frame'
import { usePlayerFrame } from '@/lib/use-player-frame'
import { usePlaybackLeave } from '@/lib/use-playback-leave'
import { cn } from '@/lib/utils'
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
): string {
  return JSON.stringify([mode, site, networkId, serviceId, recordingId])
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
   * live playlist の画質（`live.profiles` の名前）。省略時は
   * streamer の先頭プロファイル（既定）。
   *
   * 再開位置とは独立している。画質切替でも同じ再生位置を引き継ぐ（issue #874）。
   *
   * ライブの切替はセッションを共有したまま playlist URL だけを替える。録画 HLS は
   * 選んだ generation の profile URL を読み直し、現在位置を持ち越す。
   */
  profile?: string
  /**
   * 音声（二重音声の主 / 副。issue #870）。省略時は標準トラック。
   *
   * **`profile` と違って HLS playlist の URL を変えない。** streamer が live と
   * recording HLS に 3 本の音声レンディションを出すので、切替はプレイヤーが取る
   * トラックを替えるだけで、playlist の取り直しもセッションの作り直しも起きない。
   */
  audio?: LiveAudioChoice
  /** 各モードの画質メニュー。1 件以下ならセレクタを隠す。 */
  availableProfiles?: readonly { name: string; height?: number; label?: string }[]
  onProfileChange?: (profile: string) => void
  onAudioChange?: (audio: LiveAudioChoice | undefined) => void
  /**
   * 最初の読み込みが終わったら再生を始める（ポスターの ▶ や再生元を替えて続きを
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
  /** recordings.id。録画 HLS の録画。 */
  recordingId: number
  /** true のあいだ、チャプター編集用の再生速度で再生する。 */
  chapterEditing?: boolean
  /** 録画先頭からの現在位置（秒）。再生元を選び直すとき親が持ち越す。 */
  onRecordingPositionChange?: (seconds: number) => void
  /**
   * 再生元がエラーを返した。位置は一度も再生していないセッションでは undefined（0 秒を
   * 「明示の位置」として渡さない）。true なら親が再生元を選び直した（エラー表示に落ちない）。
   */
  onRecordingPlaybackError?: (
    recordingPositionSeconds: number | undefined,
    wasPlaying: boolean,
    allowSameSourceRetry?: boolean,
  ) => boolean | Promise<boolean>
  recordingDurationMs: number
  chapters?: ChapterSpan[]
  /** 録画 HLS のチャプター状態と編集操作。 */
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
  /** 生放送の最初から視聴中に、ライブ配信へ戻る操作を出す。 */
  onReturnLive?: () => void
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
      onRecordingPositionChange?: never
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
  | (LivePlayerCommonProps & RecordingPlaybackProps & RecordingHLSProps & {
      mode: 'recording-hls'
      networkId?: never
      serviceId?: never
      liveProgram?: never
      onLiveProgramSeek?: never
      onStartOver?: never
      liveDiagnostics?: never
      liveNotice?: never
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
    fullscreenContainerRef,
    profile,
    audio,
    autoPlay = false,
    onRecordingPositionChange,
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
  const isRecordingHLS = mode === 'recording-hls'
  const isLive = mode === 'live'
  const recordingDuration = recordingDurationSeconds(recordingDurationMs)
  const strategy = playbackStrategy(props)
  const videoRef = useRef<HTMLVideoElement>(null)
  const frameRef = useRef<HTMLDivElement>(null)
  const hlsRef = useRef<HlsLike | null>(null)
  const isRecordingScrubbingRef = useRef(false)
  const initialRecordingPositionSeconds = recordingHlsStartSeconds ?? 0
  const resumePlaybackPendingRef = useRef(false)
  usePlaybackLeave(mode === 'live' ? liveLeaveURL(site, props.networkId, props.serviceId) : null)
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
  const [recordingHLSProfileOverrideState, setRecordingHLSProfileOverrideState] = useState<{
    recordingId: number | undefined
    value: string | undefined
  }>({ recordingId, value: undefined })
  const recordingHLSProfileOverride = recordingHLSProfileOverrideState.recordingId === recordingId
    ? recordingHLSProfileOverrideState.value
    : undefined
  const [recordingHLSAudioOverrideState, setRecordingHLSAudioOverrideState] = useState<{
    recordingId: number | undefined
    set: boolean
    value: LiveAudioChoice | undefined
  }>({ recordingId, set: false, value: undefined })
  const recordingHLSAudioOverride = recordingHLSAudioOverrideState.recordingId === recordingId
    ? recordingHLSAudioOverrideState
    : { recordingId, set: false, value: undefined }
  const [liveAudioOverrideState, setLiveAudioOverrideState] = useState<{
    set: boolean
    value: LiveAudioChoice | undefined
  }>({ set: false, value: undefined })
  const playbackProfile = isRecordingHLS
    ? onProfileChange ? profile : recordingHLSProfileOverride ?? profile
    : profile
  const displayedFrameVideoKey = `${recordingId}:${playbackProfile ?? ''}`
  const getDisplayedFrameSeconds = useDisplayedFrameSeconds(
    videoRef,
    chapterEditing && isRecordingHLS,
    displayedFrameVideoKey,
  )
  const [editorSelected, setEditorSelected] = useState<number | null>(null)
  const localChapterEditorCommandsRef = useRef<ChapterEditorCommands | null>(null)
  const resolvedChapterEditorCommandsRef = chapterEditorCommandsRef ?? localChapterEditorCommandsRef
  const playbackAudio = isRecordingHLS && !onAudioChange && recordingHLSAudioOverride.set
    ? recordingHLSAudioOverride.value
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
  )
  const hasCurrentPlaybackAxisState = playbackAxisState.identity === playbackAxisIdentity
  const playbackHoverSeconds = hasCurrentPlaybackAxisState ? playbackAxisState.hoverSeconds : null
  const playbackAxis = strategy.axis(playbackHoverSeconds)
  const liveTimelineBar = playbackAxis?.kind === 'live-program' ? playbackAxis : undefined
  const initialAxisPositionSeconds = initialRecordingPositionSeconds
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
  const [recordingTilesAvailable, setRecordingTilesAvailable] = useState(false)
  const playAroundStopRef = useRef<number | null>(null)
  const playAroundStopBoundaryMsRef = useRef<number | null>(null)
  const playAroundTimerRef = useRef<number | undefined>(undefined)
  const pendingBoundarySeekMsRef = useRef<number | null>(null)
  const [recordingSubtitlesEnabled, setRecordingSubtitlesEnabled] = useState(isRecordingHLS)
  const [chapterSkipEnabled, setChapterSkipEnabled] = useState(loadChapterSkip)
  const chapterEditorStatusChange = onChapterEditorStatusChange ?? (() => {})
  const previousRecordingSecondsRef = useRef(0)
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
  // 録画 HLS の再生位置を画質の切替で持ち越す（issue #874）。
  //
  // プロファイルを替えると HLS URL と media element を読み直すため、切替直前の
  // 録画時間を持ち越して再開する。録画 ID が変わった場合は持ち越さない。
  const lastRecordingHLSPositionRef = useRef<number | null>(null)
  const lastRecordingHLSInputsRef = useRef<string | null>(null)
  // A generation-changing error can pause/reset the media element when hls.js is destroyed.
  // Save the error-time snapshot before handing control to the caller; effect cleanup consumes it.
  const pendingRecordingHLSRecoveryRef = useRef<{
    recordingId: number | undefined
    positionSeconds: number | undefined
    wasPlaying: boolean
  } | null>(null)
  // 持ち越した位置へまだ戻し終えていない間は位置を保存しない。ソースの張り直し中に
  // 位置 0 の `timeupdate` が届いて保存位置を上書きするのを防ぐ。
  const recordingResumePending = useRef<number | null>(null)
  // 今の読み込みが `loadedmetadata` に届いたか。**届く前の要素の位置（0）は
  // 持ち越さない** --- probe 中（ffmpeg の起動待ちで最長 15 秒）に切り替えると、
  // 0 を持ち越して既存の復元を潰し、保存位置が消える。jsdom の `readyState`
  // は動かないので、要素の状態ではなく自前で持つ。
  const recordingMetadataLoaded = useRef(false)
  const watchedRequestPending = useRef(false)
  /**
   * 録画 HLS playlist が ENDLIST まで書かれたか。変換中の EVENT playlist の
   * `video.duration` は変換の先端でしかないので、これが true になるまで
   * 「終端付近」の判定に duration を渡さない（渡すと先端付近で位置が消える）。
   * 信号は hls.js の LEVEL_LOADED の `details.live === false`（ENDLIST あり）と
   * `ended` イベントである。
   */
  const recordingHLSFinalized = useRef(false)
  const saveCurrentPosition = useCallback((video: HTMLVideoElement, keepalive = false) => {
    if (mode === 'live' || recordingResumePending.current !== null) return
    const globalPosition = video.currentTime
    const knownFinalDuration = recordingHLSFinalized.current
      ? recordingDuration || video.duration
      : video.duration
    const write = playbackPositionWrite(
      globalPosition,
      knownFinalDuration,
      isRecordingHLS && recordingHLSFinalized.current,
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
  }, [isRecordingHLS, mode, recordingDuration, recordingId])

  // Shared frame behavior lives here. The playback source supplies its own seek semantics and
  // HLS subtitle-track classification; live has no recording seek callbacks.
  const frame = usePlayerFrame(videoRef, frameRef, undefined, {
    fullscreenContainerRef,
    // commitRecordingSeek は後方の const。usePlayerFrame がコールバックを ref に入れ直し、
    // キー押下（render 後）にだけ呼ぶので、宣言前でも TDZ にならない。
    onSeekBy: strategy.recording
      ? (seconds) => {
          const video = videoRef.current
          if (!video) return
          commitRecordingSeek(video.currentTime + seconds)
        }
      : undefined,
    onSeekToFraction: isRecordingHLS && recordingDuration > 0
      ? (fraction) => {
          seekRecording(recordingDuration * fraction)
          return true
        }
      : undefined,
    onSavePosition: isRecordingHLS ? saveCurrentPosition : undefined,
    savePositionKey: `${mode}:${recordingId}`,
    getSubtitleTracks: hlsSubtitleTracks,
    subtitleState: recordingSubtitlesEnabled,
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
    // **`profile` を依存に入れない（issue #874）。** 入れると画質切替のたびに
    // 復元が立ち直り、切替が保存位置まで巻き戻る。位置の持ち越しは下の effect が
    // `lastRecordingHLSPositionRef` で行う。
    // 画質の切替は再生位置の基準を変えないので、復元をやり直す理由が無い。
    site,
    networkId,
    serviceId,
    isRecordingHLS,
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
    const rate = effectivePlaybackRate(activePlaybackRate, isRecordingHLS)
    const appliedRate = applyPlaybackRate(video, rate, saveActivePlaybackRate)
    if (isRecordingHLS && appliedRate !== activePlaybackRate) {
      playbackRateRef.current = appliedRate
      setActivePlaybackRate(appliedRate)
    }
  }, [activePlaybackRate, chapterEditing, isRecordingHLS, saveActivePlaybackRate, setActivePlaybackRate, videoRef])

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
    // 画質（プロファイル）だけが変わった再実行か（`lastRecordingHLSPositionRef` の
    // コメント参照）。プロファイルを含めない入力の同一性で判定する。
    const recordingInputs = `${mode}|${site}|${recordingId}`
    const resumePosition =
      isRecordingHLS && lastRecordingHLSInputsRef.current === recordingInputs
        ? lastRecordingHLSPositionRef.current
        : null
    lastRecordingHLSInputsRef.current = recordingInputs
    recordingHLSFinalized.current = false
    // 録画 HLS は再開位置を hls.js の開始位置に渡す。位置が無いときは 0 秒から始める。
    const startAt = isRecordingHLS ? (resumePosition ?? recordingHlsStartSeconds ?? 0) : null
    recordingResumePending.current = startAt
    if (startAt !== null) {
      // 持ち越しは既存の復元より優先する。サーバーの再開位置が更新されて復元が
      // 立ち直っていると、`onLoadedMetadata` が保存位置へ戻してしまう。
      restorePending.current = false
    }
    // 同じ録画の中で張り直した（セッション外へのシーク・画質の切替・416 の丸め）ときは再生を
    // 引き継ぐ。`load()` は pause を発火しないので、引き継がないときは操作バーの状態を戻す。
    // 別の録画へ替わったときは勝手に再生を始めない。ライブ（`recordingId` を持たない）から
    // 録画 HLS へ替えたとき（ライブページの「最初から」・軸のシーク）は同じ番組なので引き継ぐ。
    // 自動再生の指定は最初のセッションにだけ効く。張り直し前に消費しても、再開待ちの
    // `resumePlaybackPendingRef` が次のセッションへ持ち越す（下の `preservedState`）。
    const autoPlayNow = isRecordingHLS && autoPlayPendingRef.current
    autoPlayPendingRef.current = false
    const resumePlaying =
      (preserved?.playing === true &&
        (!isRecordingHLS || preserved.recordingId === undefined || preserved.recordingId === recordingId)) ||
      autoPlayNow
    resumePlaybackPendingRef.current = resumePlaying
    if (isRecordingHLS && !resumePlaying) setMediaPlaying(false)
    // video / hls の外部再生状態と UI の loading/error 表示を同期する effect。
    // render 中に導出すると、再生開始・失敗イベントの境界を表現できない。
    // oxlint-disable-next-line react/set-state-in-effect -- 別ソースの読込状態を setup ごとに初期化する
    setLoading(true)
    setError(null)
    onDiagnosticsRef.current?.(null)

    const url = mode === 'recording-hls'
      ? recordingHlsUrl
      : livePlaylistURL(site, networkId, serviceId, playbackProfile)

    /**
     * handOffError は録画再生のエラーを親に渡し、親が再生元を選び直したら true を返す
     * （このプレイヤーはエラー表示に落ちない）。一度も再生していないセッションのエラーは
     * 位置を渡さない --- 0 秒を「明示の位置」にすると、親が持つ保存位置・先頭からの意図を上書きする。
     * 要求した開始位置が適用中の間も currentTime は位置として使わない。seek 失敗で
     * 誤った秒を作り直しへ渡さない（`recording-playback-timeline.mjs`）。
     * 再生の意図は、始まる前なら再開待ち（▶ の autoPlay・張り直しの持ち越し）だけで決める。
     * 「再生していない」ことを再生の意図にすると、止めたまま移った先のエラーで勝手に再生が始まる
     * （両方向は `live-player.test.tsx` の「再生前のエラー」）。
     */
    const handOffError = (
      media: HTMLVideoElement | null,
      allowSameSourceRetry = true,
    ): boolean | Promise<boolean> => {
      if (!isRecordingHLS) return false
      const recordingHlsPositionApplied = !isRecordingHLS || recordingResumePending.current === null
      const position = playedRef.current && media
        ? (recordingHlsPositionApplied ? media.currentTime : undefined)
        : undefined
      const wasPlaying = media ? !media.paused || resumePlaybackPendingRef.current : true
      const recovery = { recordingId, positionSeconds: position, wasPlaying }
      pendingRecordingHLSRecoveryRef.current = recovery
      const clearUnclaimedRecovery = (handled: boolean) => {
        if (!handled && pendingRecordingHLSRecoveryRef.current === recovery) {
          pendingRecordingHLSRecoveryRef.current = null
        }
        return handled
      }
      const handedOff = onRecordingPlaybackErrorRef.current?.(position, wasPlaying, allowSameSourceRetry) ?? false
      return typeof handedOff === 'boolean'
        ? clearUnclaimedRecovery(handedOff)
        : handedOff.then(clearUnclaimedRecovery, (error) => {
          clearUnclaimedRecovery(false)
          throw error
        })
    }

    // teardown はこの effect が張ったものを外す手続き（メディアイベントの
    // リスナと stall 監視のタイマー）。cleanup から呼ぶ
    const teardown: Array<() => void> = []

    recordingMetadataLoaded.current = false
    const markLoaded = () => {
      recordingMetadataLoaded.current = true
    }
    if (video && isRecordingHLS) {
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
        if (recordingResumePending.current !== null) apply()
        if (cancelled) return
        recordingResumePending.current = null
        if (isRecordingHLS) {
          // cancelled を見たあとなので、state の identity はこの effect のもの。
          const positionSeconds = startAt
          setPlaybackAxisState((current) => ({ ...current, positionSeconds }))
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
      if (!probe.ok) {
        const handedOff = await handOffError(video)
        if (cancelled || handedOff) return
        // 世代付き録画 HLS は、親が次の世代の URL を渡すとこの effect が読み直す。再生の意図を
        // 残し、利用者の操作なしで再生を続ける（`recording-detail.test.tsx` の「エラー表示に落ちた後でも」）。
        resumePlaybackPendingRef.current = false
        if (isRecordingHLS) setMediaPlaying(false)
        setError(probe.error)
        setLoading(false)
        return
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
            if (isRecordingHLS) setMediaPlaying(false)
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
      // 録画 HLS では自動降格しない。ライブの `onStalled` は視聴ページに配線される。
      const canDowngrade = !probe.bundlesProfiles && !isRecordingHLS
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
      // hls.js otherwise chooses the live edge for an EVENT playlist. Recording HLS
      // starts at the requested recording second within the generation's playlist.
      const hls = new Hls(
        isRecordingHLS ? { startPosition: startAt ?? 0 } : undefined,
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
      if (isRecordingHLS) {
        hls.on(Hls.Events.LEVEL_LOADED, (_event, data) => {
          if (data.details?.live === false) recordingHLSFinalized.current = true
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
        const recovery = pendingRecordingHLSRecoveryRef.current
        const recoveryMatchesRecording = isRecordingHLS && recovery?.recordingId === recordingId
        preservedState.current = {
          subtitles: readSubtitleVisibility(Array.from(video.textTracks)),
          // 切替の直前（= cleanup の時点）の再生状態。`load()` は paused を
          // true に戻すので、これを見ないと切替のたびに利用者が押し直すことになる
          // 再開を待っている間（canplay の前）に張り直したときも、再生の意図を引き継ぐ。
          playing: recoveryMatchesRecording
            ? recovery.wasPlaying
            : !video.paused || resumePlaybackPendingRef.current,
          recordingId,
        }
        // 画質の切替で持ち越す再生位置（issue #874）。**`src` を外す前に読む。**
        // 前の持ち越しを戻し終える前にもう一度切り替えたときは、要素の位置
        // （まだ 0）ではなく持ち越し中の位置を引き継ぐ。
        lastRecordingHLSPositionRef.current = isRecordingHLS
          ? (recoveryMatchesRecording && recovery.positionSeconds !== undefined
            ? recovery.positionSeconds
            : recordingResumePending.current ??
              (recordingMetadataLoaded.current ? video.currentTime : null))
          : null
        if (recoveryMatchesRecording) pendingRecordingHLSRecoveryRef.current = null
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
    isRecordingHLS,
    mode,
    recordingHlsUrl,
    recordingHlsStartSeconds,
    manualRetryGeneration,
    playbackProfile,
    recordingId,
    site,
    networkId,
    serviceId,
    resumePlaybackPendingRef,
    setMediaPlaying,
    strategy.classifyLoadError,
    strategy.errorMessages.fatalTitle,
    strategy.errorMessages.unsupported,
  ])

  const currentAxisSeconds = liveTimelineBar?.liveEdgeSeconds ??
    playbackAxisForRender.previewSeconds ?? playbackAxisForRender.positionSeconds
  const recordingPlayedFraction = recordingDuration > 0
    ? Math.max(0, Math.min(1, playbackAxisForRender.positionSeconds / recordingDuration))
    : 0
  // ドラッグ中・キー操作中だけ位置のプレビューでつまみと時刻を動かす。マウスのホバーは吹き出しだけ。
  const playbackDurationSeconds = liveTimelineBar?.plannedEndSeconds ?? playbackAxis?.maxSeconds ?? 0
  const updateRecordingPosition = (video: HTMLVideoElement) => {
    if (mode === 'live') return
    if (positionReportBlockedRef.current) return
    if (isRecordingHLS && recordingResumePending.current !== null) return
    const seconds = video.currentTime
    if (isRecordingHLS) previousRecordingSecondsRef.current = seconds
    updatePlaybackAxisState({ positionSeconds: seconds })
    onRecordingPositionChange?.(video.currentTime)
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
  /** seekRecording は録画先頭からの秒を HLS の seekable 範囲へ丸めて移る。 */
  const seekRecording = (seconds: number) => {
    const video = videoRef.current
    const playlistEndSeconds = video ? mediaSeekableEndSeconds(video) : undefined
    if (playlistEndSeconds === undefined) return
    const target = strategy.recording?.seekTarget(seconds, playlistEndSeconds)
    if (target === undefined) return
    pendingBoundarySeekMsRef.current = null
    previousRecordingSecondsRef.current = target
    if (!video) return
    video.currentTime = target
    recordingResumePending.current = null
    updatePlaybackAxisState({ positionSeconds: target })
  }
  /** commitRecordingSeek はシークバー・キー操作の確定。位置のプレビューを消してから移る。 */
  const commitRecordingSeek = (seconds: number) => {
    updatePlaybackAxisState({ previewSeconds: null })
    seekRecording(seconds)
  }
  const seekToRecordingBoundary = (boundaryMs: number) => {
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
    // a recording HLS source is waiting for its canplay auto-resume.
    resumePlaybackPendingRef.current = false
    if (!media.paused) media.pause()
    seekToRecordingBoundary(Math.round(seconds * 1000))
  }
  const finishPlayAround = (media: HTMLVideoElement) => {
    const boundaryMs = playAroundStopBoundaryMsRef.current
    clearPlayAround()
    media.pause()
    if (boundaryMs !== null) seekToRecordingBoundary(boundaryMs)
  }
  const schedulePlayAroundStop = (media: HTMLVideoElement) => {
    const stop = playAroundStopRef.current
    if (stop === null || media.paused) return
    window.clearTimeout(playAroundTimerRef.current)
    const remainingSeconds = Math.max(0, stop - media.currentTime)
    // HLS の起動待ちで境界を失わないよう、playing/timeupdate 後に残り時間を測り、
    // タイマー発火時にも位置を再確認する。
    playAroundTimerRef.current = window.setTimeout(() => {
      if (playAroundStopRef.current !== stop) return
      if (media.currentTime >= stop) {
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
  const jumpRecordingChapter = (direction: 'next' | 'prev') => {
    const target = chapterJumpTarget(chapters ?? [], playbackAxisForRender.positionSeconds, direction)
    if (target !== undefined) seekRecording(chapterBoundaryMsToSeekSeconds(Math.round(target * 1000)))
  }
  // 再開位置は録画時間軸で API に保存する。`profile` は映像品質の選択だけに使う。
  const video = (
    <video
      ref={videoRef}
      controls={false}
      playsInline
      aria-label={isRecordingHLS ? '録画映像' : undefined}
      {...frame.video}
      className={cn(
        'absolute inset-0 size-full rounded object-contain',
        (loading || error) && 'invisible',
      )}
      onLoadedMetadata={(event) => {
        frame.onVolumeChange(event.currentTarget)
        // 録画 HLS は保存位置へ戻す前の位置を報告しない（再試行の上限が効かなくなる）。
        if (isRecordingHLS) updateRecordingPosition(event.currentTarget)
        if (mode === 'live' || !restorePending.current) return
        restorePending.current = false
        event.currentTarget.currentTime = initialRecordingPositionSeconds
        updateRecordingPosition(event.currentTarget)
      }}
      onSeeked={(event) => {
        saveCurrentPosition(event.currentTarget)
        updateRecordingPosition(event.currentTarget)
        const pendingBoundaryMs = pendingBoundarySeekMsRef.current
        if (pendingBoundaryMs !== null) seekToRecordingBoundary(pendingBoundaryMs)
      }}
      onSeeking={(event) => {
        if (!isRecordingHLS) return
        // seeking → timeupdate → seeked の順に来る環境では、保存位置からの再開が
        // cut 区間の途中でも timeupdate が「0 秒から区間へ入った」と誤認しないよう
        // seek 開始時に直前位置を更新する。
        previousRecordingSecondsRef.current = event.currentTarget.currentTime
      }}
      onTimeUpdate={(event) => {
        if (mode === 'live' || recordingResumePending.current !== null) return
        const media = event.currentTarget
        const previousSeconds = previousRecordingSecondsRef.current
        updateRecordingPosition(media)
        if (!isRecordingHLS) return
        const playAroundStop = playAroundStopRef.current
        if (playAroundStop !== null) {
          if (media.currentTime >= playAroundStop) {
            finishPlayAround(media)
          } else {
            schedulePlayAroundStop(media)
          }
        }
        if (chapterSkipEnabled && !chapterEditing && !isRecordingScrubbingRef.current && !media.paused) {
          const target = skipTarget(
            chapters ?? [],
            previousSeconds,
            media.currentTime,
            recordingDuration,
          )
          if (target !== undefined) {
            seekRecording(autoSkipSeekSeconds(target, recordingDuration))
            return
          }
        }
        // EVENT duration is only the current conversion edge. Auto-watch is valid
        // only after ENDLIST or `ended` finalizes the VOD.
        const finalLength = recordingDuration || media.duration
        if (
          recordingHLSFinalized.current &&
          finalLength > 0 &&
          media.currentTime >= finalLength * 0.9
        ) saveCurrentPosition(media)
      }}
      onPlaying={(event) => {
        playedRef.current = true
        if (isRecordingHLS) schedulePlayAroundStop(event.currentTarget)
      }}
      onPlay={() => frame.onPlay()}
      onPause={(event) => {
        saveCurrentPosition(event.currentTarget)
        frame.onPause()
      }}
      onVolumeChange={(event) => frame.onVolumeChange(event.currentTarget)}
      onEnded={(event) => {
        if (!isRecordingHLS || !recordingHlsShowWatched) return
        if (playAroundStopRef.current !== null) finishPlayAround(event.currentTarget)
        // ended は ENDLIST 済みの終端でだけ発火する前提で位置を消す。
        recordingHLSFinalized.current = true
        saveCurrentPosition(event.currentTarget)
      }}
      onRateChange={(event) => {
        if (!isRecordingHLS) return
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
        // 録画 HLS では操作の幕（z-10）より上に出す。幕の下だと 400px で「再読み込み」を押せない。
        <div
          className={cn(
            'absolute inset-0 flex flex-col items-center justify-center gap-2 px-4 text-center',
            isRecordingHLS && 'z-20 bg-black',
          )}
        >
          <LiveErrorMessage error={error} errorMessages={strategy.errorMessages} />
          <button
            type="button"
            onClick={() => {
              setManualRetryGeneration((generation) => generation + 1)
            }}
            className={cn(
              'rounded-md border px-3 py-1.5 text-sm transition-colors',
              // 録画 HLS は黒い枠の上に出すので、テーマに依らず映像の上の配色にする。
              isRecordingHLS
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
      {liveTimelineBar?.canStartOver && onStartOver && (
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
  ) : (
    <>
      <span data-testid="recording-hls-source-label" className="shrink-0 rounded bg-white/15 px-2 py-1 text-xs font-medium text-white">
        ● 録画 HLS
      </span>
      {onReturnLive && (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          data-testid="recording-hls-return-live"
          onClick={onReturnLive}
          className="shrink-0 text-white hover:bg-white/15 hover:text-white"
        >
          ライブへ戻る
        </Button>
      )}
    </>
  )
  const playbackControls = (
    <RecordingPlaybackControls
      recordingId={recordingId}
      profile={menuProfile}
      encodedAssets={[]}
      timeline={playbackAxis}
      className={className}
      liveNotice={isLive ? liveNotice : undefined}
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
        else if (isRecordingHLS) setRecordingHLSAudioOverrideState({ recordingId, set: true, value: choice })
      }}
      {...frame.controls}
      video={<>{video}{playerOverlay}</>}
      currentSeconds={currentAxisSeconds}
      durationSeconds={playbackDurationSeconds}
      playedFraction={recordingPlayedFraction}
      chapters={chapterEditing ? [] : (chapters ?? [])}
      playingCut={false}
      chapterEditing={chapterEditing}
      canEditChapters={
        isRecordingHLS && !chapterEditing && !chapterDetectionPending && chapterVersion !== undefined &&
        onSaveChapters !== undefined && onResetChapters !== undefined
      }
      onEnterChapterEditing={onEnterChapterEditing}
      onPlayAround={editorSelected === null ? undefined : () => playAround(editorSelected)}
      onPlayToBoundary={editorSelected === null ? undefined : () => playAround(editorSelected, 'to')}
      onPlayFromBoundary={editorSelected === null ? undefined : () => playAround(editorSelected, 'from')}
      seekTilesEnabled={isRecordingHLS}
      requestSeekTilesOnMount={chapterEditing && isRecordingHLS}
      onTileImageLoad={() => setRecordingTilesAvailable(true)}
      onTileImageError={() => {
        setRecordingTilesAvailable(false)
      }}
      onScrubPreview={(seconds) => {
        updatePlaybackAxisState({ previewSeconds: seconds })
      }}
      onTimelineHoverChange={(seconds) => {
        updatePlaybackAxisState({ hoverSeconds: seconds })
      }}
      onScrubbingChange={(scrubbing) => {
        if (isRecordingHLS) isRecordingScrubbingRef.current = scrubbing
      }}
      onSeek={(seconds) => {
        if (isLive) {
          commitLiveProgramSeek(seconds)
        } else {
          commitRecordingSeek(seconds)
        }
      }}
      deferKeyboardSeek
      onSeekPreview={(seconds) => {
        if (isLive) {
          updatePlaybackAxisState({ hoverSeconds: isSelectableLiveProgramPoint(seconds) ? seconds : null })
        } else updatePlaybackAxisState({ previewSeconds: seconds })
      }}
      onSelectProfile={(nextProfile) => {
        if (onProfileChange) onProfileChange(nextProfile)
        else if (isRecordingHLS) setRecordingHLSProfileOverrideState({ recordingId, value: nextProfile })
      }}
      onPreviousChapter={() => jumpRecordingChapter('prev')}
      onNextChapter={() => jumpRecordingChapter('next')}
      playbackRate={activePlaybackRate}
      subtitlesEnabled={recordingSubtitlesEnabled}
      skipEnabled={chapterSkipEnabled}
      showWatched={isRecordingHLS && recordingHlsShowWatched === true}
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
        const enabled = !recordingSubtitlesEnabled
        if (hlsRef.current) hlsRef.current.subtitleDisplay = enabled
        setRecordingSubtitlesEnabled(enabled)
      }}
      onToggleSkip={(enabled) => {
        setChapterSkipEnabled(enabled)
        saveChapterSkip(enabled)
      }}
    />
  )
  const editorOpen =
    isRecordingHLS && chapterEditing && chapterVersion !== undefined &&
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
        {isRecordingHLS && editorOpen && (
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
            durationSeconds={recordingDuration}
            tilesAvailable={recordingTilesAvailable}
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
  return (
    <div className="text-sm text-destructive">
      <p>{errorMessages.otherTitle}</p>
      {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
    </div>
  )
}
