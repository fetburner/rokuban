/**
 * ブラウザ再生の再開位置を localStorage に保存する（#14 7c / M3-5）。
 * サーバー側視聴履歴は持たない。キーは録画 ID + プロファイル。
 */

const PREFIX = 'rokuban:playback:'

/** playbackStorageKey は recording id と profile から localStorage キーを作る。 */
export function playbackStorageKey(recordingId: number, profile: string): string {
  return `${PREFIX}${recordingId}:${profile}`
}

/** loadPlaybackPosition は保存済みの秒位置を返す。無ければ null。 */
export function loadPlaybackPosition(recordingId: number, profile: string): number | null {
  try {
    const raw = localStorage.getItem(playbackStorageKey(recordingId, profile))
    if (raw === null) return null
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0) return null
    return n
  } catch {
    // private mode 等で localStorage が使えない場合は無視
    return null
  }
}

/** savePlaybackPosition は秒位置を保存する。終端付近はクリアする。 */
export function savePlaybackPosition(
  recordingId: number,
  profile: string,
  seconds: number,
  duration?: number,
): void {
  try {
    // 終了 5 秒以内、または先頭 2 秒未満は「続きから」に残さない
    if (
      !Number.isFinite(seconds) ||
      seconds < 2 ||
      (duration !== undefined && Number.isFinite(duration) && duration > 0 && seconds >= duration - 5)
    ) {
      localStorage.removeItem(playbackStorageKey(recordingId, profile))
      return
    }
    localStorage.setItem(playbackStorageKey(recordingId, profile), String(Math.floor(seconds)))
  } catch {
    // ignore
  }
}

/** recordingFileURL は streamer のバイナリ配信 URL を組み立てる（OpenAPI 外）。 */
export function recordingFileURL(recordingId: number, profile?: string): string {
  const base = `/api/recordings/${recordingId}/file`
  if (!profile) return base
  return `${base}?profile=${encodeURIComponent(profile)}`
}
