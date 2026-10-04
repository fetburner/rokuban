import { useCallback, useEffect, useMemo, useRef, useState, type MutableRefObject } from 'react'

import type { ChapterSpan, RecordingChaptersSource } from '@/api/generated'
import { Button } from '@/components/ui/button'
import { RecordingChapterFilmstrip } from '@/components/recording-chapter-filmstrip'
import {
  chapterBoundaries,
  chapterBoundaryMsToSeekSeconds,
  displayedFrameBoundaryMs,
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
  durationSeconds: number
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  playAround: (seconds: number) => void
  jumpTo: (seconds: number) => void
  /** 選んでいる境界（既定は再生位置に最も近い境界）。映像側の「前後 3 秒を再生」が使う。 */
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
  durationSeconds,
  tilesAvailable,
  onTileImageLoad,
  onTileImageError,
  playAround,
  jumpTo,
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
  const durationMs = Number.isFinite(durationSeconds) && durationSeconds > 0
    ? Math.round(durationSeconds * 1000)
    : 0
  // 描画用（ボタンの無効判定）は currentTime の floor、押した瞬間の値は mediaTime 優先。
  const currentMs = displayedFrameBoundaryMs(null, currentSeconds)
  const nowMs = () => displayedFrameBoundaryMs(getDisplayedFrameSeconds(), currentSeconds)
  const nearestBoundary = boundaries.length === 0
    ? null
    : boundaries.reduce((best, candidate) =>
      Math.abs(candidate - currentSeconds) < Math.abs(best - currentSeconds) ? candidate : best,
    )
  // nudgeBoundary stores integer milliseconds, while frame controls pass a rational
  // 29.97fps interval. Match within 1ms so the chosen boundary stays selected.
  const selectedBoundary = boundaries.find((boundary) =>
    selectedBoundaryValue !== null && Math.abs(boundary - selectedBoundaryValue) <= 0.001,
  ) ?? nearestBoundary

  const pushHistory = (snapshot: ChapterSpan[]) => {
    setHistory((current) => [...current, snapshot.map((span) => ({ ...span }))])
  }

  const replaceDraft = (next: ChapterSpan[]) => {
    draftRef.current = next
    setDraft(next)
  }

  const commitDraft = (next: ChapterSpan[], operatedIndexes: readonly number[] = []) => {
    const normalized = normalizeChapterDraft(next, operatedIndexes)
    if (sameSpans(draftRef.current, normalized)) return
    pushHistory(draftRef.current)
    replaceDraft(normalized)
  }

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

  // ← → は編集画面のどこにフォーカスがあっても前後の境界へ移る（入力欄・シークバー・メニューを除く）。
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
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || boundaries.length === 0) return
      if (editingTarget) return
      event.preventDefault()
      const index = selectedBoundary === null ? -1 : boundaries.indexOf(selectedBoundary)
      const next = event.key === 'ArrowRight' ? index + 1 : (index < 0 ? 0 : index - 1)
      setSelectedBoundaryValue(boundaries[Math.max(0, Math.min(boundaries.length - 1, next))])
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [boundaries, history, selectedBoundary, undo])

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
          spans={draft}
          selectedBoundary={selectedBoundary}
          tilesAvailable={tilesAvailable}
          onTileImageLoad={onTileImageLoad}
          onTileImageError={onTileImageError}
          onSeek={jumpTo}
          onSelectBoundary={setSelectedBoundaryValue}
          onChangeSpans={commitDraft}
          onPlayAround={playAround}
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
                      setSelectedBoundaryValue(boundaryMs / 1000)
                      jumpTo(chapterBoundaryMsToSeekSeconds(boundaryMs))
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
