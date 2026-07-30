/**
 * encodeProfiles / keepOriginal のクライアント側検証。
 *
 * API は `keepOriginal: until_encoded` かつ encodeProfiles 空で 400 を返す
 * （rules.go の validateRuleInput）。UI でも同じ制約を先に防ぎ、送れない理由を
 * ボタン横に出す（検索フォームの draftError と同じ流儀）。
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
