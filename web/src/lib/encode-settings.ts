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
