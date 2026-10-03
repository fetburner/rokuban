import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type MutableRefObject,
  type PointerEvent as ReactPointerEvent,
} from 'react'

import type { ChapterSpan, EncodedAsset, KeepRange, RecordingChaptersSource } from '@/api/generated'
import {
  RecordingChapterEditor,
  type ChapterEditorCommands,
  type ChapterEditorStatus,
} from '@/components/recording-chapter-editor'
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
  cutMsToOriginalMs,
  recordingFileURL,
  recordingSubtitleURL,
  savePlaybackRate,
} from '@/lib/playback-position'
import { formatDate, formatTime } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { usePlayerFrame } from '@/lib/use-player-frame'
import { cn } from '@/lib/utils'
import {
  seekTilePlacement,
} from '@/lib/seek-tiles'

/** 終端カードが次のエピソードへ自動で移るまでの秒数。「取り消す」で止められる。 */
const AUTO_ADVANCE_SECONDS = 3

type RecordingPlayerProps = {
  recordingId: number
  resumePositionMs?: number
  /** 現在の位置を原本時間軸の秒で親へ伝える。 */
  onRecordingPositionChange?: (seconds: number) => void
  /** 終端 / エラーで再生元を選び直した場合は true を返す。 */
  onRecordingPlaybackEnded?: (recordingPositionSeconds: number, wasPlaying: boolean) => boolean
  /** 動画がエラーを返した。一度も再生していなければ位置は undefined。親が選び直したら true。 */
  onRecordingPlaybackError?: (recordingPositionSeconds: number | undefined, wasPlaying: boolean) => boolean
  /** 最初の読み込みが終わったら再生を始める（再生元を替えた直後に、再生中だった続きを見る）。 */
  autoPlay?: boolean
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
  /** 操作バーと終端カードに出す再生可能な次のエピソード。 */
  nextEpisode?: { id: number; title: string; startAt: string }
  /** バーの「次のエピソード」リンクを押したとき（移動先の詳細を先にキャッシュへ入れる）。 */
  onNextEpisodeNavigate?: () => void
  /** 番組時間枠より前後を録画した部分をシークバー内に示す割合。 */
  outsideProgramSegments?: { beforeEndPercent: number; afterStartPercent: number }
  /**
   * 別の録画の詳細へ移る（履歴に積む）。終端カードの「今すぐ再生」と自動遷移が使う。
   * 呼び出し側が移動先の詳細を先にキャッシュへ入れておくと、全画面のまま移れる。
   */
  onNavigateToRecording?: (id: number) => void
  onTrash?: () => void
  onProfileChange?: (profile: string) => void
  /**
   * 再生可能な encoded 派生物（active media_assets）。空ならプレイヤーを出さない。
   * `sizeBytes` が省略された要素も**選択肢そのものは隠さない**（M7-3 の値札
   * 方針: サイズが取れないという分類の失敗で機能を隠さない。ドロップ統計の
   * 「分類できなかった PID」と同じ判断。docs/frontend/recordings.md）。
   */
  encodedAssets: EncodedAsset[]
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
  onResetChapters?: () => Promise<unknown> | void
  /** 保存 / 取り消しの実行中。 */
  chapterSavePending?: boolean
  /** ページ見出しと連動するチャプター編集モード。 */
  chapterEditing?: boolean
  onEnterChapterEditing?: () => void
  chapterEditorCommandsRef?: MutableRefObject<ChapterEditorCommands | null>
  onChapterEditorStatusChange?: (status: ChapterEditorStatus) => void
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
  autoPlay = false,
  onWatched,
  onRecordingPositionChange,
  onRecordingPlaybackEnded,
  onRecordingPlaybackError,
  showWatched = false,
  watched = false,
  watchedPending = false,
  putWatched,
  deleteWatched,
  preferredProfile,
  nextEpisode,
  onNextEpisodeNavigate,
  outsideProgramSegments,
  onNavigateToRecording,
  onTrash,
  onProfileChange,
  encodedAssets,
  chapters,
  chapterSource = 'auto',
  chapterVersion,
  chapterDetectionPending = false,
  onSaveChapters,
  onResetChapters,
  chapterSavePending = false,
  chapterEditing = false,
  onEnterChapterEditing,
  chapterEditorCommandsRef,
  onChapterEditorStatusChange,
  onReencode,
  reencodePending = false,
  className,
}: RecordingPlayerProps) {
  // `encodedAssets` の参照が変わらない限り再計算しない --- 素の `.map()` だと
  // 毎レンダーで新しい配列になり、下の useEffect の依存配列がレンダーごとに
  // 変化したと判定されて毎回走ってしまう（中身は冪等で setProfile を呼ばない
  // 限りループにはならないが、無駄な再実行を避ける）。
  const profiles = useMemo(() => encodedAssets.map((a) => a.profile), [encodedAssets])
  // 選んだ画質は録画ごとに戻す（docs/frontend/recordings.md）。親は録画を切り替えても
  // このコンポーネントを作り直さないので、id が変わったら描画中に選択を捨てて既定に倒す。
  // 親（版タブの「再生中」）も同じ時点で既定に戻すので、両者が一致する。「戻る」で前の回へ
  // 戻ったときも既定に戻す（id と組で残すだけだと、戻った回でプレイヤーだけが前の選択に戻る）。
  const [chosenProfile, setChosenProfile] = useState<string | null>(null)
  const [chosenFor, setChosenFor] = useState(recordingId)
  if (chosenFor !== recordingId) {
    setChosenFor(recordingId)
    setChosenProfile(null)
  }
  const defaultProfile =
    preferredProfile !== undefined && profiles.includes(preferredProfile) ? preferredProfile : (profiles[0] ?? '')
  // props の資産一覧が更新されて選択中プロファイルが消えた場合は、effect で一度
  // 無効な値を描いてから直すのではなく、表示値をその場で既定へ導出する。
  const selectedProfile =
    chosenFor === recordingId && chosenProfile !== null && profiles.includes(chosenProfile) ? chosenProfile : defaultProfile
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
  const [editorSelected, setEditorSelected] = useState<number | null>(null)
  const localChapterEditorCommandsRef = useRef<ChapterEditorCommands | null>(null)
  const resolvedChapterEditorCommandsRef = chapterEditorCommandsRef ?? localChapterEditorCommandsRef
  const isScrubbingRef = useRef(false)
  const playingRef = useRef(false)
  const jumpToRef = useRef<(seconds: number) => void>(() => {})
  const subtitleLinesRef = useRef(new WeakMap<VTTCue, VTTCue['line']>())
  // 枠（バーの自動非表示・フォーカス・映像のタップ・全画面・PiP）は原本 HLS の LivePlayer と共有する。
  const frame = usePlayerFrame(videoRef, fullscreenRef, `${recordingId}:${selectedProfile}`)
  const { controlsVisible, requestFullscreen } = frame
  // 終端カードを出している録画の id。録画を切り替えても作り直さないので、id と組で持って
  // 切り替えた瞬間に前の録画のカードを描かない（`played` と同じ規律）。
  const [endCardFor, setEndCardFor] = useState<number | null>(null)
  // 編集中は終端カードも次の回への自動遷移も止める（自動スキップと同じ。前後 3 秒の再生で終端に届きうる）。
  const endCardOpen = endCardFor === recordingId && !chapterEditing
  const [countdownSeconds, setCountdownSeconds] = useState(AUTO_ADVANCE_SECONDS)
  // 終端カードから移った先の録画 id。移った先は再生を始める（カードの文言どおり）。
  const autoplayRecordingRef = useRef<number | null>(autoPlay ? recordingId : null)
  const [subtitlesEnabled, setSubtitlesEnabled] = useState(false)
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
    width: number
    height: number
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
  const chapterEditorStatusChange = onChapterEditorStatusChange ?? (() => {})
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
    () => (playingCut || chapterEditing ? [] : (chapters ?? [])),
    [chapters, chapterEditing, playingCut],
  )
  const shownPreview =
    tilePreview?.recordingId === recordingId && tilesAvailableFor === recordingId ? tilePreview : null
  const navigateToRecordingRef = useRef(onNavigateToRecording)
  navigateToRecordingRef.current = onNavigateToRecording
  const nextEpisodeRef = useRef(nextEpisode)
  nextEpisodeRef.current = nextEpisode

  const advanceToNext = (id: number) => {
    autoplayRecordingRef.current = id
    navigateToRecordingRef.current?.(id)
  }
  const advanceToNextRef = useRef(advanceToNext)
  advanceToNextRef.current = advanceToNext

  useEffect(() => {
    if (!endCardOpen || nextEpisodeRef.current === undefined || navigateToRecordingRef.current === undefined) return
    const interval = window.setInterval(() => setCountdownSeconds((seconds) => Math.max(0, seconds - 1)), 1000)
    const timeout = window.setTimeout(() => {
      const nextId = nextEpisodeRef.current?.id
      if (nextId !== undefined) advanceToNextRef.current(nextId)
    }, AUTO_ADVANCE_SECONDS * 1000)
    return () => {
      window.clearInterval(interval)
      window.clearTimeout(timeout)
    }
  }, [endCardOpen, nextEpisode?.id])

  // 暗い映像の上の固定色のボタン。テーマのボタンは明るい地を前提にしていて、黒いカード上では読めない。
  const cardButton =
    'inline-flex min-h-10 items-center justify-center rounded-md border border-white/40 bg-white/10 px-4 text-sm font-medium whitespace-nowrap text-white outline-none hover:bg-white/20 focus-visible:ring-2 focus-visible:ring-white'
  const cardPrimaryButton =
    'inline-flex min-h-10 items-center justify-center rounded-md bg-white px-4 text-sm font-semibold whitespace-nowrap text-black outline-none hover:bg-white/90 focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-white focus-visible:ring-offset-black'
  const trashButton = onTrash && (
    <button type="button" className={cardButton} onClick={onTrash}>
      この回をごみ箱へ
    </button>
  )
  const endCard = endCardOpen ? (
    <div
      data-testid="recording-end-card"
      role="group"
      aria-label="再生が終わりました"
      className="absolute inset-0 z-20 flex items-center justify-center bg-black/90 p-3 text-white sm:p-6"
    >
      {nextEpisode ? (
        <div className="flex w-full max-w-3xl items-center gap-3 sm:gap-6">
          <div className="relative aspect-video w-[34%] max-w-72 shrink-0 overflow-hidden rounded-md bg-white/10">
            <img
              src={`/api/media/recordings/${nextEpisode.id}/thumbnail`}
              alt=""
              className="size-full object-cover"
              onError={(event) => event.currentTarget.remove()}
            />
            <svg
              data-testid="end-card-countdown-ring"
              viewBox="0 0 36 36"
              aria-hidden
              className="absolute right-1.5 bottom-1.5 size-9 sm:size-11"
            >
              <circle cx="18" cy="18" r="15" className="fill-black/70" />
              <circle cx="18" cy="18" r="15" fill="none" strokeWidth="3" className="stroke-white/30" />
              <circle
                cx="18"
                cy="18"
                r="15"
                fill="none"
                strokeWidth="3"
                strokeLinecap="round"
                className="origin-center -rotate-90 stroke-white transition-[stroke-dashoffset] duration-1000 ease-linear"
                strokeDasharray={2 * Math.PI * 15}
                strokeDashoffset={2 * Math.PI * 15 * (1 - countdownSeconds / AUTO_ADVANCE_SECONDS)}
              />
              <text x="18" y="22.5" textAnchor="middle" className="fill-white text-[13px]">
                {countdownSeconds}
              </text>
            </svg>
          </div>
          <div className="flex min-w-0 flex-col gap-1.5 sm:gap-2">
            <p className="text-xs text-white/70 sm:text-sm">
              次のエピソード · {countdownSeconds} 秒後に再生
            </p>
            <p className="truncate text-sm font-semibold sm:text-lg">
              {programTitle(nextEpisode.title)}
              <span className="ml-2 font-normal">
                {formatDate(nextEpisode.startAt)} {formatTime(nextEpisode.startAt)}
              </span>
            </p>
            <div className="flex flex-wrap gap-2">
              <button type="button" className={cardPrimaryButton} onClick={() => advanceToNext(nextEpisode.id)}>
                今すぐ再生
              </button>
              <button type="button" className={cardButton} onClick={() => setEndCardFor(null)}>
                取り消す
              </button>
              {trashButton}
            </div>
          </div>
        </div>
      ) : (
        <div className="flex flex-col items-center gap-3 text-center">
          <p className="text-sm text-white/70 sm:text-base">最後のエピソードです</p>
          <div className="flex flex-wrap justify-center gap-2">
            <button
              type="button"
              className={cardPrimaryButton}
              onClick={() => {
                setEndCardFor(null)
                const video = videoRef.current
                if (video) {
                  video.currentTime = 0
                  void video.play().catch(() => {})
                }
              }}
            >
              もう一度見る
            </button>
            {trashButton}
          </div>
        </div>
      )}
    </div>
  ) : null
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

  const originalPositionSeconds = useCallback((video: HTMLVideoElement) => {
    const keepRanges = frozenKeepRangesRef.current
    return playingCut && keepRanges && keepRanges.length > 0
      ? cutMsToOriginalMs(video.currentTime * 1000, keepRanges) / 1000
      : video.currentTime
  }, [playingCut])

  const rememberPosition = useCallback((video: HTMLVideoElement) => {
    if (restorePending.current) return
    const write = currentWrite(video)
    onRecordingPositionChange?.(originalPositionSeconds(video))
    if (write === null) return
    carriedPositionRef.current = { recordingId, ms: write.kind === 'put' ? write.positionMs : 0 }
  }, [currentWrite, onRecordingPositionChange, originalPositionSeconds, recordingId])

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
    if (chapterEditing && !playingCut) setTilesRequestedFor(recordingId)
  }, [chapterEditing, playingCut, recordingId])

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

  // 再生中に別の録画へ移っても境界の前後再生を残さない。
  useEffect(() => () => window.clearTimeout(playAroundTimerRef.current), [])

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
  useEffect(() => {
    const video = videoRef.current
    const frameElement = fullscreenRef.current
    const controls = frameElement?.querySelector<HTMLElement>('[data-testid="player-controls-bottom"]')
    if (!video) return
    const update = () => updateSubtitleCueLines(video, controlsVisible)
    update()

    const trackElements = Array.from(video.querySelectorAll('track[kind="subtitles"]'))
    const textTracks = Array.from(video.textTracks).filter((track) => track.kind === 'subtitles')
    const eventTrackElements = trackElements.filter((track) => typeof track.addEventListener === 'function')
    const eventTextTracks = textTracks.filter((track) => typeof track.addEventListener === 'function')
    eventTrackElements.forEach((track) => track.addEventListener('load', update))
    eventTextTracks.forEach((track) => track.addEventListener('cuechange', update))

    const observer = frameElement && controls && typeof ResizeObserver !== 'undefined'
      ? new ResizeObserver(update)
      : null
    observer?.observe(frameElement!)
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
          requestFullscreen()
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
  }, [requestFullscreen])

  // 再生できる版が無いときは呼び出し側（録画詳細）が空状態を出す。ここには来ない。
  if (profiles.length === 0) return null

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
    const rect = event.currentTarget.getBoundingClientRect()
    const tile =
      seconds === null ? null : seekTilePlacement(seconds, rect.width, event.clientX - rect.left)
    if (tile === null || tilesAvailableFor !== recordingId) {
      setTilePreview(null)
      return
    }
    setTilePreview({ recordingId, ...tile, seconds: seconds ?? 0 })
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

  const playbackControls = (
      <RecordingPlaybackControls
        recordingId={recordingId}
        profile={selectedProfile}
        encodedAssets={encodedAssets}
        nextEpisode={nextEpisode}
        outsideProgramSegments={outsideProgramSegments}
        onNextEpisodeNavigate={onNextEpisodeNavigate}
        endCard={endCard}
        currentSeconds={currentSeconds}
        durationSeconds={durationSeconds}
        playedFraction={playedFraction}
        chapters={chapterSpans}
        playingCut={playingCut}
        chapterEditing={chapterEditing}
        canEditChapters={
          !chapterEditing && !playingCut && !chapterDetectionPending && chapterVersion !== undefined &&
          onSaveChapters !== undefined && onResetChapters !== undefined
        }
        onEnterChapterEditing={onEnterChapterEditing}
        onPlayAround={editorSelected === null ? undefined : () => playAround(editorSelected)}
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
        onSelectProfile={(nextProfile) => {
          setChosenProfile(nextProfile)
          onProfileChange?.(nextProfile)
        }}
        onPreviousChapter={() => jumpChapter('prev')}
        onNextChapter={() => jumpChapter('next')}
        {...frame.controls}
        playbackRate={playbackRate}
        subtitlesEnabled={subtitlesEnabled}
        skipEnabled={skipEnabled}
        showWatched={showWatched}
        watched={watched}
        watchedPending={watchedPending}
        onPutWatched={putWatched}
        onDeleteWatched={deleteWatched}
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
        video={(
          <video
            ref={videoRef}
            key={`${recordingId}:${selectedProfile}:${keepRangesKey}`}
            {...frame.video}
            aria-label="録画映像"
            playsInline
            preload="metadata"
            src={src}
            className="absolute inset-0 size-full bg-black object-contain"
            onLoadedMetadata={(e) => {
              updatePlayedFraction(e.currentTarget)
              frame.syncFromVideo(e.currentTarget)
              setSubtitlesEnabled(Array.from(e.currentTarget.textTracks).some((track) => track.kind === 'subtitles' && track.mode === 'showing'))
              updateSubtitleCueLines(e.currentTarget, controlsVisible)
              if (autoplayRecordingRef.current === recordingId) {
                autoplayRecordingRef.current = null
                void e.currentTarget.play().catch(() => {})
              }
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
              playingRef.current = true
              setEndCardFor(null)
              frame.onPlay()
            }}
            onPause={(e) => {
              // 自然終端では ended の直前に pause が来る（ended 状態）。それは再生中だった扱いのまま残す。
              if (!e.currentTarget.ended) playingRef.current = false
              frame.onPause()
              saveCurrentPosition(e.currentTarget)
            }}
            onEnded={(e) => {
              if (chapterEditing) return
              if (onRecordingPlaybackEnded?.(originalPositionSeconds(e.currentTarget), playingRef.current) === true) return
              setCountdownSeconds(AUTO_ADVANCE_SECONDS)
              setEndCardFor(recordingId)
            }}
            onError={(e) => {
              onRecordingPlaybackError?.(
                e.currentTarget.currentTime > 0 ? originalPositionSeconds(e.currentTarget) : undefined,
                !e.currentTarget.paused,
              )
            }}
            onVolumeChange={(e) => {
              frame.onVolumeChange(e.currentTarget)
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
  )

  const editorOpen =
    chapterEditing && !playingCut && chapterVersion !== undefined &&
    onSaveChapters !== undefined && onResetChapters !== undefined

  // **`playbackControls`（`<video>` を含む）は編集の出入りで同じ木の位置に置く。** 編集用に別の木へ
  // 描き直すと `<video>` が作り直され、再生位置が 0 に戻って止まる。編集の部品は兄弟として足すだけ。
  return (
    <section className={cn(editorOpen ? 'min-w-0' : 'flex flex-col gap-2', className)} aria-label={editorOpen ? 'チャプターを直す' : '再生'}>
      <div
        data-testid={editorOpen ? 'chapter-edit-layout' : undefined}
        className={editorOpen
          ? 'grid h-[calc(100dvh-var(--page-header-height,72px)-var(--sticky-banners-height,0px)-var(--bottom-nav-height,0px)-1rem)] min-h-0 grid-cols-1 grid-rows-[auto_auto_minmax(0,1fr)] gap-3 overflow-hidden md:h-auto md:grid-cols-[minmax(0,1.65fr)_minmax(20rem,0.9fr)] md:grid-rows-[auto_auto] md:overflow-visible'
          : 'contents'}
      >
        <div data-testid={editorOpen ? 'chapter-edit-player' : undefined} className={editorOpen ? 'min-w-0 md:col-start-1 md:row-start-1' : 'contents'}>
          {playbackControls}
        </div>
        {editorOpen && (
          <RecordingChapterEditor
            key={recordingId}
            spans={chapters ?? []}
            version={chapterVersion}
            detectionPending={false}
            source={chapterSource}
            recordingId={recordingId}
            currentSeconds={currentSeconds}
            durationSeconds={durationSeconds}
            tilesAvailable={tilesAvailableFor === recordingId}
            onTileImageLoad={() => setTilesAvailableFor(recordingId)}
            onTileImageError={() => {
              setTilesAvailableFor((current) => (current === recordingId ? null : current))
              setTilePreview(null)
            }}
            playAround={playAround}
            jumpTo={jumpTo}
            onSelectedBoundaryChange={setEditorSelected}
            onSave={onSaveChapters}
            onReset={async () => await onResetChapters()}
            pending={chapterSavePending}
            commandsRef={resolvedChapterEditorCommandsRef}
            onStatusChange={chapterEditorStatusChange}
          />
        )}
      </div>
      {playingCut && selectedAsset?.cutStale === true && (
        <div className="flex flex-wrap items-center gap-2 rounded border border-warning/50 bg-warning/10 px-3 py-2">
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

      {!playingCut && chapterDetectionPending && (
        <p className="text-muted-foreground" data-testid="chapter-detecting">
          CM を検出しています。終わるまでチャプター編集は開けません
        </p>
      )}
    </section>
  )
}
