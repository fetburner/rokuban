import type { RecordingShelf } from '@/api/generated'

/** ShelfRow は画面が描く 1 行。値の無い棚はシリーズを開けないため含めない。 */
export type ShelfRow = {
  /** 棚のキー。分類ルールの value に渡す値。 */
  value: string
  /** 見出しに出す代表の録画の生タイトル。 */
  title: string
  count: number
  /** 代表の録画の id。番組ハブの起点に使う。 */
  representativeId: number
}

/**
 * buildShelfRows は API の棚を画面の行へ変換する。
 *
 * API は件数によらず全棚を返す。値が NULL の棚だけは起点の実効シリーズが
 * NULL で番組ハブを開けないため画面から除外する。残りはサーバーが返した
 * 件数降順の並びをそのまま保つ。
 */
export function buildShelfRows(shelves: readonly RecordingShelf[]): ShelfRow[] {
  return shelves.flatMap((shelf): ShelfRow[] => {
    if (shelf.value === undefined || shelf.value === null) return []
    return [
      {
        value: shelf.value,
        title: shelf.title,
        count: shelf.count,
        representativeId: shelf.representativeId,
      },
    ]
  })
}

/** shelfInputError は分類ルールの入力を検証し、問題があれば理由を返す。 */
export function shelfInputError(keyword: string, value: string): string | undefined {
  if (keyword.trim() === '') return 'キーワードを入力してください'
  if (value.trim() === '') return '棚のキーを入力してください'
  return undefined
}

/**
 * valueKeyMismatch は入力した値と実効の棚キーが食い違うときの説明を返す
 * （一致・未取得なら undefined）。
 *
 * 値にも自動キーの正規化がかかり、最初の空白などで切れる。`NHK高校講座 数学I` と
 * `NHK高校講座 化学` は同じ棚キー `NHK高校講座` になり、割るつもりのルールが
 * 同じ棚に落ちる。空文字は「正規化で空になる」（ルールは作れない）。
 */
export function valueKeyMismatch(value: string, valueKey: string | undefined): string | undefined {
  if (valueKey === undefined || value.trim() === '') return undefined
  if (valueKey === '') return 'この値は棚キーになりません（記号のみなど）'
  if (valueKey === value) return undefined
  return `この値は棚キー ${valueKey} として扱われます`
}
