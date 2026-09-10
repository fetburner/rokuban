/**
 * 番組表の時間軸の縮尺を端末ごとの好みとして localStorage に保存する。
 *
 * 短い番組を選ぶ判定基準は、セルを視覚的に引き伸ばしたり隣接セルと
 * ヒット領域を重ねたりせず、時間軸全体を同じ倍率で拡大することにする。
 * これなら高さ = 放送時間の比例と同時性の表現を保ったまま、5 分セルの
 * 選択距離だけを確実に大きくできる（issue #724、docs/frontend/programs.md）。
 */

const KEY = 'rokuban:programs:grid-scale'

/** 選択肢は既定・2 倍・4 倍の 3 段階に固定し、自由な数値設定にはしない。 */
export const gridPxPerHourOptions = [120, 240, 480] as const

export type GridPxPerHour = (typeof gridPxPerHourOptions)[number]

/** 既定の時間軸倍率。30 分番組が 60px になる現行の縮尺。 */
export const defaultGridPxPerHour: GridPxPerHour = gridPxPerHourOptions[0]

function isGridPxPerHour(value: number): value is GridPxPerHour {
  return (gridPxPerHourOptions as readonly number[]).includes(value)
}

/** loadProgramsGridPxPerHour は保存済みの縮尺を返す。無い・壊れているなら undefined。 */
export function loadProgramsGridPxPerHour(): GridPxPerHour | undefined {
  try {
    const raw = localStorage.getItem(KEY)
    if (raw === null) return undefined
    const value = Number(raw)
    return isGridPxPerHour(value) ? value : undefined
  } catch {
    // private mode などで localStorage 自体が使えない場合は既定値へ戻る
    return undefined
  }
}

/** saveProgramsGridPxPerHour は時間軸の縮尺を保存する。 */
export function saveProgramsGridPxPerHour(pxPerHour: GridPxPerHour): void {
  try {
    localStorage.setItem(KEY, String(pxPerHour))
  } catch {
    // ignore
  }
}
