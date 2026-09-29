import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'

import type { ChapterSpan, EncodedAsset, RecordingChaptersSource } from '@/api/generated'
import { RecordingChapterEditor } from '@/components/recording-chapter-editor'
import { Button } from '@/components/ui/button'
import {
  PLAY_AROUND_SECONDS,
  chapterJumpTarget,
  formatChaptersTime,
  loadChapterSkip,
  saveChapterSkip,
  skipTarget,
} from '@/lib/chapters'
import { formatBytes } from '@/lib/format'
import {
  applyPlaybackRate,
  loadPlaybackPosition,
  loadPlaybackRate,
  recordingFileURL,
  recordingSubtitleURL,
  savePlaybackPosition,
  savePlaybackRate,
  shouldSavePlaybackPosition,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'
import {
  SEEK_TILES_DISPLAY_HEIGHT,
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileAt,
  seekTileBackgroundSize,
  seekTilesURL,
} from '@/lib/seek-tiles'

type RecordingPlayerProps = {
  recordingId: number
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
  /** 原本 TS の実サイズ。`hasOriginal` のときだけ渡され、ダウンロード / VLC リンクに常置する。 */
  originalSizeBytes?: number
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
  onSaveChapters?: (spans: ChapterSpan[], version: string) => void
  /** 所有を捨てて自動層へ戻す。 */
  onResetChapters?: () => void
  /** 保存 / 取り消しの実行中。 */
  chapterSavePending?: boolean
  className?: string
}

/**
 * RecordingPlayer は encoded 派生物をネイティブ video 要素で再生する。
 * MP4 progressive + Range（streamer）。位置は localStorage（サーバー履歴なし）。
 */
export function RecordingPlayer({
  recordingId,
  preferredProfile,
  encodedAssets,
  hasOriginal = false,
  originalSizeBytes,
  chapters,
  chapterSource = 'auto',
  chapterVersion,
  chapterDetectionPending = false,
  onSaveChapters,
  onResetChapters,
  chapterSavePending = false,
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
  const [playbackRate, setPlaybackRate] = useState(loadPlaybackRate)
  const videoRef = useRef<HTMLVideoElement>(null)
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
  const chapterSpans = useMemo(() => chapters ?? [], [chapters])
  const shownPreview =
    tilePreview?.recordingId === recordingId && tilesAvailableFor === recordingId ? tilePreview : null
  // プロファイル切替時に load したあとだけ currentTime を復元する
  const restorePending = useRef(true)
  // timeupdate 間引き用: 直近に保存した Math.floor(currentTime)。null は未保存
  const lastSavedSecond = useRef<number | null>(null)

  useEffect(() => {
    restorePending.current = true
    lastSavedSecond.current = null
    previousSecondsRef.current = 0
    skipSuppressedRef.current = false
    playAroundStopRef.current = null
  }, [recordingId, selectedProfile])

  // 境界の前後再生のタイマーを残さない（再生中に別の録画へ移っても止まる）。
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

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const video = videoRef.current
      if (!video || event.ctrlKey || event.metaKey || event.altKey) return
      if (
        event.target instanceof Element &&
        event.target.closest('input, textarea, select, button, a, video, [contenteditable]')
      ) {
        return
      }

      const key = event.key.toLowerCase()
      const seekBy = (seconds: number) => {
        const target = Math.max(0, video.currentTime + seconds)
        video.currentTime = Number.isFinite(video.duration)
          ? Math.min(video.duration, target)
          : target
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
          void video.requestFullscreen?.()
          break
        default:
          if (/^[0-9]$/.test(key) && Number.isFinite(video.duration)) {
            video.currentTime = (video.duration * Number(key)) / 10
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
              className="text-primary underline-offset-2 hover:underline"
            >
              VLC 等で開く{originalSizeBytes !== undefined && ` (${formatBytes(originalSizeBytes)})`}
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
  const selectedAsset = encodedAssets.find((a) => a.profile === selectedProfile)
  // EncodedAsset に container 列がないため、プロファイルから拡張子を推測せず、
  // ダウンロード名は提案どおり .mp4 に固定する。保存されるデータ自体には影響しない。
  const downloadFilename = `recording-${recordingId}-${selectedProfile}.mp4`
  const encodedDownloadLink = (
    <a
      href={src}
      download={downloadFilename}
      aria-label="encoded 動画をダウンロード"
      className="text-primary underline-offset-2 hover:underline"
    >
      ダウンロード
    </a>
  )
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
  // スクラブ帯の上のポインタ位置 → 再生位置（秒）。duration 未確定なら null。
  // **プレビューはネイティブ controls のシークバーに重ねない。** ネイティブの
  // シークバーは位置も幅もブラウザごとに違い（Firefox は再生ボタンと音量の間、
  // Chrome も左右に余白がある）、外から測れないので、重ねると「見えたタイル」と
  // 「クリックで飛ぶ先」がずれる。座標を自分で持つ帯なら両者は同じ式から出る。
  const scrubSeconds = (event: ReactMouseEvent<HTMLDivElement>): number | null => {
    const video = videoRef.current
    if (!video || !Number.isFinite(video.duration) || video.duration <= 0) return null
    const rect = event.currentTarget.getBoundingClientRect()
    if (rect.width <= 0) return null
    const fraction = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width))
    return fraction * video.duration
  }
  const handleScrubMove = (event: ReactPointerEvent<HTMLDivElement>) => {
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
    setTilePreview({ recordingId, ...tile, left, scale })
  }
  const handleScrubClick = (event: ReactMouseEvent<HTMLDivElement>) => {
    const video = videoRef.current
    const seconds = scrubSeconds(event)
    if (!video || seconds === null) return
    video.currentTime = seconds
    updatePlayedFraction(video)
  }

  return (
    <section className={cn('flex flex-col gap-2', className)} aria-label="再生">
      {profiles.length > 1 ? (
        <div className="flex flex-wrap items-center gap-2">
          <label htmlFor={`profile-${recordingId}`} className="text-muted-foreground">
            プロファイル
          </label>
          <select
            id={`profile-${recordingId}`}
            value={selectedProfile}
            onChange={(e) => setProfile(e.target.value)}
            className="rounded border border-border bg-background px-2 py-1 text-xs"
          >
            {encodedAssets.map((a) => (
              <option key={a.profile} value={a.profile}>
                {assetOptionLabel(a)}
              </option>
            ))}
          </select>
          {encodedDownloadLink}
        </div>
      ) : (
        // 選択肢が 1 つ（= セレクタを出さない）でも、押す前にサイズを見せる
        // という値札の方針（issue #236 M7-3）は変わらないので、常にキャプション
        // として出す。サイズが取れない資産でも選択肢（プロファイル名）自体は
        // 隠さない --- 分類の失敗で機能を隠さないというドロップ統計と同じ判断。
        <div className="flex flex-wrap items-center gap-2">
          {selectedAsset && (
            <p className="text-muted-foreground">{assetOptionLabel(selectedAsset)}</p>
          )}
          {encodedDownloadLink}
        </div>
      )}

      <div className="flex max-w-3xl flex-col gap-1">
        <video
          ref={videoRef}
          key={`${recordingId}:${selectedProfile}`}
          controls
          playsInline
          preload="metadata"
          src={src}
        // tabIndex は明示しない。実 Chromium で測ったところ `<video controls>` は
        // tabindex 無しでもそれ自体が唯一の Tab stop になっており（native controls
        // の個々のボタンは Tab stop ではない）、`tabIndex={-1}` を付けると逆に
        // Tab 順から完全に外れてキーボード到達性を落とす退行になった（実測: 対照
        // ページで `<button> <video controls> <video controls tabindex=-1> <button>`
        // の Tab 順が `VIDEO(tabindex無し) -> 次のbutton`。展開後に Tab を押しても
        // 一度も VIDEO に止まらないことを確認した）。`.focus()` はこの属性が無くても
        // 実 Chromium では効く（同じく実測）。以前ここに書いていた
        // 「個々のボタンにフォーカスを持つため tabindex は不要」という理屈は
        // 測らずに書いた誤りだった（CLAUDE.md「測っていない挙動を断言しない」）。
        className="aspect-video w-full max-w-3xl rounded bg-black"
        onLoadedMetadata={(e) => {
          updatePlayedFraction(e.currentTarget)
          if (!restorePending.current) return
          restorePending.current = false
          const pos = loadPlaybackPosition(recordingId, selectedProfile)
          if (pos !== null && pos > 0) {
            e.currentTarget.currentTime = pos
          }
        }}
        onSeeking={(e) => {
          // シークの処理順は seeking → timeupdate → seeked（HTML spec）なので、
          // seeked で直前位置を更新しても最初の timeupdate には間に合わない。再生中に
          // 区間の手前から中へシークすると、シーク前の位置が直前位置のまま残って
          // 「先頭を跨いだ」と誤認し追い出す。seeking の時点で currentTime はシーク先。
          previousSecondsRef.current = e.currentTarget.currentTime
        }}
        onSeeked={(e) => {
          // 手動シーク（ネイティブ controls のシークバー・キー操作）でも直前位置を
          // 更新する。区間の中へシークした場合に「先頭を跨いだ」と誤認して
          // 追い出さないため（`lib/chapters.ts` の skipTarget）。
          previousSecondsRef.current = e.currentTarget.currentTime
          updatePlayedFraction(e.currentTarget)
        }}
        onTimeUpdate={(e) => {
          const v = e.currentTarget
          updatePlayedFraction(v)
          const previous = previousSecondsRef.current
          previousSecondsRef.current = v.currentTime
          const stopAt = playAroundStopRef.current
          if (stopAt !== null && v.currentTime >= stopAt) finishPlayAround(v)
          // 自動スキップ。**通常の再生で区間の先頭に差し掛かったときだけ**飛ばす。
          if (skipEnabled && !skipSuppressedRef.current && !v.paused) {
            const target = skipTarget(chapterSpans, previous, v.currentTime, v.duration)
            if (target !== undefined) {
              v.currentTime = target
              previousSecondsRef.current = target
            }
          }
          // timeupdate は約 4Hz で発火するが保存値は秒単位なので、秒が変わったときだけ書く
          if (!shouldSavePlaybackPosition(lastSavedSecond.current, v.currentTime)) return
          lastSavedSecond.current = Math.floor(v.currentTime)
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
        }}
        onPause={(e) => {
          const v = e.currentTarget
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
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
        />
        </video>

        {/*
          シークプレビュー用のスクラブ帯。ポインタ操作の補助なので aria-hidden にする
          （キーボード・支援技術の経路はネイティブ controls のシークバーが持つ）。
        */}
        <div
          aria-hidden="true"
          data-testid="seek-scrub"
          className="relative h-4 cursor-pointer"
          onPointerMove={handleScrubMove}
          onPointerLeave={() => setTilePreview(null)}
          onClick={handleScrubClick}
        >
          {/*
            チャプターの目盛り。**座標はこの帯が自分で持つ**（ネイティブ controls の
            シークバーには重ねない。docs/frontend/recordings.md）。位置の割合は
            `<video>.duration` を分母にする --- 区間の隙間は本編で、その終端が
            動画の終端だからである。
          */}
          {chapterSpans.length > 0 && durationSeconds > 0 && (
            <div className="pointer-events-none absolute inset-x-0 top-0 h-1">
              {chapterSpans.map((span) => {
                const left = (span.startMs / 1000 / durationSeconds) * 100
                const width = ((span.endMs - span.startMs) / 1000 / durationSeconds) * 100
                return (
                  <div
                    key={`${span.startMs}-${span.endMs}`}
                    data-testid="chapter-marker"
                    data-cut={span.cut ? 'true' : 'false'}
                    title={`${span.label ?? (span.cut ? 'CM' : 'チャプター')} ${formatChaptersTime(
                      span.startMs / 1000,
                    )}–${formatChaptersTime(span.endMs / 1000)}`}
                    className={`absolute inset-y-0 rounded-full ${span.cut ? 'bg-warning' : 'bg-primary/60'}`}
                    style={{ left: `${left}%`, width: `${width}%` }}
                  />
                )
              })}
            </div>
          )}
          <div className="absolute inset-x-0 top-1/2 h-1.5 -translate-y-1/2 overflow-hidden rounded-full bg-muted">
            <div className="h-full bg-primary" style={{ width: `${playedFraction * 100}%` }} />
          </div>
          {tilesRequestedFor === recordingId && (
            <img
              src={seekTilesURL(recordingId)}
              alt=""
              className="pointer-events-none absolute size-px opacity-0"
              onLoad={() => setTilesAvailableFor(recordingId)}
              onError={() => {
                setTilesAvailableFor((current) => (current === recordingId ? null : current))
                setTilePreview(null)
              }}
            />
          )}
          {shownPreview && (
            <div
              data-testid="seek-tile-preview"
              className="pointer-events-none absolute bottom-full z-10 mb-1 origin-bottom-left overflow-hidden rounded border border-border bg-black shadow-lg"
              style={{
                left: shownPreview.left,
                width: SEEK_TILES_DISPLAY_WIDTH,
                height: SEEK_TILES_DISPLAY_HEIGHT,
                transform: shownPreview.scale < 1 ? `scale(${shownPreview.scale})` : undefined,
              }}
            >
              <div
                className="h-full w-full bg-no-repeat"
                style={{
                  backgroundImage: `url(${seekTilesURL(recordingId)})`,
                  backgroundPosition: `${shownPreview.x}px ${shownPreview.y}px`,
                  backgroundSize: seekTileBackgroundSize(),
                }}
              />
            </div>
          )}
        </div>
      </div>

      {/* チャプター一覧と移動。区間が 1 つも無ければ「機能しないコントロールは
          置かない」の規律でセクションごと出さない（CM 無しの録画がこれに当たる）。 */}
      {chapterSpans.length > 0 && (
        <div className="flex max-w-3xl flex-col gap-2" aria-label="チャプター">
          <div className="flex flex-wrap items-center gap-2">
            <Button type="button" size="sm" variant="outline" onClick={() => jumpChapter('prev')}>
              前のチャプター
            </Button>
            <Button type="button" size="sm" variant="outline" onClick={() => jumpChapter('next')}>
              次のチャプター
            </Button>
            <label className="flex items-center gap-1 text-muted-foreground">
              <input
                type="checkbox"
                checked={skipEnabled}
                onChange={(event) => {
                  setSkipEnabled(event.target.checked)
                  saveChapterSkip(event.target.checked)
                }}
              />
              CM を飛ばす
            </label>
          </div>
          <ul className="flex flex-col gap-1 text-muted-foreground">
            {chapterSpans.map((span) => (
              <li key={`${span.startMs}-${span.endMs}`}>
                <button
                  type="button"
                  onClick={() => jumpTo(span.startMs / 1000)}
                  className="text-left text-primary underline-offset-2 hover:underline"
                >
                  <span>
                    {formatChaptersTime(span.startMs / 1000)}–{formatChaptersTime(span.endMs / 1000)}
                  </span>{' '}
                  {span.label ?? (span.cut ? 'CM' : 'チャプター')}
                </button>
                {span.cut && <span className="ml-2">切る</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      {onSaveChapters && onResetChapters && chapterVersion !== undefined && (
        <div className="max-w-3xl">
          <RecordingChapterEditor
            spans={chapterSpans}
            version={chapterVersion}
            detectionPending={chapterDetectionPending}
            source={chapterSource}
            currentSeconds={currentSeconds}
            playAround={playAround}
            onSave={onSaveChapters}
            onReset={onResetChapters}
            pending={chapterSavePending}
          />
        </div>
      )}

      {hasOriginal && (
        <p className="text-muted-foreground">
          原本 TS:{' '}
          <a
            href={recordingFileURL(recordingId)}
            className="text-primary underline-offset-2 hover:underline"
          >
            ダウンロード / VLC
            {originalSizeBytes !== undefined && ` (${formatBytes(originalSizeBytes)})`}
          </a>
        </p>
      )}
    </section>
  )
}

/**
 * assetOptionLabel はプロファイルセレクタの選択肢・単一プロファイル時の
 * キャプションに共通で使う表示文字列。`sizeBytes` が省略された資産でも
 * プロファイル名だけは出す（値札方針: サイズが取れないことを理由に選択肢
 * そのものを隠さない）。
 */
function assetOptionLabel(asset: EncodedAsset): string {
  return asset.sizeBytes === undefined ? asset.profile : `${asset.profile} (${formatBytes(asset.sizeBytes)})`
}
