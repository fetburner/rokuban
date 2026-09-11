/** 番組に関する利用者向け表示名をまとめるモジュール。 */

/**
 * programTitle は番組タイトルを表示用の文字列にする。
 *
 * 番組タイトルが未設定・`null`・空文字のときは、画面間で同じ欠損表示を使う。
 * `||` による判定にすることで、既存の空文字を「番組名なし」と扱う挙動を保つ
 * （issue #730）。
 */
export function programTitle(title: string | null | undefined): string {
  return title || '（番組名なし）'
}
