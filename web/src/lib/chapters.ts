/**
 * チャプター（CM とユーザー区間）の表示・スキップ・ジャンプの純関数。
 *
 * **合成関数はここに複製しない。** 有効なタイムラインの導出（フレーム境界への
 * 量子化・隙間の本編・CM 率 50% の安全弁・所有の有無による層の選択）は Go の
 * `internal/chapters/` 1 か所にあり、API がその結果を返す。TS 側は返ってきた
 * 区間を描き、再生時に飛ばすだけである（シークタイルの固定値が Go と TS の
 * 2 か所にあった轍を踏まない。docs/frontend/recordings.md）。
 *
 * **区間の隙間は本編である。** API は本編の区間を返さない（DB が持つ長さは EPG 上の
 * 番組長だけで、ファイルの実際の長さを api は知らない）。再生中の
 * `<video>.duration` がタイムラインの終端になる。
 */

import type { ChapterSpan } from '@/api/generated'

/** CHAPTER_SKIP_STORAGE_KEY は自動スキップの ON / OFF（端末ごとの好み）。 */
export const CHAPTER_SKIP_STORAGE_KEY = 'rokuban:chapter-skip'

/**
 * FRAME_SECONDS は 1 フレームぶんの秒（30000/1001 fps 固定）。
 *
 * **クライアントは fps を知る手段を持たない**（配信は progressive MP4 で、
 * プロファイルは解像度とコーデックしか出さない）。地上波・BS の実放送の大半が
 * この値なので固定する。境界映像の表示位置とフレーム刻みの目安に使う。サーバー側は
 * 保存時に必ずフレーム境界へ丸め直すため、ここが違っても保存値自体は壊れない。
 */
export const FRAME_SECONDS = 1001 / 30000

/**
 * chapterBoundaryMsToSeekSeconds は保存済みの境界 ms を、フレーム表示区間の中央へ写す。
 * 保存値は Go 側でフレーム境界へ量子化済みなので、ここではその境界が指すフレームを
 * 復元して表示位置を求めるだけである。保存値の量子化・比較は Go に任せる。
 *
 * **通す呼び出し元**: チャプターカード、前 / 次チャプター、一覧、自動スキップの着地点、
 * フィルムストリップの境界ボタン、±調整、選択境界からの再生開始、「境界まで」の停止後。
 * **通さない呼び出し元**: シークバー、再生位置を動かすキー、フィルムストリップの帯（境界以外）の
 * クリック、「前後 3 秒」と「境界まで」の開始位置。任意の再生位置を示す値を中央へ寄せると、
 * 利用者が指定した位置をずらす。
 */
export function chapterBoundaryMsToSeekSeconds(boundaryMs: number): number {
  if (!Number.isFinite(boundaryMs)) return 0
  const frame = Math.max(0, Math.round(boundaryMs / (FRAME_SECONDS * 1000)))
  return (frame + 0.5) * FRAME_SECONDS
}

/**
 * autoSkipSeekSeconds は自動スキップの飛び先（`skipTarget` の境界秒）を、境界のフレームが映る
 * シーク先へ写す。動画の長さを超える場合は長さで頭打ちにする（長さが未確定なら頭打ちしない）。
 */
export function autoSkipSeekSeconds(targetSeconds: number, durationSeconds: number): number {
  const seek = chapterBoundaryMsToSeekSeconds(Math.round(targetSeconds * 1000))
  return Number.isFinite(durationSeconds) && durationSeconds > 0 ? Math.min(seek, durationSeconds) : seek
}

/** frameToBoundaryMs はフレーム番号を保存形式の整数 ms の境界へ写す。 */
export function frameToBoundaryMs(frame: number): number {
  return Math.round(frame * FRAME_SECONDS * 1000)
}

/**
 * displayedFrameBoundaryMs は最後に表示されたフレームの `mediaTime`（秒）があれば
 * `round(mediaTime / T)` でフレーム番号にし、無ければ `currentTime` の floor に fallback する。
 * 一時停止直後の `currentTime` は表示中フレームの開始より手前に来ることがあり（測定は #1095）、
 * floor は 1 つ前のフレームを返しうるので、mediaTime を優先する。
 */
export function displayedFrameBoundaryMs(mediaSeconds: number | null, currentSeconds: number): number {
  if (mediaSeconds === null || !Number.isFinite(mediaSeconds) || mediaSeconds < 0) {
    return playbackSecondsToChapterBoundaryMs(currentSeconds)
  }
  return frameToBoundaryMs(Math.round(mediaSeconds / FRAME_SECONDS))
}

/** playbackSecondsToChapterBoundaryMs は `currentTime` の floor でフレームの境界を整数 ms で返す（mediaTime が無いときの fallback）。 */
export function playbackSecondsToChapterBoundaryMs(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return 0
  const framePosition = seconds / FRAME_SECONDS
  // t がフレーム境界ちょうどのとき、除算の浮動小数誤差で k よりわずかに小さく
  // なる場合だけ救う。許容幅は framePosition の double 丸め誤差ぶんに限る。
  const roundoff = Number.EPSILON * Math.max(1, Math.abs(framePosition)) * 4
  return frameToBoundaryMs(Math.floor(framePosition + roundoff))
}

/** NUDGE_SECONDS は ±1 秒ボタンの刻み。 */
export const NUDGE_SECONDS = 1

/** PLAY_AROUND_SECONDS は境界の前後を再生する長さ（前 3 秒 + 後 3 秒）。 */
export const PLAY_AROUND_SECONDS = 3

/** loadChapterSkip は自動スキップの設定を返す。既定は有効。 */
export function loadChapterSkip(): boolean {
  try {
    return localStorage.getItem(CHAPTER_SKIP_STORAGE_KEY) !== 'off'
  } catch {
    // private mode 等で localStorage が使えない場合は既定（有効）
    return true
  }
}

/** saveChapterSkip は自動スキップの設定を保存する。 */
export function saveChapterSkip(enabled: boolean): void {
  try {
    localStorage.setItem(CHAPTER_SKIP_STORAGE_KEY, enabled ? 'on' : 'off')
  } catch {
    // 保存できなくても再生は続く
  }
}

/** spanRangeSeconds は区間を秒に直す（API は ms で返す）。 */
export function spanRangeSeconds(span: ChapterSpan): { start: number; end: number } {
  return { start: span.startMs / 1000, end: span.endMs / 1000 }
}

/**
 * skipTarget は自動スキップの飛び先（秒）を返す。飛ばさないときは undefined。
 *
 * **飛ばすのは、通常の再生で区間の先頭に差し掛かったときだけである。** 手動の
 * シークで区間の中に入ったときは飛ばさない（見たくてシークした人を追い出さない）。
 * 判定は「直前の観測位置が区間の先頭より手前にあり、今回の観測位置が区間の中に
 * あること」で行う。シークで中に入った場合は直前の観測位置（seeked 後の位置）が
 * 既に区間の中なので、この条件を満たさない。
 *
 * 飛び先は区間の終端。区間の終端が動画の終端を越えるなら動画の終端で止める
 * （最後の区間が cut のときは再生の終端まで飛ぶ）。
 */
export function skipTarget(
  spans: ChapterSpan[],
  previousSeconds: number,
  seconds: number,
  durationSeconds: number,
): number | undefined {
  if (!Number.isFinite(seconds)) return undefined
  for (const span of spans) {
    if (!span.cut) continue
    const { start, end } = spanRangeSeconds(span)
    if (previousSeconds < start && seconds >= start && seconds < end) {
      return Number.isFinite(durationSeconds) && durationSeconds > 0
        ? Math.min(end, durationSeconds)
        : end
    }
  }
  return undefined
}

/**
 * chapterBoundaries は区間の境界（開始と終端）を昇順・重複なしで返す。
 * 前 / 次のチャプターへのジャンプと編集 UI の境界一覧に使う。
 */
export function chapterBoundaries(spans: ChapterSpan[]): number[] {
  const seconds = spans.flatMap((span) => {
    const { start, end } = spanRangeSeconds(span)
    return [start, end]
  })
  return [...new Set(seconds)].sort((a, b) => a - b)
}

/**
 * chapterJumpTarget は前 / 次の境界を返す。無ければ undefined。
 *
 * 現在位置とみなす境界は飛ばす（現在位置が境界の上にあるときに同じ場所へ
 * 戻り続けないよう、0.5 秒の遊びを置く）。
 */
export function chapterJumpTarget(
  spans: ChapterSpan[],
  seconds: number,
  direction: 'next' | 'prev',
): number | undefined {
  const boundaries = chapterBoundaries(spans)
  if (direction === 'next') {
    return boundaries.find((b) => b > seconds + 0.5)
  }
  return [...boundaries].reverse().find((b) => b < seconds - 0.5)
}

/**
 * nudgeBoundary は境界を delta 秒ずらしたドラフトを返す。
 *
 * **区間の同一性は区間そのものである**（自動チャプターの ID のような、再検出で
 * 動く id を宛先にしない）。境界は前後の区間で共有されうるので、一致する境界を
 * すべて同じ量だけ動かす。
 */
export function nudgeBoundary(spans: ChapterSpan[], boundary: number, delta: number): ChapterSpan[] {
  const targetMs = Math.round((boundary + delta) * 1000)
  const fromMs = Math.round(boundary * 1000)
  return spans.map((span) => ({
    ...span,
    startMs: span.startMs === fromMs ? targetMs : span.startMs,
    endMs: span.endMs === fromMs ? targetMs : span.endMs,
  }))
}

/** nearestChapterBoundary は boundaries のうち seconds に最も近い値を返す（同距離なら前）。空なら null。 */
export function nearestChapterBoundary(boundaries: readonly number[], seconds: number): number | null {
  let best: number | null = null
  for (const candidate of boundaries) {
    if (best === null || Math.abs(candidate - seconds) < Math.abs(best - seconds)) best = candidate
  }
  return best
}

/** chapterSpanIndexesAtBoundary は指定境界を持つ全区間の index を返す。 */
export function chapterSpanIndexesAtBoundary(spans: ChapterSpan[], boundarySeconds: number): number[] {
  const boundaryMs = Math.round(boundarySeconds * 1000)
  return spans.flatMap((span, index) =>
    span.startMs === boundaryMs || span.endMs === boundaryMs ? [index] : [],
  )
}

/**
 * normalizeChapterDraft は編集下書きの重なりを切り取り・合併で解消する。
 * operatedIndexes は今回追加・変更した区間を示し、同じ cut 状態で異なるラベルが
 * 重なったときはその区間を残す。
 *
 * 共有境界を動かすと両側の区間が operated になる。両方が異なるラベルの cut 区間として
 * 第三の区間と競合する場合は、時間上で先に始まる区間を残す。境界を両側へ適用する
 * `nudgeBoundary` の対称性を保ちつつ、入力配列の順序に頼らずタイムラインで結果を決められる。
 */
export function normalizeChapterDraft(
  spans: ChapterSpan[],
  operatedIndexes: readonly number[] = [],
): ChapterSpan[] {
  type Candidate = { span: ChapterSpan; operated: boolean; order: number }
  let candidates: Candidate[] = spans
    .map((span, order) => ({ span: { ...span }, operated: operatedIndexes.includes(order), order }))
    .filter(({ span }) => span.endMs > span.startMs)

  while (true) {
    candidates.sort((a, b) => a.span.startMs - b.span.startMs || a.span.endMs - b.span.endMs || a.order - b.order)
    let changed = false

    outer: for (let i = 0; i < candidates.length; i += 1) {
      for (let j = i + 1; j < candidates.length; j += 1) {
        const left = candidates[i]
        const right = candidates[j]
        if (right.span.startMs > left.span.endMs) break

        const overlaps = right.span.startMs < left.span.endMs
        const touches = right.span.startMs === left.span.endMs
        const sameKind = left.span.cut === right.span.cut
        const leftLabel = left.span.label ?? ''
        const rightLabel = right.span.label ?? ''
        const labelsCanMerge = leftLabel === rightLabel || leftLabel === '' || rightLabel === ''
        const labelsMatch = leftLabel === rightLabel

        if (
          sameKind &&
          ((overlaps && labelsCanMerge) || (touches && labelsMatch))
        ) {
          const label = leftLabel || rightLabel
          const mergedSpan: ChapterSpan = {
            ...left.span,
            startMs: Math.min(left.span.startMs, right.span.startMs),
            endMs: Math.max(left.span.endMs, right.span.endMs),
          }
          if (label) mergedSpan.label = label
          else delete mergedSpan.label
          candidates.splice(j, 1)
          candidates.splice(i, 1, {
            span: mergedSpan,
            operated: left.operated || right.operated,
            order: Math.min(left.order, right.order),
          })
          changed = true
          break outer
        }

        if (!overlaps) continue

        let winner: Candidate
        if (left.span.cut !== right.span.cut) {
          winner = left.span.cut ? left : right
        } else if (left.operated !== right.operated) {
          winner = left.operated ? left : right
        } else {
          // 同じラベルの区間は合併済み。編集の優先度が同じ競合では、配列の順ではなく
          // タイムラインで結果が決まるよう、先に始まる区間を残す。
          winner = left.span.startMs <= right.span.startMs ? left : right
        }
        const loser = winner === left ? right : left
        const fragments: Candidate[] = []
        // 1 フレーム未満の断片は捨てる。サーバーの量子化は最寄りのフレーム境界への丸めなので、
        // 1 フレーム以上の区間は丸めても空にならず、未満の断片は空になりうるため保存できない。
        // 対象は切り取りで生まれた断片だけで、利用者が元から持つ区間は落とさない（サーバーが拒否する）。
        const keepsFrame = (startMs: number, endMs: number) => endMs - startMs >= FRAME_SECONDS * 1000
        if (loser.span.startMs < winner.span.startMs && keepsFrame(loser.span.startMs, winner.span.startMs)) {
          fragments.push({
            ...loser,
            span: { ...loser.span, endMs: winner.span.startMs },
          })
        }
        if (loser.span.endMs > winner.span.endMs && keepsFrame(winner.span.endMs, loser.span.endMs)) {
          fragments.push({
            ...loser,
            span: { ...loser.span, startMs: winner.span.endMs },
          })
        }
        candidates.splice(j, 1)
        candidates.splice(i, 1, winner, ...fragments)
        changed = true
        break outer
      }
    }

    if (!changed) break
  }

  return candidates
    .sort((a, b) => a.span.startMs - b.span.startMs || a.span.endMs - b.span.endMs || a.order - b.order)
    .map(({ span }) => span)
}

/**
 * moveChapterBoundary は境界を delta 秒動かし、重なりを正規化した下書きと、動かした後に
 * 選ぶ境界を返す。境界の移動はすべてここを通す（正規化を素通りさせない）。
 *
 * 動かした先の境界が合併・切り取りで消えたときは、残った境界のうち最も近いものを選ぶ。
 * 消えた境界を選んだままにすると、選択もシーク先も存在しない境界を指す。
 */
export function moveChapterBoundary(
  spans: ChapterSpan[],
  fromSeconds: number,
  deltaSeconds: number,
): { spans: ChapterSpan[]; boundary: number | null } {
  const moved = normalizeChapterDraft(
    nudgeBoundary(spans, fromSeconds, deltaSeconds),
    chapterSpanIndexesAtBoundary(spans, fromSeconds),
  )
  const targetSeconds = Math.round((fromSeconds + deltaSeconds) * 1000) / 1000
  return { spans: moved, boundary: nearestChapterBoundary(chapterBoundaries(moved), targetSeconds) }
}
