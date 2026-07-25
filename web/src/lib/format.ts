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

/** dayKey は日付ヘッダのグルーピングに使うローカル日付のキーを返す。 */
export function dayKey(iso: string): string {
  const d = new Date(iso)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
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
