import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type FocusEvent as ReactFocusEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'

import type { ChapterSpan, EncodedAsset, KeepRange, RecordingChaptersSource } from '@/api/generated'
import { DetailSummary } from '@/components/detail-heading'
import { RecordingChapterEditor } from '@/components/recording-chapter-editor'
import { RecordingPlaybackControls } from '@/components/recording-playback-controls'
import { Button } from '@/components/ui/button'
import {
  PLAY_AROUND_SECONDS,
  chapterJumpTarget,
  loadChapterSkip,
  saveChapterSkip,
  skipTarget,
} from '@/lib/chapters'
import {
  applyPlaybackRate,
  clearLegacyPlaybackPositions,
  loadPlaybackRate,
  playbackPositionWrite,
  playbackResumeSeconds,
  persistPlaybackPosition,
  recordingFileURL,
  recordingSubtitleURL,
  savePlaybackRate,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'
import {
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileAt,
} from '@/lib/seek-tiles'

type RecordingPlayerProps = {
  recordingId: number
  resumePositionMs?: number
  /** 90% 到達の視聴済み PUT が通った後に呼ぶ（親が録画クエリを取り直してボタンと未視聴の印を更新する）。 */
  onWatched?: () => void
  /** 視聴済みボタンを出す完了録画かどうか。 */
  showWatched?: boolean
  /** 現在の視聴状態。 */
  watched?: boolean
  /** 視聴済みボタンの mutation 中かどうか。 */
  watchedPending?: boolean
  /** 視聴済みにする API 操作。 */
  putWatched?: () => void
  /** 未視聴に戻す API 操作。 */
  deleteWatched?: () => void
  /** 追っかけ再生と揃えるVOD側の既定プロファイル。資産に無ければ先頭を使う。 */
  preferredProfile?: string
  /**
   * 再生可能な encoded 派生物（active media_assets）。空ならプレイヤーを出さない。
   * `sizeBytes` が省略された要素も**選択肢そのものは隠さない**（M7-3 の値札
   * 方針: サイズが取れないという分類の失敗で機能を隠さない。ドロップ統計の
   * 「分類できなかった PID」と同じ判断。docs/frontend/recordings.md）。
   */
  encodedAssets: EncodedAsset[]
  /** 原本 TS があるとき VLC 向けリンクを出す。 */
  hasOriginal?: boolean
  /**
   * 有効なチャプターの区間（`GET /api/recordings/{id}/chapters` の結果そのまま）。
   * **本編の区間は含まれない** --- 区間の隙間が本編で、終端は `<video>.duration`
   * で閉じる。undefined は未取得（目盛りも一覧も出さない）。
   */
  chapters?: ChapterSpan[]
  /** どの層を読んだか。編集 UI の「確認済み / 未確認」表示に使う。 */
  chapterSource?: RecordingChaptersSource
  /** `chapters` の版。保存時にそのまま返す。 */
  chapterVersion?: string
  /** 検出中。編集 UI を出さずに理由を表示する。 */
  chapterDetectionPending?: boolean
  /**
   * タイムライン全体の保存。undefined なら編集 UI を出さない（エンコードが無い
   * 録画・ごみ箱など。呼び出し側が判断して渡す）。
   */
  onSaveChapters?: (spans: ChapterSpan[], version: string) => Promise<unknown>
  /** 所有を捨てて自動層へ戻す。 */
  onResetChapters?: () => void
  /** 保存 / 取り消しの実行中。 */
  chapterSavePending?: boolean
  /**
   * カット版を作り直す（`encodedAssets[].cutStale` が真のときだけ出す）。
   * undefined ならボタンを出さない。
   */
  onReencode?: (profile: string) => void
  /** 作り直しの投入中。 */
  reencodePending?: boolean
  className?: string
}

/**
 * RecordingPlayer は encoded 派生物を video 要素で再生し、自前の操作バーを重ねる。
 * MP4 progressive + Range（streamer）。再開位置と視聴済み状態は API で世帯共有する。
 */
export function RecordingPlayer({
  recordingId,
  resumePositionMs,
  onWatched,
  showWatched = false,
  watched = false,
  watchedPending = false,
  putWatched,
  deleteWatched,
  preferredProfile,
  encodedAssets,
  hasOriginal = false,
  chapters,
  chapterSource = 'auto',
  chapterVersion,
  chapterDetectionPending = false,
  onSaveChapters,
  onResetChapters,
  chapterSavePending = false,
  onReencode,
  reencodePending = false,
  className,
}: RecordingPlayerProps) {
  // `encodedAssets` の参照が変わらない限り再計算しない --- 素の `.map()` だと
  // 毎レンダーで新しい配列になり、下の useEffect の依存配列がレンダーごとに
  // 変化したと判定されて毎回走ってしまう（中身は冪等で setProfile を呼ばない
  // 限りループにはならないが、無駄な再実行を避ける）。
  const profiles = useMemo(() => encodedAssets.map((a) => a.profile), [encodedAssets])
  const [profile, setProfile] = useState(
    preferredProfile !== undefined && profiles.includes(preferredProfile)
      ? preferredProfile
      : (profiles[0] ?? ''),
  )
  // props の資産一覧が更新されて選択中プロファイルが消えた場合は、effect で一度
  // 無効な値を描いてから直すのではなく、表示値をその場で先頭へ導出する。
  const selectedProfile = profiles.includes(profile) ? profile : (profiles[0] ?? '')
  const selectedAsset = encodedAssets.find((a) => a.profile === selectedProfile)
  // カット版を再生しているあいだは、原本の時間軸で作られたものを一切出さない。
  // シークタイルは原本の時間軸で作られており、本編に残した OP などをカット版の
  // 軸へ写像する処理を初版では持たない。チャプターの目盛り・一覧・スキップも
  // 同じ理由で出さない（境界は原本の ms で、その動画には当てられない）。
  const playingCut = selectedAsset?.cut === true
  const keepRangesKey = JSON.stringify(selectedAsset?.keepRanges ?? [])
  const [playbackRate, setPlaybackRate] = useState(loadPlaybackRate)
  const videoRef = useRef<HTMLVideoElement>(null)
  const fullscreenRef = useRef<HTMLDivElement>(null)
  const isScrubbingRef = useRef(false)
  const jumpToRef = useRef<(seconds: number) => void>(() => {})
  const controlsTimerRef = useRef<number | undefined>(undefined)
  // 映像を押したポインタの種類。タッチは再生 / 一時停止ではなく操作の表示に使う（スマホの定石）。
  const videoPointerTypeRef = useRef<string>('')
  const chapterDetailsRef = useRef<HTMLDetailsElement>(null)
  const subtitleLinesRef = useRef(new WeakMap<VTTCue, VTTCue['line']>())
  const [mediaPlaying, setMediaPlaying] = useState(false)
  const [muted, setMuted] = useState(false)
  const [volume, setVolume] = useState(1)
  const [subtitlesEnabled, setSubtitlesEnabled] = useState(false)
  const [pictureInPicture, setPictureInPicture] = useState(false)
  const [isFullscreen, setIsFullscreen] = useState(false)
  const [controlsVisible, setControlsVisible] = useState(true)
  const [toolbarFocused, setToolbarFocused] = useState(false)
  // タイルは録画ごとに 1 枚で profile に依存しないので、キーは recordingId だけ。
  const [tilesRequestedFor, setTilesRequestedFor] = useState<number | null>(null)
  const [tilesAvailableFor, setTilesAvailableFor] = useState<number | null>(null)
  // 親は録画を切り替えてもこのコンポーネントを作り直さない（key が無い）。そのため
  // 帯の状態は recordingId と組で持ち、描くときに今の録画のものだけを使う。
  const [tilePreview, setTilePreview] = useState<{
    recordingId: number
    x: number
    y: number
    left: number
    scale: number
    seconds: number
  } | null>(null)
  // スクラブ帯の再生済み割合（0..1）・現在位置（秒）・タイムラインの終端
  // （`<video>.duration`）。timeupdate / seeked / loadedmetadata で更新する。
  // **編集 UI が現在位置を要る**ので同じ state に載せる（4Hz の再描画はこの
  // 1 か所に集約する）。録画を切り替えた直後に前の録画の値を描かないよう、
  // 録画 ID と組で持ち、描くときに今の録画のものだけを使う（`tilePreview` と
  // 同じ規律。effect で 0 に戻すとその 1 レンダーぶん古い値が見える）。
  const [played, setPlayed] = useState<{
    recordingId: number
    fraction: number
    seconds: number
    duration: number
  } | null>(null)
  const playedFraction = played?.recordingId === recordingId ? played.fraction : 0
  const currentSeconds = played?.recordingId === recordingId ? played.seconds : 0
  // チャプターの目盛りを割合に直す分母。未確定の間は 0（目盛りを出さない）。
  const durationSeconds = played?.recordingId === recordingId ? played.duration : 0
  // CM 自動スキップ（端末ごとの好み。`rokuban:playback-rate` と同じ扱い）。
  const [skipEnabled, setSkipEnabled] = useState(loadChapterSkip)
  // 直前の観測位置。通常の再生で区間の先頭を跨いだかだけを見る（手動シークで
  // 区間の中に入ったときに飛ばさないため。`lib/chapters.ts` の skipTarget）。
  const previousSecondsRef = useRef(0)
  // 境界の前後再生の間は自動スキップを止める。境界が cut 区間の先頭のとき、
  // 飛ばすと「その境界を見る」操作そのものが成立しない。
  const skipSuppressedRef = useRef(false)
  const playAroundTimerRef = useRef<number | undefined>(undefined)
  // 前後再生の停止位置（秒）。null は前後再生中でない。停止は再生位置で判定する
  // （実時間のタイマーだと再生速度が 1 倍でないとき止まる位置がずれる）。
  const playAroundStopRef = useRef<number | null>(null)
  // `chapters ?? []` を毎レンダー評価すると、未取得の間だけ配列の参照が毎回変わる。
  // 編集 UI は「参照が変わった = サーバーの値が変わった」と見なしてドラフトを
  // 追随させるので、参照はここで安定させておく。
  const chapterSpans = useMemo(
    () => (playingCut ? [] : (chapters ?? [])),
    [chapters, playingCut],
  )
  const shownPreview =
    tilePreview?.recordingId === recordingId && tilesAvailableFor === recordingId ? tilePreview : null
  // 再生開始時の変換表を固定する。SSE で別世代が届くと video key が変わり、
  // 新しいファイルだけが新しい keepRanges を使う。
  const restorePending = useRef(true)
  const frozenKeepRangesRef = useRef<readonly KeepRange[] | undefined>(
    playingCut ? selectedAsset?.keepRanges : undefined,
  )
  const watchedRequestPendingRef = useRef(false)
  // 同じページ内で画質を切り替えると <video> ごと作り直される。`resumePositionMs` は
  // ページを開いた時点の値のままなので、直前まで見ていた位置は原本 ms でここに持ち越す
  // （保存の成否に依らない）。復元が済むまでは書かない（先頭の 0 で上書きしない）。
  const carriedPositionRef = useRef<{ recordingId: number; ms: number } | null>(null)
  const onWatchedRef = useRef(onWatched)
  useEffect(() => {
    onWatchedRef.current = onWatched
  })

  const currentWrite = useCallback((video: HTMLVideoElement) => {
    const keepRanges = frozenKeepRangesRef.current
    if (playingCut && (!keepRanges || keepRanges.length === 0)) return null
    return playbackPositionWrite(
      video.currentTime,
      video.duration,
      true,
      playingCut ? keepRanges : undefined,
    )
  }, [playingCut])

  const rememberPosition = useCallback((video: HTMLVideoElement) => {
    if (restorePending.current) return
    const write = currentWrite(video)
    if (write === null) return
    carriedPositionRef.current = { recordingId, ms: write.kind === 'put' ? write.positionMs : 0 }
  }, [currentWrite, recordingId])

  const saveCurrentPosition = useCallback((video: HTMLVideoElement, keepalive = false) => {
    const write = currentWrite(video)
    if (write === null) return
    rememberPosition(video)
    if (write.kind === 'watched') {
      if (watchedRequestPendingRef.current) return
      watchedRequestPendingRef.current = true
      void persistPlaybackPosition(recordingId, write, keepalive).then((saved) => {
        if (saved) onWatchedRef.current?.()
        else watchedRequestPendingRef.current = false
      })
      return
    }
    void persistPlaybackPosition(recordingId, write, keepalive)
  }, [currentWrite, recordingId, rememberPosition])

  useLayoutEffect(() => {
    frozenKeepRangesRef.current = playingCut ? selectedAsset?.keepRanges : undefined
    restorePending.current = true
    watchedRequestPendingRef.current = false
    previousSecondsRef.current = 0
    skipSuppressedRef.current = false
    playAroundStopRef.current = null
    // keepRangesKey が selectedAsset.keepRanges の内容を表す。参照を依存に入れると、
    // 内容が同じ再取得でも復元待ち・直前位置がリセットされる。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keepRangesKey, playingCut, recordingId, selectedProfile])

  useEffect(() => {
    clearLegacyPlaybackPositions()
  }, [])

  useEffect(() => {
    const saveIfPlaying = () => {
      const video = videoRef.current
      if (video && !video.paused) saveCurrentPosition(video)
    }
    const onPageHide = () => {
      const video = videoRef.current
      if (video) saveCurrentPosition(video, true)
    }
    const timer = window.setInterval(saveIfPlaying, 15_000)
    window.addEventListener('pagehide', onPageHide)
    return () => {
      window.clearInterval(timer)
      window.removeEventListener('pagehide', onPageHide)
    }
  }, [keepRangesKey, recordingId, saveCurrentPosition, selectedProfile])

  // 再生中に別の録画へ移ってもタイマーや境界の前後再生を残さない。
  useEffect(() => () => {
    window.clearTimeout(controlsTimerRef.current)
    window.clearTimeout(playAroundTimerRef.current)
  }, [])

  // 録画を変えても速度は保つ（以前はここで 1 倍に戻していた）。速度は端末ごとの
  // 好みであって録画ごとの状態ではない（`lib/playback-position.ts`）。
  //
  // **`recordingId` を依存に含める。** `<video>` は `key={`${recordingId}:${profile}`}`
  // なので、別の録画に移ると DOM 要素ごと作り直される。`recordingId` が依存に無いと
  // 「`profile` は変わらず `playbackRate` state も既に 1.5 のまま」という場合に
  // 依存配列が前回と同じと判定されて effect が再実行されず、新しい要素の既定値
  // （1 倍）のままになる、という退行（レビュー指摘）。**`defaultPlaybackRate` にも同じ値を
  // 入れる。** `src` を差し替える media element load algorithm は `playbackRate` を
  // `defaultPlaybackRate` へ戻すため、`playbackRate` だけ設定しても再生が始まった
  // 瞬間に 1 倍へ巻き戻りうる。
  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const appliedRate = applyPlaybackRate(video, playbackRate)
    if (appliedRate !== playbackRate) setPlaybackRate(appliedRate)
  }, [recordingId, selectedProfile, playbackRate])

  const updateFullscreenState = () => {
    setIsFullscreen(document.fullscreenElement === fullscreenRef.current)
  }
  const requestPlayerFullscreen = () => {
    const container = fullscreenRef.current
    if (document.fullscreenElement === container) {
      void document.exitFullscreen?.().catch(() => {})
      return
    }
    if (container?.requestFullscreen) {
      void container.requestFullscreen().catch(() => {})
      return
    }
    const video = videoRef.current as (HTMLVideoElement & { webkitEnterFullscreen?: () => void }) | null
    video?.webkitEnterFullscreen?.()
  }
  const togglePictureInPicture = () => {
    const video = videoRef.current
    if (!video || !document.pictureInPictureEnabled) return
    if (document.pictureInPictureElement === video) {
      void document.exitPictureInPicture?.().catch(() => {})
      return
    }
    void video.requestPictureInPicture?.().catch(() => {})
  }
  const syncMediaState = (video: HTMLVideoElement) => {
    setMediaPlaying(!video.paused)
    setMuted(video.muted)
    setVolume(video.volume)
    setSubtitlesEnabled(Array.from(video.textTracks).some((track) => track.kind === 'subtitles' && track.mode === 'showing'))
    setPictureInPicture(document.pictureInPictureElement === video)
  }
  const updateSubtitleCueLines = (video: HTMLVideoElement, raise: boolean) => {
    const frame = fullscreenRef.current
    // スマホの操作表示は枠全体に幕を敷くので、字幕を避ける高さは下端の帯（時刻・シークバー）だけ。
    const controls = frame?.querySelector<HTMLElement>('[data-testid="player-controls-bottom"]')
    const frameHeight = frame?.getBoundingClientRect().height ?? 0
    const controlsHeight = controls?.getBoundingClientRect().height ?? 0
    // WebVTT の snap-to-lines は画面高の約 5% が 1 行分。シークバーと操作行が
    // 隠す高さから必要な行数を計算し、固定行数ではなく画面幅に追随させる。
    const lineHeight = frameHeight * 0.05
    const raisedLine = lineHeight > 0
      ? -(Math.ceil((controlsHeight + 8) / lineHeight) + 1)
      : -5
    for (const track of Array.from(video.textTracks)) {
      if (track.kind !== 'subtitles') continue
      for (const rawCue of Array.from(track.cues ?? [])) {
        const cue = rawCue as VTTCue
        if (raise) {
          if (!subtitleLinesRef.current.has(cue)) subtitleLinesRef.current.set(cue, cue.line)
          cue.line = raisedLine
        } else {
          const original = subtitleLinesRef.current.get(cue)
          if (original !== undefined) cue.line = original
        }
      }
    }
  }
  const handleControlsActivity = () => {
    setControlsVisible(true)
    window.clearTimeout(controlsTimerRef.current)
    if (!mediaPlaying || toolbarFocused) return
    controlsTimerRef.current = window.setTimeout(() => setControlsVisible(false), 3000)
  }
  // バーを出したままにするのはキーボードフォーカス（:focus-visible）だけ。
  // マウスで押したボタンに残ったフォーカスで出しっぱなしにすると、再生中ずっと映像に被る。
  const handleToolbarFocus = (event: ReactFocusEvent<HTMLElement>) => {
    let keyboard = false
    try {
      keyboard = (event.target as Element).matches(':focus-visible')
    } catch {
      keyboard = false
    }
    if (!keyboard) return
    setToolbarFocused(true)
    setControlsVisible(true)
    window.clearTimeout(controlsTimerRef.current)
  }
  const handleToolbarBlur = (event: ReactFocusEvent<HTMLElement>) => {
    const relatedTarget = event.relatedTarget
    const shell = event.currentTarget.closest('[data-testid="recording-player-shell"]')
    const toolbar = shell?.querySelector('[data-testid="player-controls"]')
    const settings = shell?.querySelector('[data-testid="playback-settings"]')
    if (
      relatedTarget instanceof Node &&
      (toolbar?.contains(relatedTarget) || settings?.contains(relatedTarget))
    ) return
    // マウスで押したボタンの blur（隠れたバーの inert でフォーカスが落ちるときも来る）で
    // 再表示しない。キーボードフォーカスを離れたときだけ、隠すタイマーを張り直す。
    if (!toolbarFocused) return
    setToolbarFocused(false)
    handleControlsActivity()
  }

  useEffect(() => {
    const onFullscreenChange = () => updateFullscreenState()
    document.addEventListener('fullscreenchange', onFullscreenChange)
    return () => {
      document.removeEventListener('fullscreenchange', onFullscreenChange)
    }
  }, [])

  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const onPictureInPictureChange = () => {
      setPictureInPicture(document.pictureInPictureElement === video)
    }
    video.addEventListener('enterpictureinpicture', onPictureInPictureChange)
    video.addEventListener('leavepictureinpicture', onPictureInPictureChange)
    return () => {
      video.removeEventListener('enterpictureinpicture', onPictureInPictureChange)
      video.removeEventListener('leavepictureinpicture', onPictureInPictureChange)
    }
  }, [recordingId, selectedProfile])

  useEffect(() => {
    const video = videoRef.current
    const frame = fullscreenRef.current
    const controls = frame?.querySelector<HTMLElement>('[data-testid="player-controls-bottom"]')
    if (!video) return
    const update = () => updateSubtitleCueLines(video, controlsVisible)
    update()

    const trackElements = Array.from(video.querySelectorAll('track[kind="subtitles"]'))
    const textTracks = Array.from(video.textTracks).filter((track) => track.kind === 'subtitles')
    const eventTrackElements = trackElements.filter((track) => typeof track.addEventListener === 'function')
    const eventTextTracks = textTracks.filter((track) => typeof track.addEventListener === 'function')
    eventTrackElements.forEach((track) => track.addEventListener('load', update))
    eventTextTracks.forEach((track) => track.addEventListener('cuechange', update))

    const observer = frame && controls && typeof ResizeObserver !== 'undefined'
      ? new ResizeObserver(update)
      : null
    observer?.observe(frame!)
    observer?.observe(controls!)
    return () => {
      eventTrackElements.forEach((track) => track.removeEventListener('load', update))
      eventTextTracks.forEach((track) => track.removeEventListener('cuechange', update))
      observer?.disconnect()
    }
  }, [recordingId, selectedProfile, controlsVisible, subtitlesEnabled])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const video = videoRef.current
      if (!video || event.ctrlKey || event.metaKey || event.altKey) return
      if (
        event.target instanceof Element &&
        event.target.closest('input, textarea, select, button, a, [role="slider"], [contenteditable]')
      ) {
        return
      }

      const key = event.key.toLowerCase()
      const seekBy = (seconds: number) => {
        const target = Math.max(0, video.currentTime + seconds)
        jumpToRef.current(Number.isFinite(video.duration)
          ? Math.min(video.duration, target)
          : target)
      }
      let handled = true
      switch (key) {
        case ' ':
          if (video.paused) void video.play()
          else video.pause()
          break
        case 'arrowleft':
          seekBy(-10)
          break
        case 'arrowright':
          seekBy(10)
          break
        case 'j':
          seekBy(-30)
          break
        case 'l':
          seekBy(30)
          break
        case 'm':
          video.muted = !video.muted
          break
        case 'f':
          requestPlayerFullscreen()
          break
        default:
          if (/^[0-9]$/.test(key) && Number.isFinite(video.duration)) {
            jumpToRef.current((video.duration * Number(key)) / 10)
          } else {
            handled = false
          }
      }
      if (handled) event.preventDefault()
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  if (profiles.length === 0) {
    return (
      <div className={cn('text-muted-foreground', className)}>
        {hasOriginal ? (
          <p>
            ブラウザ再生用のエンコードがまだありません。原本は{' '}
            <a
              href={recordingFileURL(recordingId)}
              className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline"
            >
              VLC 等で開く
            </a>
            ことができます。
          </p>
        ) : (
          <p>再生可能なファイルがありません。</p>
        )}
      </div>
    )
  }

  const src = recordingFileURL(recordingId, selectedProfile)
  const updatePlayedFraction = (video: HTMLVideoElement) => {
    const known = Number.isFinite(video.duration) && video.duration > 0 ? video.duration : 0
    setPlayed((previous) => ({
      recordingId,
      seconds: video.currentTime,
      fraction: known > 0 ? Math.max(0, Math.min(1, video.currentTime / known)) : 0,
      // duration が読めない瞬間（load 直後）に前回の値を捨てない。
      duration: known > 0 ? known : previous?.recordingId === recordingId ? previous.duration : 0,
    }))
  }
  // jumpTo は区間の境界・一覧から飛ぶ。飛んだ先を直前位置として記録する
  // （飛んだ直後の timeupdate を「区間の先頭を跨いだ」と誤認しないため）。
  const jumpTo = (seconds: number) => {
    const video = videoRef.current
    if (!video) return
    video.currentTime = seconds
    previousSecondsRef.current = seconds
    updatePlayedFraction(video)
  }
  jumpToRef.current = jumpTo
  const jumpChapter = (direction: 'next' | 'prev') => {
    const target = chapterJumpTarget(chapterSpans, currentSeconds, direction)
    if (target !== undefined) jumpTo(target)
  }
  // playAround は境界の前後 3 秒を再生して止める（修正 UI の「前後 3 秒」）。
  const playAround = (seconds: number) => {
    const video = videoRef.current
    if (!video) return
    const start = Math.max(0, seconds - PLAY_AROUND_SECONDS)
    const stop = seconds + PLAY_AROUND_SECONDS
    window.clearTimeout(playAroundTimerRef.current)
    skipSuppressedRef.current = true
    playAroundStopRef.current = stop
    jumpTo(start)
    void video.play()
    // 本来の停止は timeupdate の `currentTime >= stop`。これは再生が進まない場合
    // （バッファ待ちなど）に抑制が残り続けないための保険で、再生速度ぶん余裕を持たせる。
    playAroundTimerRef.current = window.setTimeout(
      () => finishPlayAround(video),
      ((stop - start) * 1000) / Math.max(video.playbackRate, 0.1) + 2000,
    )
  }
  const finishPlayAround = (video: HTMLVideoElement) => {
    window.clearTimeout(playAroundTimerRef.current)
    playAroundStopRef.current = null
    skipSuppressedRef.current = false
    video.pause()
  }
  // プレイヤー内の唯一の seekbar 上のポインタ位置 → 再生位置（秒）。
  const scrubSeconds = (event: ReactPointerEvent<HTMLDivElement>): number | null => {
    const video = videoRef.current
    if (!video || !Number.isFinite(video.duration) || video.duration <= 0) return null
    const rect = event.currentTarget.getBoundingClientRect()
    if (rect.width <= 0) return null
    const fraction = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width))
    return fraction * video.duration
  }
  const seekAtPointer = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!isScrubbingRef.current) return
    const seconds = scrubSeconds(event)
    if (seconds !== null) jumpTo(seconds)
  }
  const handleScrubMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    seekAtPointer(event)
    // カット版ではタイルを出さない（原本の時間軸で作られており、カット版の軸へ
    // 写像していない。上の playingCut のコメント参照）。取りに行きもしない。
    if (playingCut) {
      setTilePreview(null)
      return
    }
    // プレビューはマウスだけに出す。タッチは pointerleave が来ないので、タップの
    // 後にプレビューが映像を覆ったまま残る。タップは帯のクリック（シーク）だけに効く。
    if (event.pointerType !== 'mouse') {
      setTilePreview(null)
      return
    }
    // タイルは**最初に触れたときだけ**取りに行く。マウント時に先読みすると、
    // 3 時間の録画で 2 MB 程度を、一度もホバーしない利用者にも払わせることになる。
    // 同じキーを再設定しても React は再描画しないので、毎回呼んでよい。
    setTilesRequestedFor(recordingId)

    const seconds = scrubSeconds(event)
    const tile = seconds === null ? null : seekTileAt(seconds)
    if (tile === null || tilesAvailableFor !== recordingId) {
      setTilePreview(null)
      return
    }
    const rect = event.currentTarget.getBoundingClientRect()
    // 帯が 1 枚ぶんより狭い（狭い画面）ときは、はみ出さないよう縮めて出す。
    const scale = Math.min(1, rect.width / SEEK_TILES_DISPLAY_WIDTH)
    const width = SEEK_TILES_DISPLAY_WIDTH * scale
    const left = Math.max(0, Math.min(rect.width - width, event.clientX - rect.left - width / 2))
    setTilePreview({ recordingId, ...tile, left, scale, seconds: seconds ?? 0 })
  }
  const handleScrubPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    isScrubbingRef.current = true
    event.currentTarget.setPointerCapture?.(event.pointerId)
    handleScrubMove(event)
  }
  const handleScrubPointerUp = (event: ReactPointerEvent<HTMLDivElement>) => {
    seekAtPointer(event)
    if (!isScrubbingRef.current) return
    isScrubbingRef.current = false
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture?.(event.pointerId)
    }
  }

  return (
    <section className={cn('flex flex-col gap-2', className)} aria-label="再生">
      <RecordingPlaybackControls
        recordingId={recordingId}
        profile={selectedProfile}
        encodedAssets={encodedAssets}
        fullscreenRef={fullscreenRef}
        currentSeconds={currentSeconds}
        durationSeconds={durationSeconds}
        playedFraction={playedFraction}
        chapters={chapterSpans}
        playingCut={playingCut}
        tilePreview={shownPreview}
        tilesRequested={tilesRequestedFor === recordingId}
        tilesAvailable={tilesAvailableFor === recordingId}
        onTileImageLoad={() => setTilesAvailableFor(recordingId)}
        onTileImageError={() => {
          setTilesAvailableFor((current) => (current === recordingId ? null : current))
          setTilePreview(null)
        }}
        onSeekPointerDown={handleScrubPointerDown}
        onSeekPointerMove={handleScrubMove}
        onSeekPointerUp={handleScrubPointerUp}
        onSeekPointerLeave={() => setTilePreview(null)}
        onSeek={jumpTo}
        onSelectProfile={setProfile}
        onPreviousChapter={() => jumpChapter('prev')}
        onNextChapter={() => jumpChapter('next')}
        onShowChapters={
          !playingCut && onSaveChapters && onResetChapters && !chapterDetectionPending && chapterVersion !== undefined
            ? () => {
                const details = chapterDetailsRef.current
                if (!details) return
                if (document.fullscreenElement) void document.exitFullscreen?.().catch(() => {})
                details.open = true
                details.scrollIntoView?.({ block: 'nearest' })
              }
            : undefined
        }
        isPlaying={mediaPlaying}
        muted={muted}
        volume={volume}
        playbackRate={playbackRate}
        subtitlesEnabled={subtitlesEnabled}
        skipEnabled={skipEnabled}
        pictureInPicture={pictureInPicture}
        isFullscreen={isFullscreen}
        showWatched={showWatched}
        watched={watched}
        watchedPending={watchedPending}
        onPutWatched={putWatched}
        onDeleteWatched={deleteWatched}
        onTogglePlay={() => {
          const video = videoRef.current
          if (!video) return
          if (video.paused) void video.play().catch(() => {})
          else video.pause()
        }}
        onToggleMute={() => {
          const video = videoRef.current
          if (!video) return
          video.muted = !video.muted
          setMuted(video.muted)
        }}
        onVolumeChange={(nextVolume) => {
          const video = videoRef.current
          if (!video) return
          video.volume = nextVolume
          video.muted = nextVolume === 0
          setVolume(video.volume)
          setMuted(video.muted)
        }}
        onRateChange={(rate) => {
          const video = videoRef.current
          if (!video) return
          const applied = applyPlaybackRate(video, rate)
          setPlaybackRate(applied)
          savePlaybackRate(applied)
        }}
        onToggleSubtitles={() => {
          const video = videoRef.current
          if (!video) return
          const enabled = !subtitlesEnabled
          for (const track of Array.from(video.textTracks)) {
            if (track.kind === 'subtitles') track.mode = enabled ? 'showing' : 'disabled'
          }
          setSubtitlesEnabled(enabled)
          updateSubtitleCueLines(video, enabled && controlsVisible)
        }}
        onToggleSkip={(enabled) => {
          setSkipEnabled(enabled)
          saveChapterSkip(enabled)
        }}
        onTogglePictureInPicture={togglePictureInPicture}
        onToggleFullscreen={requestPlayerFullscreen}
        controlsVisible={controlsVisible || !mediaPlaying || toolbarFocused}
        onControlsActivity={handleControlsActivity}
        onHideControls={() => {
          window.clearTimeout(controlsTimerRef.current)
          if (mediaPlaying) setControlsVisible(false)
        }}
        onToolbarFocus={handleToolbarFocus}
        onToolbarBlur={handleToolbarBlur}
        onShellKeyDown={handleControlsActivity}
        video={(
          <video
            ref={videoRef}
            key={`${recordingId}:${selectedProfile}:${keepRangesKey}`}
            tabIndex={0}
            aria-label="録画映像"
            playsInline
            preload="metadata"
            src={src}
            className="absolute inset-0 size-full bg-black object-contain"
            onPointerDown={(event) => {
              videoPointerTypeRef.current = event.pointerType
            }}
            onClick={(event) => {
              // タッチでは映像のタップで操作を出す（再生 / 一時停止は中央のボタン）。
              if (videoPointerTypeRef.current === 'touch') {
                videoPointerTypeRef.current = ''
                handleControlsActivity()
                return
              }
              if (event.currentTarget.paused) void event.currentTarget.play().catch(() => {})
              else event.currentTarget.pause()
            }}
            onLoadedMetadata={(e) => {
              updatePlayedFraction(e.currentTarget)
              syncMediaState(e.currentTarget)
              updateSubtitleCueLines(e.currentTarget, controlsVisible)
              if (!restorePending.current) return
              restorePending.current = false
              const carried = carriedPositionRef.current
              const pos = playbackResumeSeconds(
                carried?.recordingId === recordingId ? carried.ms : resumePositionMs,
                frozenKeepRangesRef.current,
              )
              if (pos !== null) {
                e.currentTarget.currentTime = Number.isFinite(e.currentTarget.duration)
                  ? Math.min(pos, e.currentTarget.duration)
                  : pos
              }
            }}
            onSeeking={(e) => {
              // seeking → timeupdate → seeked の順なので、シーク開始時に直前位置を更新する。
              previousSecondsRef.current = e.currentTarget.currentTime
            }}
            onSeeked={(e) => {
              previousSecondsRef.current = e.currentTarget.currentTime
              updatePlayedFraction(e.currentTarget)
              saveCurrentPosition(e.currentTarget)
            }}
            onTimeUpdate={(e) => {
              const v = e.currentTarget
              updatePlayedFraction(v)
              rememberPosition(v)
              const previous = previousSecondsRef.current
              previousSecondsRef.current = v.currentTime
              const stopAt = playAroundStopRef.current
              if (stopAt !== null && v.currentTime >= stopAt) finishPlayAround(v)
              if (skipEnabled && !skipSuppressedRef.current && !v.paused) {
                const target = skipTarget(chapterSpans, previous, v.currentTime, v.duration)
                if (target !== undefined) {
                  v.currentTime = target
                  previousSecondsRef.current = target
                }
              }
              if (Number.isFinite(v.duration) && v.duration > 0 && v.currentTime >= v.duration * 0.9) {
                saveCurrentPosition(v)
              }
            }}
            onPlay={() => {
              setMediaPlaying(true)
              setControlsVisible(true)
              window.clearTimeout(controlsTimerRef.current)
              if (!toolbarFocused) {
                controlsTimerRef.current = window.setTimeout(() => setControlsVisible(false), 3000)
              }
            }}
            onPause={(e) => {
              setMediaPlaying(false)
              setControlsVisible(true)
              window.clearTimeout(controlsTimerRef.current)
              saveCurrentPosition(e.currentTarget)
            }}
            onVolumeChange={(e) => {
              setMuted(e.currentTarget.muted)
              setVolume(e.currentTarget.volume)
            }}
            onRateChange={(e) => {
              const rate = e.currentTarget.playbackRate
              setPlaybackRate(rate)
              savePlaybackRate(rate)
            }}
          >
            <track
              kind="subtitles"
              srcLang="ja"
              label="日本語"
              src={recordingSubtitleURL(recordingId, selectedProfile)}
              onLoad={() => {
                const video = videoRef.current
                if (video) updateSubtitleCueLines(video, controlsVisible)
              }}
            />
          </video>
        )}
      />

      {playingCut && selectedAsset?.cutStale === true && (
        <div className="flex max-w-3xl flex-wrap items-center gap-2 rounded border border-warning/50 bg-warning/10 px-3 py-2">
          <p className="text-warning">
            このカット版は編集前の内容です。現在のチャプターに合わせて作り直せます。
          </p>
          {onReencode !== undefined && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={reencodePending}
              onClick={() => onReencode(selectedProfile)}
            >
              作り直す
            </Button>
          )}
        </div>
      )}

      {!playingCut && onSaveChapters && onResetChapters &&
        (chapterDetectionPending || chapterVersion !== undefined) && (
        chapterDetectionPending ? (
          <p className="text-muted-foreground" data-testid="chapter-detecting">
            CM を検出しています。終わるまでチャプターは編集できません
          </p>
        ) : (
        <details ref={chapterDetailsRef} data-testid="chapter-editor-details" className="group max-w-3xl">
          <DetailSummary>チャプター {chapters?.length ?? 0} 件</DetailSummary>
          <div className="pt-2">
          <RecordingChapterEditor
            spans={chapterSpans}
            version={chapterVersion!}
            detectionPending={false}
            source={chapterSource}
            currentSeconds={currentSeconds}
            playAround={playAround}
            jumpTo={jumpTo}
            onSave={onSaveChapters}
            onReset={onResetChapters}
            pending={chapterSavePending}
          />
          </div>
        </details>
        )
      )}
    </section>
  )
}
