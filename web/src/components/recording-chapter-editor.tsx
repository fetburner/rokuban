import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type MutableRefObject } from 'react'

import type { ChapterSpan, RecordingChaptersSource } from '@/api/generated'
import { Button } from '@/components/ui/button'
import { RecordingChapterFilmstrip } from '@/components/recording-chapter-filmstrip'
import {
  FRAME_SECONDS,
  chapterBoundaries,
  displayedFrameBoundaryMs,
  moveChapterBoundary,
  nearestChapterBoundary,
  normalizeChapterDraft,
} from '@/lib/chapters'
import { formatPlaybackTime } from '@/lib/format'

export type ChapterEditorCommands = {
  save: () => Promise<boolean>
  reset: () => Promise<boolean>
  discard: () => void
}

export type ChapterEditorStatus = {
  source: RecordingChaptersSource
  dirty: boolean
  stale: boolean
}

type RecordingChapterEditorProps = {
  /** サーバーが持っているタイムライン。ドラフトの初期値。 */
  spans: ChapterSpan[]
  /** `spans` の版（GET が返す）。保存時にそのまま返す。 */
  version: string
  /** 検出が終端に達していない。空の `spans` は「CM 無し」ではないので編集させない。 */
  detectionPending: boolean
  /** どの層を編集しているか。 */
  source: RecordingChaptersSource
  recordingId: number
  currentSeconds: number
  /** 最後に表示されたフレームの mediaTime（秒）。取れなければ null（currentTime の floor に fallback）。 */
  getDisplayedFrameSeconds?: () => number | null
  isPlaying: boolean
  durationSeconds: number
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  jumpTo: (seconds: number) => void
  /** 選択・調整した境界のコマへ映像を連れて行き、停止する。 */
  onBoundaryAction: (seconds: number) => void
  /** 選んでいる境界（既定は再生位置に最も近い境界）。映像側の「前後 3 秒」「境界まで」「境界から」が使う。 */
  onSelectedBoundaryChange: (seconds: number | null) => void
  /** 保存は成功で resolve、失敗で reject。 */
  onSave: (spans: ChapterSpan[], version: string) => Promise<unknown>
  onReset: () => Promise<unknown>
  pending: boolean
  commandsRef: MutableRefObject<ChapterEditorCommands | null>
  onStatusChange: (status: ChapterEditorStatus) => void
}

/**
 * RecordingChapterEditor は編集専用の録画プレイヤー画面。
 *
 * 境界をストリップで選び、選択中の境界だけをフレーム単位で調整する。編集は
 * タイムライン全体の置き換えとして保存し、サーバーが返す版をそのまま使う。
 */
export function RecordingChapterEditor(props: RecordingChapterEditorProps) {
  if (props.detectionPending) {
    return (
      <p className="text-muted-foreground" data-testid="chapter-detecting">
        CM を検出しています。終わるまでチャプターは編集できません
      </p>
    )
  }
  return <ChapterDraftEditor {...props} />
}

const noDisplayedFrame = () => null

function ChapterDraftEditor({
  spans,
  version,
  source,
  recordingId,
  currentSeconds,
  getDisplayedFrameSeconds = noDisplayedFrame,
  isPlaying,
  durationSeconds,
  tilesAvailable,
  onTileImageLoad,
  onTileImageError,
  jumpTo,
  onBoundaryAction,
  onSelectedBoundaryChange,
  onSave,
  onReset,
  pending,
  commandsRef,
  onStatusChange,
}: RecordingChapterEditorProps) {
  const [draft, setDraft] = useState<ChapterSpan[]>(spans)
  const draftRef = useRef(draft)
  const labelEditRef = useRef<{ before: ChapterSpan[]; changed: boolean } | null>(null)
  const [history, setHistory] = useState<ChapterSpan[][]>([])
  // 下書きの基にしたサーバーの値と版。下書きが未変更か自分の保存結果と一致するとき
  // だけ追随する。それ以外は stale として知らせ、黙って上書きしない。
  const [base, setBase] = useState({ spans, version })
  const [adoptNext, setAdoptNext] = useState(false)
  if (base.spans !== spans || base.version !== version) {
    if (adoptNext || sameSpans(draft, base.spans) || sameSpans(draft, spans)) {
      setBase({ spans, version })
      setDraft(spans)
      setHistory([])
      setAdoptNext(false)
    }
  }
  useEffect(() => {
    draftRef.current = draft
  }, [draft])
  useEffect(() => {
    labelEditRef.current = null
  }, [base.spans, base.version])
  const stale = base.spans !== spans || base.version !== version
  const discardDraft = useCallback(() => {
    setBase({ spans, version })
    draftRef.current = spans
    setDraft(spans)
    setHistory([])
    labelEditRef.current = null
    setAdoptNext(false)
  }, [spans, version])
  const boundaries = useMemo(() => chapterBoundaries(draft), [draft])
  const dirty = useMemo(() => !sameSpans(draft, base.spans), [draft, base.spans])
  const [selectedBoundaryValue, setSelectedBoundaryValue] = useState<number | null>(null)
  const [pendingStartMs, setPendingStartMs] = useState<number | null>(null)
  // selectBoundary を安定した参照に保つ（キー処理の effect を毎レンダー貼り直さない）。
  const onBoundaryActionRef = useRef(onBoundaryAction)
  useLayoutEffect(() => {
    onBoundaryActionRef.current = onBoundaryAction
  }, [onBoundaryAction])
  // 長押しの連続送りを undo 1 件にまとめる。直前の移動が履歴を積んだか。
  const moveGestureOpenRef = useRef(false)
  const durationMs = Number.isFinite(durationSeconds) && durationSeconds > 0
    ? Math.round(durationSeconds * 1000)
    : 0
  // 描画用（ボタンの無効判定）は currentTime の floor、押した瞬間の値は mediaTime 優先。
  const currentMs = displayedFrameBoundaryMs(null, currentSeconds)
  const nowMs = () => displayedFrameBoundaryMs(getDisplayedFrameSeconds(), currentSeconds)
  const nearestBoundary = nearestChapterBoundary(boundaries, currentSeconds)
  // nudgeBoundary stores integer milliseconds, while frame controls pass a rational
  // 29.97fps interval. Match within 1ms so the chosen boundary stays selected.
  const selectedBoundary = boundaries.find((boundary) =>
    selectedBoundaryValue !== null && Math.abs(boundary - selectedBoundaryValue) <= 0.001,
  ) ?? nearestBoundary

  const pushHistory = useCallback((snapshot: ChapterSpan[]) => {
    setHistory((current) => [...current, snapshot.map((span) => ({ ...span }))])
  }, [])

  const replaceDraft = useCallback((next: ChapterSpan[]) => {
    draftRef.current = next
    setDraft(next)
  }, [])

  /** commitDraft は正規化して確定する。変化があれば true。`coalesce` は履歴を積まない。 */
  const commitDraft = useCallback((next: ChapterSpan[], operatedIndexes: readonly number[] = [], coalesce = false) => {
    const normalized = normalizeChapterDraft(next, operatedIndexes)
    if (sameSpans(draftRef.current, normalized)) return false
    if (!coalesce) pushHistory(draftRef.current)
    replaceDraft(normalized)
    return true
  }, [pushHistory, replaceDraft])

  const undo = useCallback(() => {
    if (history.length === 0) return
    const previous = history[history.length - 1]
    labelEditRef.current = null
    setHistory(history.slice(0, -1))
    draftRef.current = previous
    setDraft(previous)
  }, [history])

  useEffect(() => {
    onStatusChange({ source, dirty, stale })
  }, [dirty, onStatusChange, source, stale])

  useEffect(() => {
    onSelectedBoundaryChange(selectedBoundary)
  }, [onSelectedBoundaryChange, selectedBoundary])

  // 境界を選ぶ操作はすべてここを通り、映像を境界のコマへ連れて行って止める。
  const selectBoundary = useCallback((seconds: number) => {
    setSelectedBoundaryValue(seconds)
    onBoundaryActionRef.current(seconds)
  }, [])

  /**
   * moveBoundary は境界を delta 秒動かす唯一の経路（`,` `.`・±ボタン・長押し・ドラッグ・
   * 再生位置に合わせる）。正規化と undo 履歴を通し、動かした後の境界を選んで映像を連れて行く。
   * `continued` は長押しの 2 回目以降で、最初の 1 回が積んだ履歴 1 件にまとめる。
   * 動かした先の境界が合併で消えて別の境界を選び直したときは false を返す。連続送りはここで
   * 止める（続けると、利用者が押していない別の境界を動かし始める）。
   */
  const moveBoundary = useCallback((from: number, deltaSeconds: number, continued = false): boolean => {
    const moved = moveChapterBoundary(draftRef.current, from, deltaSeconds)
    if (commitDraft(moved.spans, [], continued && moveGestureOpenRef.current)) {
      moveGestureOpenRef.current = true
    } else if (!continued) {
      moveGestureOpenRef.current = false
    }
    if (moved.boundary === null) return false
    selectBoundary(moved.boundary)
    return Math.abs(moved.boundary - Math.round((from + deltaSeconds) * 1000) / 1000) <= 0.001
  }, [commitDraft, selectBoundary])

  // キーリピートで境界が消えたら、キーを離すまでリピートを無視する。
  const keyRepeatStoppedRef = useRef(false)

  // ← → は選択を移す。`,` / `.` は選択境界を 1 フレーム動かす。
  // 編集画面のどこにフォーカスがあっても効き、入力欄・シークバー・メニューでは効かない。
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target
      const editingTarget = target instanceof HTMLElement && (
        target.isContentEditable ||
        target.closest('input, textarea, select, [role="slider"], [role="menu"], [role="dialog"]') !== null
      )
      if (
        event.key.toLowerCase() === 'z' &&
        (event.ctrlKey || event.metaKey) &&
        !event.altKey &&
        !event.shiftKey &&
        !editingTarget &&
        history.length > 0
      ) {
        event.preventDefault()
        undo()
        return
      }
      const isArrow = event.key === 'ArrowLeft' || event.key === 'ArrowRight'
      const isFrameNudge = event.key === ',' || event.key === '.'
      if (!isArrow && !isFrameNudge) return
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || boundaries.length === 0) return
      if (editingTarget) return
      event.preventDefault()
      if (isFrameNudge) {
        if (!event.repeat) keyRepeatStoppedRef.current = false
        if (selectedBoundary === null || keyRepeatStoppedRef.current) return
        // OS のキーリピートは長押しと同じく、最初の 1 回が積んだ履歴 1 件にまとめる。
        const kept = moveBoundary(selectedBoundary, event.key === ',' ? -FRAME_SECONDS : FRAME_SECONDS, event.repeat)
        if (!kept) keyRepeatStoppedRef.current = true
        return
      }
      const index = selectedBoundary === null ? -1 : boundaries.indexOf(selectedBoundary)
      const next = event.key === 'ArrowRight' ? index + 1 : (index < 0 ? 0 : index - 1)
      const nextBoundary = boundaries[Math.max(0, Math.min(boundaries.length - 1, next))]
      if (nextBoundary !== selectedBoundary) selectBoundary(nextBoundary)
    }
    const onKeyUp = () => {
      keyRepeatStoppedRef.current = false
    }
    window.addEventListener('keydown', onKeyDown)
    window.addEventListener('keyup', onKeyUp)
    return () => {
      window.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('keyup', onKeyUp)
    }
  }, [boundaries, history, moveBoundary, selectBoundary, selectedBoundary, undo])

  const save = useCallback(async () => {
    // 自動層は変更がなくても明示的に確認できる。空の層も「CM なしで確認する」
    // 意図を持つため、ユーザーの操作で所有層へ引き取れるようにする。
    if ((!dirty && source !== 'auto') || stale || pending) return false
    try {
      await onSave(draft, base.version)
      setAdoptNext(true)
      return true
    } catch {
      return false
    }
  }, [base.version, dirty, draft, onSave, pending, source, stale])

  const reset = useCallback(async () => {
    if (pending) return false
    try {
      const result = await onReset()
      return result !== false
    } catch {
      return false
    }
  }, [onReset, pending])

  useEffect(() => {
    const commands = { save, reset, discard: discardDraft }
    commandsRef.current = commands
    return () => {
      if (commandsRef.current === commands) commandsRef.current = null
    }
  }, [commandsRef, discardDraft, reset, save])

  // duration が未確定（0）や無限（native HLS の変換中）のときは頭打ちしない。
  const currentPositionMs = () => (durationMs > 0 ? Math.min(durationMs, nowMs()) : nowMs())
  const startNewSpan = () => setPendingStartMs(currentPositionMs())
  const closeNewSpan = () => {
    if (pendingStartMs === null) return
    const now = currentPositionMs()
    const startMs = Math.min(pendingStartMs, now)
    const endMs = Math.max(pendingStartMs, now)
    if (endMs <= startMs) return
    const current = draftRef.current
    commitDraft([...current, { startMs, endMs, cut: true }], [current.length])
    setPendingStartMs(null)
  }
  const addFromStartToPosition = () => {
    const endMs = currentPositionMs()
    if (endMs <= 0) return
    const current = draftRef.current
    commitDraft([...current, { startMs: 0, endMs, cut: true }], [current.length])
  }
  const addFromPositionToEnd = () => {
    const startMs = currentPositionMs()
    const endMs = durationMs
    if (endMs <= startMs) return
    const current = draftRef.current
    commitDraft([...current, { startMs, endMs, cut: true }], [current.length])
  }

  return (
    <>
      <div className="min-w-0 md:col-span-2 md:row-start-2">
        <RecordingChapterFilmstrip
          recordingId={recordingId}
          durationSeconds={durationSeconds}
          currentSeconds={currentSeconds}
          getDisplayedFrameSeconds={getDisplayedFrameSeconds}
          isPlaying={isPlaying}
          spans={draft}
          selectedBoundary={selectedBoundary}
          tilesAvailable={tilesAvailable}
          onTileImageLoad={onTileImageLoad}
          onTileImageError={onTileImageError}
          onSeek={jumpTo}
          onSelectBoundary={selectBoundary}
          onMoveBoundary={moveBoundary}
        />
      </div>

      {/* 右の列は高さを映像の行に任せ（絶対配置）、一覧だけがスクロールする。 */}
      <div className="relative min-h-0 min-w-0 md:col-start-2 md:row-start-1">
      <section
        data-testid="chapter-span-list"
        aria-label="区間の一覧"
        className="h-full overflow-y-auto overscroll-contain rounded-md border border-border/70 p-2 md:absolute md:inset-0 md:h-auto"
      >
        <div className="mb-2 flex justify-end">
          <Button type="button" size="sm" variant="outline" onClick={undo} disabled={history.length === 0}>
            元に戻す
          </Button>
        </div>
        <p className="sr-only" data-testid="chapter-source">
          {source === 'user' ? '確認済み' : '自動検出（未確認）'}{dirty ? '・未保存の変更があります' : ''}
        </p>
        {stale && (
          <div className="mb-2 rounded border border-destructive/40 bg-destructive/5 p-2" data-testid="chapter-stale" role="alert">
            <p>サーバー側の内容が変わりました。下書きを破棄して最新の内容から編集し直してください</p>
            <Button type="button" size="sm" variant="outline" className="mt-2" onClick={discardDraft}>
              下書きを破棄して最新から編集し直す
            </Button>
          </div>
        )}

        {draft.length === 0 ? (
          <p className="p-3 text-sm text-muted-foreground">チャプターはありません</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {draft.map((span, index) => {
              const selected = selectedBoundary === span.startMs / 1000 || selectedBoundary === span.endMs / 1000
              return (
                <li
                  key={`${span.startMs}-${span.endMs}-${index}`}
                  data-testid="chapter-span-row"
                  data-selected={selected}
                  className={`relative grid grid-cols-[minmax(0,1fr)_auto] gap-x-2 gap-y-1 rounded-lg border py-2 pr-2 pl-5 ${selected ? 'border-foreground ring-1 ring-foreground' : 'border-border'}`}
                >
                  <span
                    aria-hidden="true"
                    className={`absolute inset-y-2 left-2 w-1 rounded-full ${span.cut ? 'bg-chapter-cut' : 'bg-muted-foreground/40'}`}
                  />
                  <button
                    type="button"
                    className="col-span-2 flex min-h-8 items-center text-left font-mono text-sm text-muted-foreground"
                    aria-label={`${formatPlaybackTime(span.startMs / 1000)} から ${formatPlaybackTime(span.endMs / 1000)} の境界を選ぶ`}
                    onClick={() => {
                      const startMs = span.startMs
                      const endMs = span.endMs
                      const boundaryMs = Math.abs(currentSeconds - startMs / 1000) <= Math.abs(currentSeconds - endMs / 1000)
                        ? startMs
                        : endMs
                      selectBoundary(boundaryMs / 1000)
                    }}
                  >
                    {formatPlaybackTime(span.startMs / 1000)} – {formatPlaybackTime(span.endMs / 1000)}
                  </button>
                  <input
                    type="text"
                    value={span.label ?? ''}
                    placeholder="ラベル（OP / ED など）"
                    aria-label="ラベル"
                    onFocus={() => {
                      labelEditRef.current = { before: draftRef.current.map((item) => ({ ...item })), changed: false }
                    }}
                    onChange={(event) => {
                      const current = draftRef.current
                      const next = current.map((item, i) => (
                        i === index ? withLabel(item, event.target.value) : item
                      ))
                      draftRef.current = next
                      setDraft(next)
                      if (labelEditRef.current) labelEditRef.current.changed = true
                    }}
                    onBlur={() => {
                      const edit = labelEditRef.current
                      labelEditRef.current = null
                      if (edit?.changed && !sameSpans(edit.before, draftRef.current)) pushHistory(edit.before)
                    }}
                    className="h-9 min-w-0 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-none"
                  />
                  <div className="flex flex-col items-end gap-1">
                    <label className="flex items-center gap-2 text-sm text-muted-foreground">
                      <span>切る</span>
                      <input
                        type="checkbox"
                        checked={span.cut}
                        disabled={(span.label ?? '') === ''}
                        title={(span.label ?? '') === '' ? 'ラベルが無い区間は切る扱いのままにする' : undefined}
                        onChange={(event) => {
                          const current = draftRef.current
                          commitDraft(current.map((item, i) => (
                            i === index ? { ...item, cut: event.target.checked } : item
                          )), [index])
                        }}
                        className="peer sr-only"
                      />
                      <span
                        aria-hidden="true"
                        className="relative inline-flex h-6 w-11 shrink-0 rounded-full bg-muted transition-colors after:absolute after:top-0.5 after:left-0.5 after:size-5 after:rounded-full after:bg-background after:shadow after:transition-transform peer-checked:bg-chapter-cut peer-checked:after:translate-x-5 peer-focus-visible:outline-2 peer-focus-visible:outline-ring peer-focus-visible:outline-offset-2"
                      />
                    </label>
                    <Button type="button" size="sm" variant="link" className="h-7 px-1" onClick={() => {
                      const current = draftRef.current
                      commitDraft(current.filter((_, i) => i !== index))
                    }}>
                      削除
                    </Button>
                  </div>
                </li>
              )
            })}
          </ul>
        )}

        <div className="mt-2 flex flex-wrap items-center gap-2 border-t border-border pt-2">
          {pendingStartMs === null ? (
            <>
              <Button type="button" size="sm" variant="outline" onClick={startNewSpan}>
                ここから区間を足す
              </Button>
              <Button type="button" size="sm" variant="outline" onClick={addFromStartToPosition} disabled={currentMs <= 0}>
                最初からここまで切る
              </Button>
              <Button type="button" size="sm" variant="outline" onClick={addFromPositionToEnd} disabled={durationMs <= currentMs}>
                ここから最後まで切る
              </Button>
              <span className="text-xs text-muted-foreground">再生位置から始まる区間を作ります</span>
            </>
          ) : (
            <>
              <Button type="button" size="sm" variant="outline" disabled={currentMs <= pendingStartMs} onClick={closeNewSpan}>
                ここまで
              </Button>
              <span className="text-xs text-muted-foreground">{formatPlaybackTime(pendingStartMs / 1000)} から</span>
              <Button type="button" size="sm" variant="ghost" onClick={() => setPendingStartMs(null)}>取り消し</Button>
            </>
          )}
        </div>
      </section>
      </div>
    </>
  )
}

/** withLabel はラベルが空なら省略し、DB と同じ「ラベルなし cut=false」を作らせない。 */
function withLabel(span: ChapterSpan, label: string): ChapterSpan {
  if (label === '') {
    const { label: _removed, ...rest } = span
    return rest
  }
  return { ...span, label }
}

/** sameSpans は並び替えだけの差を無視して区間の集合を比較する。 */
function sameSpans(a: ChapterSpan[], b: ChapterSpan[]): boolean {
  if (a.length !== b.length) return false
  const key = (span: ChapterSpan) => `${span.startMs}:${span.endMs}:${span.label ?? ''}:${span.cut}`
  const left = a.map(key).sort()
  const right = b.map(key).sort()
  return left.every((value, index) => value === right[index])
}
