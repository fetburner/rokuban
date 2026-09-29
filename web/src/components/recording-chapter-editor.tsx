import { useMemo, useState } from 'react'

import type { ChapterSpan, RecordingChaptersSource } from '@/api/generated'
import { Button } from '@/components/ui/button'
import {
  FRAME_SECONDS,
  NUDGE_SECONDS,
  chapterBoundaries,
  formatChaptersTime,
  nudgeBoundary,
} from '@/lib/chapters'

type RecordingChapterEditorProps = {
  /** サーバーが持っているタイムライン。ドラフトの初期値。 */
  spans: ChapterSpan[]
  /** `spans` の版（GET が返す）。保存時にそのまま返し、下書きの基が変わっていないかをサーバーが確かめる。 */
  version: string
  /** 検出が終端に達していない。空の `spans` は「CM 無し」ではないので編集させない。 */
  detectionPending: boolean
  /** どの層を編集しているか（表示だけに使う）。 */
  source: RecordingChaptersSource
  /** 再生位置（秒）。「現在位置を境界にする」と「ここから / ここまで」に使う。 */
  currentSeconds: number
  /** 境界の前後 3 秒を再生する。自動スキップを一時的に止めるのは呼び出し側の責務。 */
  playAround: (seconds: number) => void
  onSave: (spans: ChapterSpan[], version: string) => void
  onReset: () => void
  pending: boolean
}

const nudgeLabel = (seconds: number) => (seconds > 0 ? `+${seconds}秒` : `${seconds}秒`)

/**
 * RecordingChapterEditor はチャプターを手で直す UI。
 *
 * **本編の区間は出さない。** API も本編を返さない（区間の隙間が本編）。ここで
 * 並ぶのは CM と、ユーザーが印を付けた OP / ED などの区間だけである。
 *
 * 編集の単位は「タイムライン全体の置き換え」1 つだけに保つ。境界ごとの差分を
 * 送る形にしない理由は、境界の修正が「その時点の検出結果に対する差分」であり、
 * 再検出で境界が動くと意味を失うためである（docs/schema/recordings.md）。
 *
 * 境界は前後の区間で共有されうるので、同じ値の境界はまとめて 1 行に出す。
 */
export function RecordingChapterEditor(props: RecordingChapterEditorProps) {
  // 検出中は編集 UI を出さない。検出中の空の層を基に下書きを作ると、検出が commit
  // された後に「CM 無し」で引き取ってしまう（サーバーも版と 409 で拒否する）。
  if (props.detectionPending) {
    return (
      <section className="flex flex-col gap-2 border-t border-border/60 pt-3" aria-label="チャプターの編集">
        <h4 className="font-medium">チャプター</h4>
        <p className="text-muted-foreground" data-testid="chapter-detecting">
          CM を検出しています。終わるまでチャプターは編集できません
        </p>
      </section>
    )
  }
  return <ChapterDraftEditor {...props} />
}

function ChapterDraftEditor({
  spans,
  version,
  source,
  currentSeconds,
  playAround,
  onSave,
  onReset,
  pending,
}: RecordingChapterEditorProps) {
  const [draft, setDraft] = useState<ChapterSpan[]>(spans)
  // 下書きの基にしたサーバーの値と版。サーバーの値が変わったら（保存後の再取得・
  // 再検出・他タブの編集）、下書きが基と同じか、新しい値と同じ（自分の保存が
  // 反映された）ときだけ追随する。**それ以外は黙って捨てず** stale として
  // 知らせる（下書きは残し、保存は止める。保存は版で 409 になる）。effect では
  // なく**レンダー中の調整**にする（React の "storing information from previous
  // renders" の形）。親が `unwrap(query.data)` の配列をそのまま渡すので、参照が
  // 変わるのは新しいデータが来たときだけである。
  const [base, setBase] = useState({ spans, version })
  if (base.spans !== spans || base.version !== version) {
    if (sameSpans(draft, base.spans) || sameSpans(draft, spans)) {
      setBase({ spans, version })
      setDraft(spans)
    }
  }
  const stale = base.spans !== spans || base.version !== version
  const discardDraft = () => {
    setBase({ spans, version })
    setDraft(spans)
  }
  const [pendingStartMs, setPendingStartMs] = useState<number | null>(null)
  const boundaries = useMemo(() => chapterBoundaries(draft), [draft])
  const dirty = useMemo(() => !sameSpans(draft, base.spans), [draft, base.spans])

  const startNewSpan = () => {
    setPendingStartMs(Math.round(currentSeconds * 1000))
  }
  const currentMs = Math.round(currentSeconds * 1000)
  const closeNewSpan = () => {
    if (pendingStartMs === null) return
    const startMs = Math.min(pendingStartMs, currentMs)
    const endMs = Math.max(pendingStartMs, currentMs)
    if (endMs <= startMs) return
    // 既定は「CM と同じ扱い」= 切る。ラベルは後から入れる。ラベルが無いまま
    // cut を外す状態は DB が拒否するので、UI 側でも外させない（下の checkbox）。
    setDraft((current) => [...current, { startMs, endMs, cut: true }])
    setPendingStartMs(null)
  }

  return (
    <section className="flex flex-col gap-2 border-t border-border/60 pt-3" aria-label="チャプターの編集">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 className="font-medium">チャプター</h4>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={pending || !dirty || stale}
            onClick={() => onSave(draft, base.version)}
          >
            保存
          </Button>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            disabled={!dirty}
            onClick={discardDraft}
          >
            変更を破棄
          </Button>
          {/* 自動に戻すは「取り込み直し」ではない。やり直しはこれ 1 つだけである
              （出自と意図を 1 つの列に載せない規律。docs/schema/recordings.md）。 */}
          <Button
            type="button"
            size="sm"
            variant="secondary"
            disabled={pending || source === 'auto'}
            onClick={onReset}
          >
            自動に戻す
          </Button>
        </div>
      </div>
      <p className="text-muted-foreground" data-testid="chapter-source">
        {source === 'user' ? '確認済み（手で直した内容を使っています）' : '自動検出（未確認）'}
        {dirty && ' · 未保存の変更があります'}
      </p>
      {stale && (
        <p className="text-destructive" role="alert" data-testid="chapter-stale">
          サーバー側の内容が変わりました。下書きを破棄して最新の内容から編集し直してください
        </p>
      )}

      {boundaries.length === 0 ? (
        <p className="text-muted-foreground">チャプターはありません</p>
      ) : (
        <ul className="flex flex-col gap-1">
          {boundaries.map((boundary) => (
            <li key={boundary} className="flex flex-wrap items-center gap-1" data-testid="chapter-boundary">
              <span className="w-20 shrink-0 text-muted-foreground">
                {formatChaptersTime(boundary)}
              </span>
              <Button type="button" size="sm" variant="outline" onClick={() => playAround(boundary)}>
                前後3秒
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                aria-label={`${formatChaptersTime(boundary)} を ${nudgeLabel(-NUDGE_SECONDS)}`}
                onClick={() => setDraft((c) => nudgeBoundary(c, boundary, -NUDGE_SECONDS))}
              >
                −1秒
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                aria-label={`${formatChaptersTime(boundary)} を 1 フレーム戻す`}
                onClick={() => setDraft((c) => nudgeBoundary(c, boundary, -FRAME_SECONDS))}
              >
                −1f
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                aria-label={`${formatChaptersTime(boundary)} を 1 フレーム進める`}
                onClick={() => setDraft((c) => nudgeBoundary(c, boundary, FRAME_SECONDS))}
              >
                +1f
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                aria-label={`${formatChaptersTime(boundary)} を ${nudgeLabel(NUDGE_SECONDS)}`}
                onClick={() => setDraft((c) => nudgeBoundary(c, boundary, NUDGE_SECONDS))}
              >
                +1秒
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => setDraft((c) => nudgeBoundary(c, boundary, currentSeconds - boundary))}
              >
                現在位置
              </Button>
            </li>
          ))}
        </ul>
      )}

      {draft.length > 0 && (
        <ul className="flex flex-col gap-1">
          {draft.map((span, index) => (
            <li
              key={`${span.startMs}-${span.endMs}-${index}`}
              className="flex flex-wrap items-center gap-2"
              data-testid="chapter-span-row"
            >
              <span className="w-40 shrink-0 text-muted-foreground">
                {formatChaptersTime(span.startMs / 1000)} – {formatChaptersTime(span.endMs / 1000)}
              </span>
              <input
                type="text"
                value={span.label ?? ''}
                placeholder="ラベル（OP / ED など）"
                aria-label="ラベル"
                onChange={(event) =>
                  setDraft((current) =>
                    current.map((s, i) => (i === index ? withLabel(s, event.target.value) : s)),
                  )
                }
                className="h-8 w-40 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-none"
              />
              <label className="flex items-center gap-1">
                <input
                  type="checkbox"
                  checked={span.cut}
                  // ラベルが無い区間は本編と区別が付かない（DB の CHECK と同じ規則）。
                  // 表現できない状態を UI で作らせない。
                  disabled={(span.label ?? '') === ''}
                  title={(span.label ?? '') === '' ? 'ラベルが無い区間は切る扱いのままにする' : undefined}
                  onChange={(event) =>
                    setDraft((current) =>
                      current.map((s, i) => (i === index ? { ...s, cut: event.target.checked } : s)),
                    )
                  }
                />
                切る
              </label>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => setDraft((current) => current.filter((_, i) => i !== index))}
              >
                削除
              </Button>
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {pendingStartMs === null ? (
          <Button type="button" size="sm" variant="outline" onClick={startNewSpan}>
            ここから
          </Button>
        ) : (
          <>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={currentMs <= pendingStartMs}
              onClick={closeNewSpan}
            >
              ここまで
            </Button>
            <span className="text-muted-foreground">
              {formatChaptersTime(pendingStartMs / 1000)} から
            </span>
            <Button type="button" size="sm" variant="ghost" onClick={() => setPendingStartMs(null)}>
              取り消し
            </Button>
          </>
        )}
      </div>
    </section>
  )
}

/**
 * withLabel はラベルを差し替える。空文字は「ラベル無し」なので省略する
 * （生成型の `label` は省略可能で、空文字を載せるとサーバー側の
 * 「ラベルも無く cut でもない」判定と食い違う）。
 */
function withLabel(span: ChapterSpan, label: string): ChapterSpan {
  if (label === '') {
    const { label: _removed, ...rest } = span
    return rest
  }
  return { ...span, label }
}

/** sameSpans は区間の集合が同じかを返す（並び替えだけの差は無視する）。 */
function sameSpans(a: ChapterSpan[], b: ChapterSpan[]): boolean {
  if (a.length !== b.length) return false
  const key = (span: ChapterSpan) => `${span.startMs}:${span.endMs}:${span.label ?? ''}:${span.cut}`
  const left = a.map(key).sort()
  const right = b.map(key).sort()
  return left.every((value, index) => value === right[index])
}
