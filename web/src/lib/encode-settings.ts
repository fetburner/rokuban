import type { ProgramOverridesInput, ReservationOverrides } from '@/api/generated'

/**
 * encodeProfiles / keepOriginal のクライアント側検証。
 *
 * API は `keepOriginal: until_encoded` かつ encodeProfiles 空で 400 を返す。
 * ルール作成/更新は rules.go の validateRuleInput がリクエストそのものを見る。
 * 予約 overrides（PATCH .../overrides）は program_overrides.go が
 * 「既存 override + このパッチ + ルールの base」をマージした実効値を見る
 * （issue #104。`{"keepOriginal":"until_encoded"}` だけを送る、あるいは
 * `reset:["encodeProfiles"]` で戻す経路もマージ後の値で弾かれる）。UI でも
 * 同じ制約を先に防ぎ、送れない理由をボタン横に出す（検索フォームの
 * draftError と同じ流儀）。
 */

/** KeepOriginal は API の enum と同形。 */
export type KeepOriginal = 'always' | 'until_encoded'

/**
 * EncodeSettingsValue はエンコード設定フォーム（`components/encode-settings-fields.tsx`）の値。
 *
 * ルール作成/編集・予約 overrides・番組表からの予約導線（issue #132）が
 * すべてこの同じ形を使う。
 */
export type EncodeSettingsValue = {
  keepOriginal: KeepOriginal
  encodeProfiles: string[]
}

/** defaultEncodeSettingsValue は「override も rule の base も無い」既定値。 */
export function defaultEncodeSettingsValue(): EncodeSettingsValue {
  return { keepOriginal: 'always', encodeProfiles: [] }
}

/**
 * encodeSettingsValueFromOverrides は `program_overrides` jsonb（`Reservation.overrides`
 * として届く生の値）からフォームの初期値を作る。未設定のフィールドは既定値
 * （`always` / 空配列）にする。
 */
export function encodeSettingsValueFromOverrides(
  overrides: ReservationOverrides | null | undefined,
): EncodeSettingsValue {
  const keepRaw = overrides?.['keepOriginal']
  const keepOriginal: KeepOriginal =
    keepRaw === 'until_encoded' || keepRaw === 'always' ? keepRaw : 'always'
  const profilesRaw = overrides?.['encodeProfiles']
  const encodeProfiles = Array.isArray(profilesRaw)
    ? profilesRaw.filter((p): p is string => typeof p === 'string')
    : []
  return { keepOriginal, encodeProfiles }
}

/**
 * hasEncodeOverride は overrides jsonb が keepOriginal / encodeProfiles の
 * どちらかを実際に持っているか。「予約がまだ無い番組」でも同じロジックが
 * 動くよう、`Reservation` 型ではなく overrides の値そのものを引数に取る
 * （issue #132 の罠）。
 */
export function hasEncodeOverride(overrides: ReservationOverrides | null | undefined): boolean {
  if (overrides === undefined || overrides === null) return false
  return overrides['keepOriginal'] !== undefined || overrides['encodeProfiles'] !== undefined
}

/**
 * sameEncodeSettingsValue は 2 つの値が同じ設定を表すか。`encodeProfiles` は
 * `toggleProfile` が末尾に足すだけで順序に意味を持たせていないため、
 * 集合として比較する（順序違いだけで「変更あり」と誤判定しない）。
 */
export function sameEncodeSettingsValue(a: EncodeSettingsValue, b: EncodeSettingsValue): boolean {
  if (a.keepOriginal !== b.keepOriginal) return false
  if (a.encodeProfiles.length !== b.encodeProfiles.length) return false
  const set = new Set(b.encodeProfiles)
  return a.encodeProfiles.every((p) => set.has(p))
}

/**
 * encodeSettingsOverridesBody は `value` が `baseline` から変わっていなければ
 * `undefined` を返す。
 *
 * 「予約」操作は既定のままなら overrides の PATCH を一切呼ばない ---
 * 呼んでしまうと「何も主張していない」override 行を作ってしまう
 * （不変条件 10。CLAUDE.md「意味を持たない行を作らない」）。
 */
export function encodeSettingsOverridesBody(
  value: EncodeSettingsValue,
  baseline: EncodeSettingsValue,
): ProgramOverridesInput | undefined {
  if (sameEncodeSettingsValue(value, baseline)) return undefined
  return { keepOriginal: value.keepOriginal, encodeProfiles: value.encodeProfiles }
}

/**
 * encodeSettingsError は keepOriginal と encodeProfiles の組み合わせが
 * 送れないときの理由。送れるなら undefined。
 */
export function encodeSettingsError(
  keepOriginal: KeepOriginal,
  encodeProfiles: readonly string[],
): string | undefined {
  if (keepOriginal === 'until_encoded' && encodeProfiles.length === 0) {
    return 'エンコード後に原本を削除するには、プロファイルを 1 つ以上選んでください'
  }
  return undefined
}

/** keepOriginalLabel は UI 表示用のラベル。 */
export function keepOriginalLabel(value: KeepOriginal): string {
  switch (value) {
    case 'always':
      return '常に保持'
    case 'until_encoded':
      return 'エンコード後に削除'
  }
}

/**
 * toggleProfile は選択集合に name を足す / 外す。順序は維持し、
 * 新しく足す名前は末尾に置く。
 */
export function toggleProfile(selected: readonly string[], name: string): string[] {
  if (selected.includes(name)) {
    return selected.filter((n) => n !== name)
  }
  return [...selected, name]
}
