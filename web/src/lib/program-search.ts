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
  Rule,
  RuleInput,
  RuleTextMatch,
  RuleTextMatchMode,
  RuleTextMatchTarget,
} from '@/api/generated'
import { encodeSettingsError, type KeepOriginal } from '@/lib/encode-settings'
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

/** RuleMetaDraft はルールの条件以外の部分（フォーム状態）。 */
export type RuleMetaDraft = {
  name: string
  enabled: boolean
  /** 入力欄の文字列のまま持つ（SearchDraft の数値と同じ流儀） */
  priority: string
  keepOriginal: KeepOriginal
  encodeProfiles: string[]
}

/** emptyRuleMeta は新規作成時の初期値（priority は '10'、keepOriginal は 'always'）。 */
export function emptyRuleMeta(): RuleMetaDraft {
  return {
    name: '',
    enabled: true,
    priority: '10',
    keepOriginal: 'always',
    encodeProfiles: [],
  }
}

/** ruleToMeta は既存ルールをフォーム状態にする。 */
export function ruleToMeta(rule: Rule): RuleMetaDraft {
  return {
    name: rule.name,
    enabled: rule.enabled,
    priority: String(rule.priority),
    keepOriginal: rule.keepOriginal,
    encodeProfiles: rule.encodeProfiles ? [...rule.encodeProfiles] : [],
  }
}

/**
 * isoToLocalDateTime は UTC の ISO 8601 を `datetime-local` の値（ローカル壁時計）
 * にする。`localDateTimeToIso` の逆。
 *
 * `date.toISOString().slice(0, 16)` は使わない —— それは UTC の壁時計であって
 * ローカルではない。年月日時分をローカルのフィールド（`getFullYear` 等）から
 * 組み立てる必要がある。
 */
function isoToLocalDateTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  const year = d.getFullYear()
  const month = pad(d.getMonth() + 1)
  const day = pad(d.getDate())
  const hours = pad(d.getHours())
  const minutes = pad(d.getMinutes())
  return `${year}-${month}-${day}T${hours}:${minutes}`
}

/**
 * conditionsToDraft は条件（ルール・検索リクエスト）をフォームの下書きにする
 * （`buildSearchRequest` の逆）。
 *
 * 引数の型は `ProgramSearchRequest`（URL・localStorage に載せた検索条件と同形）。
 * `Rule` は `RuleInput`（条件の次元を含む）を拡張した形なので、そのまま渡せる ---
 * 条件の次元だけを取り出す専用の型を別に持つ必要が無い。
 */
export function conditionsToDraft(rule: ProgramSearchRequest): SearchDraft {
  return {
    isFree: rule.isFree === true ? 'yes' : rule.isFree === false ? 'no' : 'any',
    durationMinMinutes:
      rule.durationMinMs !== undefined && rule.durationMinMs !== null
        ? String(rule.durationMinMs / 60_000)
        : '',
    durationMaxMinutes:
      rule.durationMaxMs !== undefined && rule.durationMaxMs !== null
        ? String(rule.durationMaxMs / 60_000)
        : '',
    periodStartAt:
      rule.periodStartAt !== undefined && rule.periodStartAt !== null
        ? isoToLocalDateTime(rule.periodStartAt)
        : '',
    periodEndAt:
      rule.periodEndAt !== undefined && rule.periodEndAt !== null
        ? isoToLocalDateTime(rule.periodEndAt)
        : '',
    textMatches: (rule.textMatches ?? []).map((m) => ({
      target: m.target,
      mode: m.mode,
      value: m.value,
      caseSensitive: m.caseSensitive ?? false,
      negate: m.negate ?? false,
    })),
    services: (rule.services ?? []).map((s) => ({
      networkId: s.networkId,
      serviceId: s.serviceId,
    })),
    channelTypes: (rule.channelTypes ?? []).slice(),
    genres: rule.genres ? [...rule.genres] : [],
    times: (rule.times ?? []).map((t) => ({
      weekdays: t.weekdays,
      startSec: t.startSec,
      endSec: t.endSec,
    })),
  }
}

/**
 * canonicalSearchConditions は外から来た条件を「この画面が実際に送る形」に畳む
 * （下書きを経由して往復させる）。
 *
 * URL（`?cond=`）や localStorage から来た条件は、フォームが持たない次元を含みうる
 * --- openapi の `sites`（`rule_sites` 相当）がそれで、`conditionsToDraft` は
 * その次元を持たない。畳まずに「条件がある」と扱うと、**画面には何も表示されない
 * まま全件検索が走る**（`sites` だけの条件は下書きでは空になる）。畳んだ結果が
 * 空なら「条件なし」として扱えばよい、という判定に使う。
 *
 * 秒以下・ミリ秒の精度も下書きの粒度（分・分単位の壁時計）に落ちるが、それは
 * 実際に送られるリクエストと同じ落ち方なので、畳んだ形の方が「押したら何が
 * 起きるか」に忠実になる。
 */
export function canonicalSearchConditions(conditions: ProgramSearchRequest): ProgramSearchRequest {
  return buildSearchRequest(conditionsToDraft(conditions))
}

/**
 * buildRuleInput は下書きとメタから `RuleInput` を作る。
 *
 * 条件部分は `buildSearchRequest(draft)` をスプレッドするだけで、変換を
 * 2 箇所に重複させない。
 *
 * **`sites` は送らない（空 = 全サイト）。** `ProgramSearchRequest.sites` は
 * `rule_sites` 相当の別次元で、検索フォーム（`SearchDraft`）は UI にこの次元を
 * 出していない。UI が出していない次元を検索条件から推測して埋めると、
 * 「画面で試した条件」と「実際に保存される条件」が食い違う
 * （試していない次元が黙って決まる）。ただし `preserve` に既存ルールがあり、
 * それが `sites` を持っているなら引き継ぐ —— UI から編集できないフィールドを
 * 保存のたびに消してはならない（下の `preserve` の説明を参照）。
 *
 * `preserve` に既存ルールを渡すと、UI を持たない項目（`description` /
 * `dedupeEnabled` / `dedupeThreshold` / `dedupeWindowSeconds` /
 * `filenameTemplate` / `metadata` / `sites`）を引き継ぐ。渡さない
 * （新規作成）ときはこれらを一切送らない。
 */
export function buildRuleInput(
  draft: SearchDraft,
  meta: RuleMetaDraft,
  preserve?: Rule,
): RuleInput {
  const priorityNum = Number(meta.priority)
  const input: RuleInput = {
    ...buildSearchRequest(draft),
    name: meta.name.trim(),
    enabled: meta.enabled,
    priority: Number.isFinite(priorityNum) ? priorityNum : 10,
    keepOriginal: meta.keepOriginal,
    encodeProfiles: meta.encodeProfiles,
  }

  if (preserve !== undefined) {
    if (preserve.description !== undefined) input.description = preserve.description
    if (preserve.dedupeEnabled !== undefined) input.dedupeEnabled = preserve.dedupeEnabled
    if (preserve.dedupeThreshold !== undefined) input.dedupeThreshold = preserve.dedupeThreshold
    if (preserve.dedupeWindowSeconds !== undefined) {
      input.dedupeWindowSeconds = preserve.dedupeWindowSeconds
    }
    if (preserve.filenameTemplate !== undefined) input.filenameTemplate = preserve.filenameTemplate
    if (preserve.metadata !== undefined) input.metadata = preserve.metadata
    if (preserve.sites !== undefined) input.sites = preserve.sites
  }

  return input
}

/** ruleMetaError は保存してはいけないメタの理由を返す（問題なければ undefined）。 */
export function ruleMetaError(meta: RuleMetaDraft): string | undefined {
  if (meta.name.trim() === '') {
    return '名前は必須です'
  }
  return encodeSettingsError(meta.keepOriginal, meta.encodeProfiles)
}
