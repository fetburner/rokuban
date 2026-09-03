/**
 * ブラウザ再生の再開位置を localStorage に保存する（#14 7c / M3-5）。
 * サーバー側視聴履歴は持たない。キーは録画 ID + プロファイル。
 * 再生速度（端末ごとに 1 つ、録画をまたいで保つ）も同じく localStorage に持つ。
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

/**
 * shouldSavePlaybackPosition は timeupdate 由来の保存を間引くかどうかを判定する。
 *
 * video 要素の timeupdate は約 4Hz で発火するが、保存値は Math.floor(seconds) なので
 * 同じ秒の間に呼んでも書き込む値は変わらない。setInterval や debounce でタイマーを
 * 持つ代わりに「Math.floor(seconds) が前回保存時と変わったときだけ書く」を採用した
 * （実装が単純でタイマー管理が不要。保存頻度は最大で毎秒 1 回に収まる）。
 *
 * lastSavedSecond が null（未保存）のときは常に true を返す。
 */
export function shouldSavePlaybackPosition(lastSavedSecond: number | null, seconds: number): boolean {
  return lastSavedSecond === null || Math.floor(seconds) !== lastSavedSecond
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

/** playbackRates は再生速度セレクタの選択肢。保存値の検証もこの配列で行う。 */
export const playbackRates = [1, 1.25, 1.5, 1.75, 2] as const

const RATE_KEY = 'rokuban:playback-rate'

/**
 * loadPlaybackRate は保存済みの再生速度を返す。無い・壊れているなら 1。
 *
 * **録画ごとではなく端末ごとに 1 つ**（キーに録画 ID を含めない）。速度は
 * 「この録画をどう見るか」ではなく「自分がどう見るか」の好みなので、録画を
 * 変えるたびに 1 倍へ戻ると毎回選び直しになる（docs/frontend/design.md §個人化）。
 */
export function loadPlaybackRate(): number {
  try {
    const raw = localStorage.getItem(RATE_KEY)
    if (raw === null) return 1
    const n = Number(raw)
    // 選択肢に無い値（手で書き換えた・選択肢を減らした後の古い値）は既定へ落とす。
    // <select> に無い値を渡すと、どの option も選ばれていない空の見た目になる。
    return (playbackRates as readonly number[]).includes(n) ? n : 1
  } catch {
    // private mode 等で localStorage が使えない場合は既定
    return 1
  }
}

/** savePlaybackRate は再生速度を保存する。既定（1 倍）はキーごと消す。 */
export function savePlaybackRate(rate: number): void {
  try {
    if (rate === 1) localStorage.removeItem(RATE_KEY)
    else localStorage.setItem(RATE_KEY, String(rate))
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

/** recordingSubtitleURL は encoded アセット隣の WebVTT サイドカー URL を組み立てる。 */
export function recordingSubtitleURL(recordingId: number, profile: string): string {
  return `${recordingFileURL(recordingId, profile)}&track=subtitles`
}
