import type { RecordingMedia } from '@/api/generated'

/**
 * recordingMedia はテスト用の `Recording.media` を作る。
 *
 * URL は実サーバーの形に寄せつつ、**わざと本番と一致しない接尾辞を付ける**
 * （原本・コマは置き場 `takamatsu`、それ以外は `fixture=1`）。コンポーネントが URL を自分で
 * 組み立てると、この値と一致せずテストが落ちる。
 * `encodedAssets` を渡すと、その順にプロファイルごとの URL を作る。
 */
export function recordingMedia(
  id: number,
  encodedAssets: readonly { profile: string }[] = [],
  options: { original?: boolean } = {},
): RecordingMedia {
  const original = options.original ?? true
  return {
    originalUrl: original ? `/api/media/stores/takamatsu/recordings/${id}/file` : undefined,
    frameUrl: original ? `/api/media/stores/takamatsu/recordings/${id}/frame` : undefined,
    thumbnailUrl: `/api/media/recordings/${id}/thumbnail?fixture=1`,
    seekTilesUrl: `/api/media/recordings/${id}/seek-tiles?fixture=1`,
    encoded: encodedAssets.map(({ profile }) => ({
      profile,
      url: `/api/media/recordings/${id}/file?profile=${encodeURIComponent(profile)}&fixture=1`,
      subtitlesUrl: `/api/media/recordings/${id}/file?profile=${encodeURIComponent(profile)}&track=subtitles&fixture=1`,
    })),
  }
}
