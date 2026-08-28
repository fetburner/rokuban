/**
 * `/recordings` の検索条件（URL の `search`）の型と、そこから
 * `ListRecordingsParams` へ落とす純関数（issue #137, M3-25）。
 *
 * `lib/program-search.ts`（`/search` の下書き）と役割は似ているが、条件モデルは
 * 共有しない --- 番組表検索（`ProgramSearchRequest` / `internal/rulequery`）と
 * 録画検索（`GET /api/recordings` の絞り込み）は別の問いであり、
 * docs/frontend.md「録画検索は `/recordings` に同居する」に理由がある。
 *
 * React に依存しないのはテストのため。URL の値は常に「文字列 or 未定義 or
 * 配列」という不定形の入力（`Record<string, unknown>`）なので、パースと
 * リクエスト組み立てを純関数として固定し、壊れた URL（手入力・古いブックマーク・
 * サーバーの列挙が変わった後の共有リンク）を UI 越しに何度も再現しなくても
 * 検証できるようにする。
 */

import {
  ListRecordingsOrder,
  ListRecordingsSource,
  ListRecordingsStatus,
  type ListRecordingsParams,
} from '@/api/generated'
import { formatDateTime } from '@/lib/format'
import { parsePositiveIntId } from '@/lib/positive-id'
import { ListRecordingsQueryParams } from '@/api/zod'
import { genreCodeLabel } from '@/lib/program-search'
import { ascending, asInteger, validArray, validValue } from '@/lib/url-search'

/**
 * q は openapi.yaml から生成した `GET /api/recordings` のクエリスキーマ。
 * enum / 値域 / 正規表現を持つ軸（genre / site / service / status / source）は
 * ここが権威で、手で書き写さない。`order` / `ruleId` / `from` / `to` / `q` は
 * URL 固有の扱い（安全整数の判定・ISO 8601 の正規化・空文字の扱い）が要るので
 * 手書きのまま残している。
 */
const q = ListRecordingsQueryParams.shape

/** RecordingsPageSearch は `/recordings` の URL クエリパラメータ（検証済み）。 */
export type RecordingsPageSearch = {
  q?: string
  /** ARIB ジャンル大分類（lv1、0〜15）。複数可、OR。 */
  genre?: number[]
  /** mirakc サイト名。複数可、OR。 */
  site?: string[]
  /** `Service.id`。複数可、OR。site とは別軸（軸間は AND）。 */
  service?: number[]
  status?: ListRecordingsStatus
  source?: ListRecordingsSource
  /** 特定ルール由来の録画に絞る（ルール一覧の「このルールの録画」導線から） */
  ruleId?: number
  /** 番組開始時刻の範囲（ISO 8601、UTC）。from 以上。 */
  from?: string
  /** 番組開始時刻の範囲（ISO 8601、UTC）。to 未満。 */
  to?: string
  order?: ListRecordingsOrder
}

/** recordingStatusValues は状態フィルタの選択肢の並び順。 */
export const recordingStatusValues: ListRecordingsStatus[] = [
  ListRecordingsStatus.recording,
  ListRecordingsStatus.finished,
  ListRecordingsStatus.canceled,
  ListRecordingsStatus.failed,
]

/** statusLabels は録画状態の日本語表記（`pages/recordings.tsx` と共有）。 */
export const statusLabels: Record<ListRecordingsStatus, string> = {
  recording: '録画中',
  finished: '完了',
  canceled: '取消',
  failed: '失敗',
}

/** recordingSourceValues は種別フィルタの選択肢の並び順。 */
export const recordingSourceValues: ListRecordingsSource[] = [
  ListRecordingsSource.rule,
  ListRecordingsSource.manual,
]

/** sourceLabels は録画の出自（source）の日本語表記。 */
export const sourceLabels: Record<ListRecordingsSource, string> = {
  rule: 'ルール',
  manual: '手動',
}

/** emptyRecordingsSearch は条件を何も指定していない状態。 */
export function emptyRecordingsSearch(): RecordingsPageSearch {
  return {}
}

function parseEnum<T extends string>(raw: unknown, allowed: readonly T[]): T | undefined {
  return typeof raw === 'string' && (allowed as readonly string[]).includes(raw) ? (raw as T) : undefined
}

/**
 * parseRuleId は `ruleId` を「正の安全整数」として受け取る（`lib/positive-id.ts` の
 * `parsePositiveIntId`）。`rules.id` は `bigint` PK（Go 側 `int64` にバインドされる）
 * かつシーケンス由来で 1 以上しか存在しないが、`parsePositiveIntId` は受理範囲を
 * `bigint` の全域ではなく `1..Number.MAX_SAFE_INTEGER` へ意図的に狭める。
 * `Number.MAX_SAFE_INTEGER` を超える値は JS の `number` で正確に表せず、
 * サーバーに存在する `rules.id` そのものではなく「丸められた別の値」になる
 * ため、狭い方に倒して落とす。
 *
 * `routes.tsx` の `/search` の `ruleId`（`?ruleId=N` でルールの条件を検索画面へ
 * 写す導線。issue #24 M2-11、issue #194 で明示 `undefined` に揃えた）と `/live` の
 * `serviceId` も同じ `parsePositiveIntId` を使うので、パースの流儀が分岐しない。
 */
export function parseRuleId(raw: unknown): number | undefined {
  return parsePositiveIntId(raw)
}

/** parseIsoDate は日時として解釈できる文字列を ISO 8601（UTC）へ正規化する。 */
function parseIsoDate(raw: unknown): string | undefined {
  if (typeof raw !== 'string') return undefined
  const t = Date.parse(raw)
  return Number.isNaN(t) ? undefined : new Date(t).toISOString()
}

/**
 * parseRecordingsSearch は URL の生の値を検証済みの検索条件にする
 * （`routes.tsx` の `validateSearch`）。
 *
 * `/search` の `ruleId` と同じ流儀 --- 不正な値（型が違う・enum に無い・
 * 範囲外・NaN）は例外にせず落として「その条件なし」にする。壊れたリンク
 * （手入力・古いブックマーク・サーバーの列挙が変わった後の共有リンク）を
 * 踏んでも画面は開く。
 *
 * **落とした次元は `undefined` を明示的に代入する（キーを省略しない）。**
 * TanStack Router の既定（非 strict）モードは、実際のルートマッチ
 * （`matchRoutesInternal`。`@tanstack/router-core` の `router.js` 内、
 * `preMatchSearch = { ...parentSearch, ...strictSearch }`）でも、
 * ビルドロケーション用の軽量マッチ（`matchRoutesLightweight` の
 * `accumulatedSearch`。`Object.assign(accumulatedSearch, validateSearch(...))`）
 * でも、**`validateSearch` の戻り値を「生の（未検証の） `location.search` の上に
 * 重ねる」形で合成する**。戻り値からキーを省略すると、そのキーは上書きされず
 * 生の不正な値（`status=bogus` の文字列そのもの等）が検証済みのつもりの結果へ
 * 漏れて残る（実機で確認済み。省略する形だと壊れた URL の不正な値がチップにそのまま
 * 出た）。`{ ...x, k: undefined }` はどちらの合成方式で見ても実際に上書きになる
 * ので、無効な値を確実に消すには明示的な `undefined` 代入が要る。
 */
export function parseRecordingsSearch(search: Record<string, unknown>): RecordingsPageSearch {
  return {
    q: typeof search.q === 'string' && search.q.trim() !== '' ? search.q : undefined,
    genre: validArray<number>(q.genre.unwrap().element, search.genre, {
      coerce: asInteger,
      sort: ascending,
    }),
    // **`coerce: String` が要る。** TanStack Router の既定 `parseSearch`
    // （`qss.toValue`）は `?site=123` を**数値** 123 にしてから渡すので、
    // 文字列スキーマ（`zod.string().regex(...)`）が落としてしまう。site 名は
    // `mirakcSiteNamePattern`（`^[a-z0-9]([_-]?[a-z0-9])*$`）上すべて数字でも
    // 合法なので、`mirakcs: [{site: "123"}]` は実在しうる構成である。
    // **`localeCompare` は使わない。** ロケール依存なので、同じ共有 URL が
    // ブラウザによって違う並びになり、queryKey が食い違う（正準化の目的を
    // 自分で壊す）。チップ側（`components/recording-filters.tsx`）の
    // `Array.prototype.sort()` と同じコード単位順に揃える。
    site: validArray<string>(q.site.unwrap().element, search.site, {
      coerce: String,
      sort: (a, b) => (a < b ? -1 : a > b ? 1 : 0),
    }),
    service: validArray<number>(q.service.unwrap().element, search.service, {
      coerce: asInteger,
      sort: ascending,
    }),
    status: validValue<ListRecordingsStatus>(q.status.unwrap(), search.status),
    source: validValue<ListRecordingsSource>(q.source.unwrap(), search.source),
    ruleId: parseRuleId(search.ruleId),
    from: parseIsoDate(search.from),
    to: parseIsoDate(search.to),
    order: parseEnum(search.order, [ListRecordingsOrder.desc, ListRecordingsOrder.asc] as const),
  }
}

/**
 * hasAnyRecordingsCondition は絞り込みを 1 つ以上指定しているかを返す。
 *
 * `order`（並び順）は絞り込みではないので数えない --- 0 件のときの文言
 * （「条件に一致する録画がありません」/「録画がありません」）を分けるための
 * 判定であり、並び順を変えただけでは「まだ何も録れていない」の意味は
 * 変わらない。
 */
export function hasAnyRecordingsCondition(search: RecordingsPageSearch): boolean {
  return (
    (search.q !== undefined && search.q.trim() !== '') ||
    (search.genre?.length ?? 0) > 0 ||
    (search.site?.length ?? 0) > 0 ||
    (search.service?.length ?? 0) > 0 ||
    search.status !== undefined ||
    search.source !== undefined ||
    search.ruleId !== undefined ||
    search.from !== undefined ||
    search.to !== undefined
  )
}

/**
 * buildListRecordingsParams は検索条件を `GET /api/recordings` のクエリに落とす。
 *
 * `trash` は検索条件と直交する別の軸（タブ）なので引数で受け、search には
 * 含めない（docs/frontend.md「ごみ箱タブと検索条件は直交させる」）。
 */
export function buildListRecordingsParams(
  search: RecordingsPageSearch,
  trash: boolean,
): ListRecordingsParams {
  const params: ListRecordingsParams = { trash }

  if (search.q !== undefined && search.q.trim() !== '') params.q = search.q
  if (search.genre !== undefined && search.genre.length > 0) params.genre = search.genre
  if (search.site !== undefined && search.site.length > 0) params.site = search.site
  if (search.service !== undefined && search.service.length > 0) {
    params.service = search.service
  }
  if (search.status !== undefined) params.status = search.status
  if (search.source !== undefined) params.source = search.source
  if (search.ruleId !== undefined) params.ruleId = search.ruleId
  if (search.from !== undefined) params.from = search.from
  if (search.to !== undefined) params.to = search.to
  if (search.order !== undefined) params.order = search.order

  return params
}

/**
 * isoToLocalDateTimeInput は ISO 8601（UTC）を `<input type="datetime-local">` の
 * 値（ローカル壁時計）にする。未指定は空文字列。
 *
 * `lib/program-search.ts` の `isoToLocalDateTime` と同じ変換だが、あちらは
 * `RuleInput` の期間専用でエクスポートされていない。録画検索の期間は別の
 * 条件モデル（このファイル冒頭のコメント）に属するので、ここに同じ変換を
 * 複製する（2 つの短い純関数を共有するために条件モデルを結合したくない）。
 */
export function isoToLocalDateTimeInput(iso: string | undefined): string {
  if (iso === undefined) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** localDateTimeInputToIso は `isoToLocalDateTimeInput` の逆。空文字列は undefined。 */
export function localDateTimeInputToIso(value: string): string | undefined {
  if (value === '') return undefined
  const t = new Date(value)
  return Number.isNaN(t.getTime()) ? undefined : t.toISOString()
}

/** RecordingsFilterChip は適用中の条件チップ 1 件。 */
export type RecordingsFilterChip = {
  key: string
  label: string
  /** このチップだけを外した検索条件を返す。 */
  clear: (search: RecordingsPageSearch) => RecordingsPageSearch
}

/**
 * periodLabel は期間チップの表示文字列。`期間指定` のような値の読めないラベルに
 * しない --- チップを見るだけで何を絞っているか分かる必要がある（レビューで
 * 指摘。issue #137）。片方だけの指定は「〜」を開いたままにする。
 */
function periodLabel(from: string | undefined, to: string | undefined): string {
  if (from !== undefined && to !== undefined) {
    return `${formatDateTime(from)} 〜 ${formatDateTime(to)}`
  }
  if (from !== undefined) return `${formatDateTime(from)} 〜`
  if (to !== undefined) return `〜 ${formatDateTime(to)}`
  return ''
}

/**
 * describeRecordingsFilters は適用中の条件をチップの一覧にする。
 *
 * 配列条件（ジャンル・チャンネル）は値ごとに 1 チップ（個別に外せる）、
 * スカラー条件（状態・種別・期間・ルール）は次元ごとに 1 チップにする。
 * キーワードはチップにしない --- 検索欄自体が値を表示しているので、
 * 消すのは入力欄の編集で足りる。
 */
export function describeRecordingsFilters(
  search: RecordingsPageSearch,
  serviceLabelById: ReadonlyMap<number, string>,
): RecordingsFilterChip[] {
  const chips: RecordingsFilterChip[] = []

  for (const code of search.genre ?? []) {
    chips.push({
      key: `genre-${code}`,
      label: `ジャンル: ${genreCodeLabel(code)}`,
      clear: (s) => {
        const next = (s.genre ?? []).filter((g) => g !== code)
        return { ...s, genre: next.length > 0 ? next : undefined }
      },
    })
  }

  for (const site of search.site ?? []) {
    chips.push({
      key: `site-${site}`,
      label: `サイト: ${site}`,
      clear: (s) => {
        const next = (s.site ?? []).filter((v) => v !== site)
        return { ...s, site: next.length > 0 ? next : undefined }
      },
    })
  }

  for (const service of search.service ?? []) {
    chips.push({
      key: `service-${service}`,
      label: `チャンネル: ${serviceLabelById.get(service) ?? `チャンネル #${service}`}`,
      clear: (s) => {
        const next = (s.service ?? []).filter((key) => key !== service)
        return { ...s, service: next.length > 0 ? next : undefined }
      },
    })
  }

  if (search.status !== undefined) {
    chips.push({
      key: 'status',
      label: `状態: ${statusLabels[search.status]}`,
      clear: (s) => ({ ...s, status: undefined }),
    })
  }

  if (search.source !== undefined) {
    chips.push({
      key: 'source',
      label: `種別: ${sourceLabels[search.source]}`,
      clear: (s) => ({ ...s, source: undefined }),
    })
  }

  if (search.ruleId !== undefined) {
    chips.push({
      key: 'ruleId',
      label: `ルール #${search.ruleId}`,
      clear: (s) => ({ ...s, ruleId: undefined }),
    })
  }

  if (search.from !== undefined || search.to !== undefined) {
    chips.push({
      key: 'period',
      label: `期間: ${periodLabel(search.from, search.to)}`,
      clear: (s) => ({ ...s, from: undefined, to: undefined }),
    })
  }

  return chips
}

/**
 * clearRecordingsFilters は絞り込みを全部外す。`order`（並び順）は絞り込みでは
 * ないので保持する --- 「条件をクリア」を押した直後にソートまで初期化される
 * のはユーザーが期待しない副作用になる。
 */
export function clearRecordingsFilters(search: RecordingsPageSearch): RecordingsPageSearch {
  return { order: search.order }
}
