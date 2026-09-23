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

const RATE_KEY = 'rokuban:playback-rate'

/**
 * loadPlaybackRate は保存済みの再生速度を返す。無い・壊れているなら 1。
 *
 * **録画ごとではなく端末ごとに 1 つ**（キーに録画 ID を含めない）。速度は
 * 「この録画をどう見るか」ではなく「自分がどう見るか」の好みなので、録画を
 * 変えるたびに 1 倍へ戻ると毎回選び直しになる（docs/frontend/design.md §個人化）。
 * 値はブラウザ標準 controls が提供するので固定の選択肢一覧ではなく、正の有限値を
 * 有効とする。
 */
export function loadPlaybackRate(): number {
  try {
    const raw = localStorage.getItem(RATE_KEY)
    if (raw === null) return 1
    const n = Number(raw)
    return Number.isFinite(n) && n > 0 ? n : 1
  } catch {
    // private mode 等で localStorage が使えない場合は既定
    return 1
  }
}

/** savePlaybackRate は正の有限な再生速度を保存する。既定（1 倍）はキーごと消す。 */
export function savePlaybackRate(rate: number): void {
  try {
    if (!Number.isFinite(rate) || rate <= 0) return
    if (rate === 1) localStorage.removeItem(RATE_KEY)
    else localStorage.setItem(RATE_KEY, String(rate))
  } catch {
    // ignore
  }
}

/**
 * applyPlaybackRate は速度を video に設定し、ブラウザが拒否した場合は 1 倍へ戻す。
 * 対応する速度の範囲はブラウザごとに異なるため、保存時の数値検証だけでは
 * playbackRate の代入で NotSupportedError が起きる場合がある。その値は共通設定からも消す。
 */
export function applyPlaybackRate(video: HTMLVideoElement, rate: number): number {
  try {
    video.defaultPlaybackRate = rate
    video.playbackRate = rate
    return rate
  } catch {
    // 保存済みの速度をブラウザが受け付けない場合は、標準の 1 倍へ復旧する。
    try {
      video.defaultPlaybackRate = 1
      video.playbackRate = 1
    } catch {
      // 1 倍も設定できない環境でも React effect から例外を漏らさない。
    }
    savePlaybackRate(1)
    return 1
  }
}

/** recordingFileURL は streamer のバイナリ配信 URL を組み立てる（OpenAPI 外）。 */
export function recordingFileURL(recordingId: number, profile?: string): string {
  const base = `/api/media/recordings/${recordingId}/file`
  if (!profile) return base
  return `${base}?profile=${encodeURIComponent(profile)}`
}

/** recordingSubtitleURL は encoded アセット隣の WebVTT サイドカー URL を組み立てる。 */
export function recordingSubtitleURL(recordingId: number, profile: string): string {
  return `${recordingFileURL(recordingId, profile)}&track=subtitles`
}
