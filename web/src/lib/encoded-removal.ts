import type { Recording } from '@/api/generated'

type EncodedAsset = NonNullable<Recording['encodedAssets']>[number]

/** encodedAssetLabel は版タブの行と同じ名前（カット版は「カット版 (名前)」）を返す。 */
export function encodedAssetLabel(asset: EncodedAsset): string {
  return asset.cut ? `カット版 (${asset.profile})` : asset.profile
}

/**
 * encodedRemovalPlan は版を 1 本外したときの結果を、確認ダイアログの出し分けに使う形で返す。
 *
 * 最後の版かどうかはサーバー（`DELETE /api/recordings/{id}/encoded/{profile}` の 409）が
 * 外した後の状態で決める。ここは表示用の近似で、原本が削除処理中でも「原本あり」に見える。
 */
export function encodedRemovalPlan(recording: Recording, profile: string) {
  const assets = recording.encodedAssets ?? []
  const target = assets.find((asset) => asset.profile === profile)
  const remaining = assets.filter((asset) => asset.profile !== profile)
  const hasOriginal = recording.sizeBytes !== undefined
  return {
    freedBytes: target?.sizeBytes,
    remaining,
    hasOriginal,
    lastCopy: !hasOriginal && remaining.length === 0,
    // CM 入りの版が無くなるとチャプターを直せない。カット版を外すときは、もともと直せない。
    onlyCutRemains: !hasOriginal && target?.cut !== true && remaining.length > 0 && remaining.every((asset) => asset.cut === true),
    // until_encoded で desired が空になると、サーバーが always に倒す。
    switchesToKeepAlways:
      recording.keepOriginal === 'until_encoded' &&
      (recording.encodeProfiles ?? []).every((desired) => desired === profile),
  }
}
