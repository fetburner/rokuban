/**
 * 録画一覧の期間（`from` / `to`）の選択肢・表示・日付入力との変換。
 *
 * URL と API の意味は変えない --- `program_start_at` が `from` 以上・`to` 未満。
 * ここは入力 UI と表示のための換算だけを持つ。
 *
 * **区切りは日本時間（UTC+9）で明示して計算する。** ブラウザのタイムゾーンに
 * 頼らない（海外から開いてもクールの境目は放送の暦で決まる）。日本は夏時間が
 * 無いので固定オフセットで足りる。
 *
 * `/search` とルールの期間（`condition-fields.tsx` / `periodStartAt`）とは共有
 * しない。あちらは未来の番組の条件で DB に保存されるので、「終了日を含む」の
 * 換算をそちらへ波及させない（docs/frontend/recordings.md「`/search` と条件
 * モデルを共有しない」）。
 */

const JST_OFFSET_MS = 9 * 3600_000

/** seasonNames はクール（3 か月）の名前。添字 s のクールは (3s+1) 月 1 日に始まる。 */
export const seasonNames = ['冬', '春', '夏', '秋'] as const

/** PeriodRange は期間の両端（ISO 8601、UTC）。from 以上・to 未満。 */
export type PeriodRange = { from?: string; to?: string }

/** Cour はクール 1 つ（年と、`seasonNames` の添字）。 */
export type Cour = { year: number; season: number }

/** PeriodPreset は期間メニュー上段の選択肢。 */
export type PeriodPreset = { label: string; detail?: string; range: PeriodRange }

/**
 * jstMidnight は日本時間の y 年 (m0+1) 月 d 日 0:00 を ISO 8601（UTC）で返す。月日の繰り上がりは
 * setUTCFullYear に任せる（`Date.UTC` は 0〜99 年を 1900 年代に読み替えるので、日付欄で年を
 * 打っている途中の「0002」が 1902 になり入力と食い違う）。
 */
function jstMidnight(year: number, month0: number, day: number): string {
  const d = new Date(0)
  d.setUTCFullYear(year, month0, day)
  return new Date(d.getTime() - JST_OFFSET_MS).toISOString()
}

/** jstParts は時刻を日本時間の暦の成分に分ける。 */
function jstParts(ms: number) {
  const t = new Date(ms + JST_OFFSET_MS)
  return {
    year: t.getUTCFullYear(),
    month0: t.getUTCMonth(),
    day: t.getUTCDate(),
    weekday: t.getUTCDay(),
    hours: t.getUTCHours(),
    minutes: t.getUTCMinutes(),
    isMidnight: (ms + JST_OFFSET_MS) % 86_400_000 === 0,
  }
}

/** courRange はクールの範囲（始まりの 0:00 JST から次のクールの始まりまで）を返す。 */
export function courRange({ year, season }: Cour): PeriodRange {
  return { from: jstMidnight(year, season * 3, 1), to: jstMidnight(year, season * 3 + 3, 1) }
}

/** courLabel は「2026 夏」の形の名前を返す。 */
export function courLabel({ year, season }: Cour): string {
  return `${year} ${seasonNames[season]}`
}

/** currentCour は now を含むクールを返す。 */
export function currentCour(now: Date): Cour {
  const p = jstParts(now.getTime())
  return { year: p.year, season: Math.floor(p.month0 / 3) }
}

/** courOf は範囲がちょうど 1 クールならそのクールを返す。 */
export function courOf(range: PeriodRange): Cour | undefined {
  if (range.from === undefined) return undefined
  const cour = currentCour(new Date(range.from))
  return sameRange(range, courRange(cour)) ? cour : undefined
}

/** sameRange は両端が同じ時刻か（文字列の表記揺れは見ない）。 */
function sameRange(a: PeriodRange, b: PeriodRange): boolean {
  const t = (iso: string | undefined) => (iso === undefined ? undefined : Date.parse(iso))
  return t(a.from) === t(b.from) && t(a.to) === t(b.to)
}

/**
 * periodPresets は期間メニュー上段の選択肢。今週・今月は終わりを開いたまま
 * （`to` なし）にする --- 放送中・これからの録画も入り続ける。
 *
 * 週は月曜に始まる（ISO 8601 / JIS X 0301）。日曜始まりにすると土曜と日曜の
 * 深夜帯が別の週に割れる。
 */
export function periodPresets(now: Date): PeriodPreset[] {
  const p = jstParts(now.getTime())
  const cour = currentCour(now)
  const previous = cour.season === 0 ? { year: cour.year - 1, season: 3 } : { ...cour, season: cour.season - 1 }
  return [
    { label: 'すべての期間', range: {} },
    { label: '今週', range: { from: jstMidnight(p.year, p.month0, p.day - ((p.weekday + 6) % 7)) } },
    { label: '今月', range: { from: jstMidnight(p.year, p.month0, 1) } },
    { label: '今クール', detail: courLabel(cour), range: courRange(cour) },
    { label: '前クール', detail: courLabel(previous), range: courRange(previous) },
  ]
}

/** isSelectedPreset は選択肢が今の範囲と一致するか。 */
export function isSelectedPreset(preset: PeriodPreset, range: PeriodRange): boolean {
  return sameRange(preset.range, range)
}

/**
 * periodLabel は期間ボタンとチップが共有する表示文字列（未指定は undefined）。
 *
 * ちょうど 1 クールなら「2026 夏」、上段の選択肢に一致すればその名前、それ以外は
 * 「8/10〜8/16」（終わりは `to` の前日 = 含む最終日）。今年でない端には年を添える。
 * 日本時間の 0:00 でない端（古い共有 URL）は時刻も出す --- 表示から範囲を読み
 * 違えさせない。
 */
export function periodLabel(range: PeriodRange, now: Date): string | undefined {
  if (range.from === undefined && range.to === undefined) return undefined
  const cour = courOf(range)
  if (cour !== undefined) return courLabel(cour)
  const preset = periodPresets(now).find((candidate) => sameRange(candidate.range, range))
  if (preset !== undefined) return preset.label

  const thisYear = jstParts(now.getTime()).year
  const day = (ms: number, withTime: boolean) => {
    const p = jstParts(ms)
    const date = `${p.year === thisYear ? '' : `${p.year}/`}${p.month0 + 1}/${p.day}`
    return withTime ? `${date} ${p.hours}:${String(p.minutes).padStart(2, '0')}` : date
  }
  const from = range.from === undefined ? undefined : Date.parse(range.from)
  const to = range.to === undefined ? undefined : Date.parse(range.to)
  const start = from === undefined ? '' : day(from, !jstParts(from).isMidnight)
  // 0:00 で終わる範囲はその前日までを含む。0:00 でなければ to の時刻そのもの（未満）を出す。
  const end = to === undefined ? '' : jstParts(to).isMidnight ? day(to - 1, false) : `${day(to, true)} 前`
  return `${start}〜${end}`
}

/** dateInputToFrom は `<input type="date">` の値（開始日）を、その日の 0:00 JST にする。空は undefined。 */
export function dateInputToFrom(value: string): string | undefined {
  return dateInputToIso(value, 0)
}

/** dateInputToTo は終了日（その日を含む）を、翌日の 0:00 JST（未満）にする。空は undefined。 */
export function dateInputToTo(value: string): string | undefined {
  return dateInputToIso(value, 1)
}

function dateInputToIso(value: string, addDays: number): string | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  if (m === null) return undefined
  return jstMidnight(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + addDays)
}

/** fromToDateInput は `from` を開始日欄の値（日本時間の日付）にする。 */
export function fromToDateInput(iso: string | undefined): string {
  return iso === undefined ? '' : dateInputValue(Date.parse(iso))
}

/** toToDateInput は `to`（未満）を終了日欄の値（含む最終日）にする。7/1 0:00 なら 6/30。 */
export function toToDateInput(iso: string | undefined): string {
  return iso === undefined ? '' : dateInputValue(Date.parse(iso) - 1)
}

function dateInputValue(ms: number): string {
  if (Number.isNaN(ms)) return ''
  const p = jstParts(ms)
  return `${String(p.year).padStart(4, '0')}-${String(p.month0 + 1).padStart(2, '0')}-${String(p.day).padStart(2, '0')}`
}
