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
 */
export function chapterBoundaryMsToSeekSeconds(boundaryMs: number): number {
  if (!Number.isFinite(boundaryMs)) return 0
  const frame = Math.max(0, Math.round(boundaryMs / (FRAME_SECONDS * 1000)))
  return (frame + 0.5) * FRAME_SECONDS
}

/** playbackSecondsToChapterBoundaryMs は表示中フレームの境界を整数 ms で返す。 */
export function playbackSecondsToChapterBoundaryMs(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return 0
  const framePosition = seconds / FRAME_SECONDS
  // t がフレーム境界ちょうどのとき、除算の浮動小数誤差で k よりわずかに小さく
  // なる場合だけ救う。許容幅は framePosition の double 丸め誤差ぶんに限る。
  const roundoff = Number.EPSILON * Math.max(1, Math.abs(framePosition)) * 4
  const frame = Math.floor(framePosition + roundoff)
  return Math.round(frame * FRAME_SECONDS * 1000)
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

/**
 * formatChaptersTime はチャプターの位置を `h:mm:ss` で返す。再生位置の表示に使う。
 */
export function formatChaptersTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00:00'
  const total = Math.floor(seconds)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${Math.floor(total / 3600)}:${pad(Math.floor((total % 3600) / 60))}:${pad(total % 60)}`
}
