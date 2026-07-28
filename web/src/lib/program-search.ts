/**
 * 検索条件のフォーム状態（下書き）と、それを `ProgramSearchRequest` に落とす純関数。
 *
 * React に依存しないのはテストのため。UI 越しに検証できるのは「入力すると結果が
 * 変わる」ところまでで、分と ms、ローカル時刻と UTC、曜日ビットのような
 * 取り違えやすい変換は関数として直接固定する。
 *
 * 条件の次元は rules（`RuleInput`）と同じで、検索 API は ruler と同じ
 * コンパイラ（internal/rulequery）を通る。つまりこの画面は「ルールの条件を
 * 作る前に試す画面」であり、rules に無い次元をここで足してはならない。
 */

import type {
  ProgramSearchRequest,
  ProgramSearchRequestChannelTypesItem,
  RuleTextMatch,
  RuleTextMatchMode,
  RuleTextMatchTarget,
} from '@/api/generated'
import { genreLabel } from '@/lib/genre'

/** TriState は `boolean | null`（null = 問わない）を UI の 3 値で表す。 */
export type TriState = 'any' | 'yes' | 'no'

/** TextMatchDraft はテキスト条件 1 件。`RuleTextMatch` と同形（省略可を埋めた版）。 */
export type TextMatchDraft = {
  target: RuleTextMatchTarget
  mode: RuleTextMatchMode
  value: string
  caseSensitive: boolean
  negate: boolean
}

/** ServiceRefDraft は選択したサービス（NID/SID）。 */
export type ServiceRefDraft = { networkId: number; serviceId: number }

/**
 * TimeWindowDraft は時間帯条件 1 件。
 *
 * `weekdays` は bit0=月 … bit6=日。`startSec` / `endSec` は JST の壁時計での
 * その日の 0 時からの秒（`docs/recording.md` §3.1 / internal/rulequery）。
 * `endSec < startSec` は翌日跨ぎとして解釈される。
 */
export type TimeWindowDraft = { weekdays: number; startSec: number; endSec: number }

/**
 * SearchDraft はフォームの状態。
 *
 * 数値・日時は入力欄の文字列のまま持つ。数値に正規化して持つと「空欄」と「0」を
 * 区別できず、途中入力（`-` だけ打った状態）が勝手に 0 になる。
 */
export type SearchDraft = {
  isFree: TriState
  durationMinMinutes: string
  durationMaxMinutes: string
  /** `<input type="datetime-local">` の値（ローカル時刻の壁時計） */
  periodStartAt: string
  periodEndAt: string
  textMatches: TextMatchDraft[]
  services: ServiceRefDraft[]
  channelTypes: ProgramSearchRequestChannelTypesItem[]
  genres: number[]
  times: TimeWindowDraft[]
}

/** weekdayLabels は bit0 から順の曜日ラベル（bit0=月 … bit6=日）。 */
export const weekdayLabels = ['月', '火', '水', '木', '金', '土', '日'] as const

/** allWeekdays は全曜日のビットマスク（1..127 の上限）。 */
export const allWeekdays = 127

/** genreCodes は ARIB のジャンル大分類（lv1）の全コード。 */
export const genreCodes: readonly number[] = Array.from({ length: 16 }, (_, code) => code)

/**
 * genreCodeLabel はジャンルの選択肢ラベル。
 *
 * 知らないコード（12 / 13 の「予備」）は数値のまま出す。「その他」に丸めると
 * ARIB の本物の「その他」（コード 15）と区別できなくなる（lib/genre.ts と同じ規律）。
 */
export function genreCodeLabel(code: number): string {
  return genreLabel(code) ?? `ジャンル ${code}`
}

/** hasWeekday はビットマスクに指定の曜日が含まれるかを返す。 */
export function hasWeekday(weekdays: number, index: number): boolean {
  return (weekdays & (1 << index)) !== 0
}

/** toggleWeekday は指定の曜日ビットを反転したマスクを返す。 */
export function toggleWeekday(weekdays: number, index: number): number {
  return weekdays ^ (1 << index)
}

/** secToTimeValue は秒を `<input type="time">` の値（HH:MM）にする。 */
export function secToTimeValue(sec: number): string {
  const minutes = Math.floor(sec / 60) % (24 * 60)
  const hh = String(Math.floor(minutes / 60)).padStart(2, '0')
  const mm = String(minutes % 60).padStart(2, '0')
  return `${hh}:${mm}`
}

/** timeValueToSec は `<input type="time">` の値を秒にする。空欄は 0。 */
export function timeValueToSec(value: string): number {
  const m = /^(\d{1,2}):(\d{2})$/.exec(value)
  if (!m) return 0
  return Number(m[1]) * 3600 + Number(m[2]) * 60
}

/** emptyDraft は何も指定していない下書き。 */
export function emptyDraft(): SearchDraft {
  return {
    isFree: 'any',
    durationMinMinutes: '',
    durationMaxMinutes: '',
    periodStartAt: '',
    periodEndAt: '',
    textMatches: [],
    services: [],
    channelTypes: [],
    genres: [],
    times: [],
  }
}

/** newTextMatch は追加直後のテキスト条件（番組名のキーワード）。 */
export function newTextMatch(): TextMatchDraft {
  return { target: 'name', mode: 'keyword', value: '', caseSensitive: false, negate: false }
}

/**
 * newTimeWindow は追加直後の時間帯条件。
 *
 * 開始・終了はどちらも 0 時にしておく。この状態は「幅ゼロ」で何にもマッチしない
 * ため `draftError` が検索を止める。適当な既定の窓（21:00–23:00 等）を入れると、
 * ユーザーが指定していない絞り込みが黙って効く。
 */
export function newTimeWindow(): TimeWindowDraft {
  return { weekdays: allWeekdays, startSec: 0, endSec: 0 }
}

function parseMinutes(value: string): number | undefined {
  if (value.trim() === '') return undefined
  const n = Number(value)
  return Number.isFinite(n) && n >= 0 ? n : undefined
}

/**
 * localDateTimeToIso は `datetime-local` の値を ISO 8601（UTC）にする。
 *
 * `YYYY-MM-DDTHH:mm` はタイムゾーンを持たない形式なので `Date` がローカル時刻として
 * 解釈する。UI はブラウザのローカルタイムで表示する（lib/format.ts）ので、
 * 入力もローカル時刻として読むのが一貫する。
 */
function localDateTimeToIso(value: string): string | undefined {
  if (value === '') return undefined
  const t = new Date(value)
  return Number.isNaN(t.getTime()) ? undefined : t.toISOString()
}

function toTextMatch(draft: TextMatchDraft): RuleTextMatch {
  // caseSensitive / negate は既定値（false）と同じなら送らない。既定と同じ値を
  // 明示的に載せると、リクエストを見たときに「指定した」ように読めてしまう。
  return {
    target: draft.target,
    mode: draft.mode,
    value: draft.value,
    ...(draft.caseSensitive ? { caseSensitive: true } : {}),
    ...(draft.negate ? { negate: true } : {}),
  }
}

/**
 * buildSearchRequest は下書きをリクエストに落とす。
 *
 * **「問わない」次元はキーごと落とす。** `null` を明示的に送っても API の意味は
 * 同じだが、送らない方が「何を指定したか」がリクエストそのままで読める
 * （CLAUDE.md 不変条件 10 の精神 — 何も主張していない値を作らない）。
 */
export function buildSearchRequest(draft: SearchDraft): ProgramSearchRequest {
  const request: ProgramSearchRequest = {}

  if (draft.isFree !== 'any') request.isFree = draft.isFree === 'yes'

  const min = parseMinutes(draft.durationMinMinutes)
  if (min !== undefined) request.durationMinMs = min * 60_000
  const max = parseMinutes(draft.durationMaxMinutes)
  if (max !== undefined) request.durationMaxMs = max * 60_000

  const periodStart = localDateTimeToIso(draft.periodStartAt)
  if (periodStart !== undefined) request.periodStartAt = periodStart
  const periodEnd = localDateTimeToIso(draft.periodEndAt)
  if (periodEnd !== undefined) request.periodEndAt = periodEnd

  if (draft.textMatches.length > 0) request.textMatches = draft.textMatches.map(toTextMatch)
  if (draft.services.length > 0) {
    request.services = draft.services.map((s) => ({
      networkId: s.networkId,
      serviceId: s.serviceId,
    }))
  }
  if (draft.channelTypes.length > 0) request.channelTypes = [...draft.channelTypes]
  // ジャンルは選んだ順ではなくコード順で送る。同じ選択が常に同じリクエストになり、
  // 開発者ツールやログでの見比べができる
  if (draft.genres.length > 0) request.genres = [...draft.genres].sort((a, b) => a - b)
  if (draft.times.length > 0) {
    request.times = draft.times.map((t) => ({
      weekdays: t.weekdays,
      startSec: t.startSec,
      endSec: t.endSec,
    }))
  }

  return request
}

/**
 * draftError は送ってはいけない下書きの理由を返す（問題なければ undefined）。
 *
 * ここで見るのは「サーバーに送ると壊れる / 黙って嘘の絞り込みになる」ものだけ。
 * 矛盾した範囲（下限 > 上限）は検証しない — API は正しく 0 件を返すので、
 * 「該当なし」がそのまま答えである。
 *
 * - 曜日が 0 の時間帯: `rulequery.compileTimeWindow` が範囲外エラーを返し、
 *   API はこれを 400 に変換しないので **500 になる**（`weekdays` の下限は 1）
 * - 幅ゼロの時間帯: `sec >= X AND sec < X` は決してマッチしない
 * - 値が空のテキスト条件: keyword なら `LIKE '%%'`、regex なら `~ ''` で全件に
 *   マッチする。絞り込んだつもりで絞り込めていない状態を送らせない
 */
export function draftError(draft: SearchDraft): string | undefined {
  if (draft.textMatches.some((m) => m.value === '')) {
    return 'テキスト条件の値を入力してください'
  }
  if (
    parseMinutes(draft.durationMinMinutes) === undefined &&
    draft.durationMinMinutes.trim() !== ''
  ) {
    return '放送時間の下限には 0 以上の分数を入力してください'
  }
  if (
    parseMinutes(draft.durationMaxMinutes) === undefined &&
    draft.durationMaxMinutes.trim() !== ''
  ) {
    return '放送時間の上限には 0 以上の分数を入力してください'
  }
  if (draft.times.some((t) => t.weekdays === 0)) {
    return '時間帯には曜日を 1 つ以上選んでください'
  }
  if (draft.times.some((t) => t.startSec === t.endSec)) {
    return '時間帯の開始と終了には違う時刻を指定してください'
  }
  return undefined
}
