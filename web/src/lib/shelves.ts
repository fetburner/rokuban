import type { RecordingShelf } from '@/api/generated'

/**
 * minShelfSize は単独の棚として並べる最小件数。これ未満の棚と値の無い棚は
 * 「その他」にまとめる。
 *
 * **これは表示の閾値であって、棚の定義ではない。** サーバーは全部の棚を返し、
 * 「その他」に入れるのは画面側の判断である（分類ルールを作るときの見出しに
 * 使う値を落とさないため）。
 *
 * 5 を選んだ理由: 自動キーは「最初の空白で切る」までの正規化しかしないので、
 * 単発の特番・再放送・改行の揺れがそのまま 1 件の棚として出る。実データ
 * （関東 + BS の 4 年 15,728 録画）では棚が 1,317 個あり、大半が数件の棚で、
 * そのまま並べると「割るべき過剰併合の棚」（最大 120 件）が埋もれる。
 * 同じ作品を続けて録っていれば 5 件は超えるので、この線で実用上は分かれる。
 * **測った値ではなく、画面を作るために選んだ値**である。
 */
export const minShelfSize = 5

/**
 * otherShelfLabel はまとめ先の見出し。「その他」はサーバーに存在しない棚なので、
 * 値ではなく表示だけの名前である（分類ルールの value には使えない）。
 */
export const otherShelfLabel = 'その他'

/** ShelfRow は画面が描く 1 行。value が null なら「その他」のまとめ行。 */
export type ShelfRow = {
  /** 棚のキー。分類ルールの value に渡す値。「その他」と NULL の棚は null。 */
  value: string | null
  /** 見出しに出す代表の録画の生タイトル。 */
  title: string
  count: number
  /** 代表の録画の id。詳細への導線に使う。 */
  representativeId: number
  /** この行がまとめ先（その他）か。 */
  isOther: boolean
}

/**
 * buildShelfRows は API の棚を画面の行へ畳む。
 *
 * 件数が {@link minShelfSize} 未満の棚と、実効シリーズを導出できなかった棚
 * （value が null）を「その他」の 1 行にまとめる。**元の並び（件数の降順）は
 * 保つ**ので、大きい棚が先に来る。
 *
 * まとめ先の件数は元の行の合計、代表はまとめた中で最も件数の多い棚の代表
 * （見出しが「その他」なので、押したときに開く先は最大の棚が自然）。
 */
export function buildShelfRows(shelves: readonly RecordingShelf[]): ShelfRow[] {
  const rows: ShelfRow[] = []
  let otherCount = 0
  let otherShelves = 0
  let otherRepresentative: RecordingShelf | undefined

  for (const shelf of shelves) {
    if (shelf.value !== undefined && shelf.value !== null && shelf.count >= minShelfSize) {
      rows.push({
        value: shelf.value,
        title: shelf.title,
        count: shelf.count,
        representativeId: shelf.representativeId,
        isOther: false,
      })
      continue
    }
    otherCount += shelf.count
    otherShelves += 1
    if (otherRepresentative === undefined || shelf.count > otherRepresentative.count) {
      otherRepresentative = shelf
    }
  }

  if (otherRepresentative !== undefined) {
    rows.push({
      value: null,
      title: `${otherShelfLabel}（${otherShelves} 棚）`,
      count: otherCount,
      representativeId: otherRepresentative.representativeId,
      isOther: true,
    })
  }
  return rows
}

/** shelfInputError は分類ルールの入力を検証し、問題があれば理由を返す。 */
export function shelfInputError(keyword: string, value: string): string | undefined {
  if (keyword.trim() === '') return 'キーワードを入力してください'
  if (value.trim() === '') return '棚のキーを入力してください'
  return undefined
}
