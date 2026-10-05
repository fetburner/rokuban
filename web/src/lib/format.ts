/** 日本語 UI 向けの日時整形。ブラウザのローカルタイムで表示する。 */

const timeFormatter = new Intl.DateTimeFormat('ja-JP', {
  hour: '2-digit',
  minute: '2-digit',
})

const dateFormatter = new Intl.DateTimeFormat('ja-JP', {
  month: 'numeric',
  day: 'numeric',
  weekday: 'short',
})

const dateTimeFormatter = new Intl.DateTimeFormat('ja-JP', {
  month: 'numeric',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
})

/** formatTime は 19:00 の形式で返す。 */
export function formatTime(iso: string): string {
  return timeFormatter.format(new Date(iso))
}

/** formatDate は 7/25(土) の形式で返す。 */
export function formatDate(iso: string): string {
  return dateFormatter.format(new Date(iso))
}

/** formatDateTime は 7/25 19:00 の形式で返す。 */
export function formatDateTime(iso: string): string {
  return dateTimeFormatter.format(new Date(iso))
}

const dateTimeSecondsFormatter = new Intl.DateTimeFormat('ja-JP', {
  month: 'numeric',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

/** formatDateTimeSeconds は 7/25 19:00:30 の形式で返す。数十秒のずれを見せたいときに使う。 */
export function formatDateTimeSeconds(iso: string): string {
  return dateTimeSecondsFormatter.format(new Date(iso))
}

/** formatTimeRange は整形済みの両端を「開始〜終了」の形で返す。片端だけの範囲にも使う。 */
export function formatTimeRange(start: string | undefined, end: string | undefined): string {
  return `${start ?? ''}〜${end ?? ''}`
}

/**
 * formatPlaybackTime は動画の経過秒を 1:23 / 1:02:03 / -0:30 の形式で返す。
 * `withHours` が false なら 1 時間を超えても分で数える（62:03）。
 */
export function formatPlaybackTime(value: number, withHours = true): string {
  if (!Number.isFinite(value)) return '0:00'
  const seconds = Math.floor(Math.abs(value))
  const negative = value < 0 && seconds > 0
  const hours = withHours ? Math.floor(seconds / 3600) : 0
  const minutes = Math.floor((seconds - hours * 3600) / 60)
  const remaining = seconds % 60
  const formatted = hours > 0
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remaining).padStart(2, '0')}`
    : `${minutes}:${String(remaining).padStart(2, '0')}`
  return negative ? `-${formatted}` : formatted
}

/**
 * formatPlaybackTimeMs は経過秒をミリ秒つきの 1:23.456 形式で返す（フレーム単位の調整が見える桁）。
 * ミリ秒に丸めてから分解するので、59.9996 は 1:00.000 になる。
 */
export function formatPlaybackTimeMs(value: number): string {
  if (!Number.isFinite(value)) return '0:00.000'
  const totalMs = Math.round(Math.abs(value) * 1000)
  const ms = totalMs % 1000
  const base = formatPlaybackTime(Math.sign(value) * Math.floor(totalMs / 1000))
  return `${base}.${String(ms).padStart(3, '0')}`
}

/** dayKey は日付ヘッダのグルーピングに使うローカル日付のキーを返す。 */
export function dayKey(iso: string): string {
  const d = new Date(iso)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
}

/** calendarDayDiff は ms と baseMs のローカル暦日の差を日数で返す。 */
export function calendarDayDiff(ms: number, baseMs: number): number {
  const localDayStart = (value: number) => {
    const date = new Date(value)
    return new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime()
  }
  return Math.round((localDayStart(ms) - localDayStart(baseMs)) / 86_400_000)
}

/** formatDuration は 90分 / 1時間30分 の形式で返す。 */
export function formatDuration(ms: number): string {
  const minutes = Math.round(ms / 60000)
  if (minutes < 60) return `${minutes}分`
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return rest === 0 ? `${hours}時間` : `${hours}時間${rest}分`
}

/** formatBytes は 1.2 GB の形式で返す。 */
export function formatBytes(bytes: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${unit === 0 ? value : value.toFixed(1)} ${units[unit]}`
}

/** isAiring は現在放送中かを返す。 */
export function isAiring(startAt: string, endAt: string, now = Date.now()): boolean {
  return new Date(startAt).getTime() <= now && now < new Date(endAt).getTime()
}
