import { ArrowUpDown, ChevronDown, ListFilter, Search as SearchIcon, X } from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'

import {
  ListRecordingsOrder,
  useListRules,
  useListSites,
  type Rule,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ChannelPicker } from '@/components/channel-picker'
import { RecordingPeriodMenu } from '@/components/recording-period-menu'
import { ToolbarDot, ToolbarPanel, toolbarButtonClass } from '@/components/toolbar-panel'
import { Chip } from '@/components/ui/chip'
import { Input } from '@/components/ui/field'
import { useAllSitesServices } from '@/lib/all-sites-services'
import { genreCodeLabel, genreCodes } from '@/lib/program-search'
import { ruleDisambiguator } from '@/lib/rule-label'
import { serviceDisambiguator } from '@/lib/service-label'
import { mdMediaQuery, useMediaQuery } from '@/lib/use-media-query'
import { cn } from '@/lib/utils'
import {
  clearRecordingsFilters,
  describeRecordingsFilters,
  isSourceMootWithRule,
  parseRuleId,
  recordingSourceValues,
  recordingStatusValues,
  sourceLabels,
  statusLabels,
  type RecordingsPageSearch,
} from '@/lib/recording-search'

/** キーワード入力の debounce（ms）。1 文字ごとに URL を書き換えて履歴を汚さない。 */
const KEYWORD_DEBOUNCE_MS = 300

type Update = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => void

/**
 * RecordingFilters は録画検索の条件 UI（issue #137）。録画一覧とシリーズ一覧が
 * 同じものを使う（条件の意味が同じなので、UI も 1 つにする）。
 *
 * 状態は一切持たない（キーワード入力欄の debounce 用の下書きを除く）。条件は
 * すべて呼び出し側（`pages/recordings.tsx` / `pages/series.tsx`）が URL の search
 * として持ち、ここは表示と `onChange` 呼び出しに徹する --- 条件の永続化・共有・
 * 戻るボタンとの整合は URL 側の責務であり、ここに複製しない。
 *
 * 並び順は画面ごとに軸が違う（録画は放送日時の昇降、シリーズは新着・件数・名前）
 * ので、絞り込みの右に置く操作を `children` で受ける（`ToolbarSelect`）。
 *
 * ツールバーは `[検索][期間][絞り込み][並び順]` の 1 行。md 未満は 3 つをアイコンに
 * して 360px でも折り返さない。期間はボタンが中身を表示するので、期間のチップは
 * ボタンが文字を出さない md 未満でだけ出す。
 */
export function RecordingFilters({
  search,
  onChange,
  children,
}: {
  search: RecordingsPageSearch
  onChange: Update
  children?: ReactNode
}) {
  const sitesQuery = useListSites()
  const sites = unwrap(sitesQuery.data) ?? []
  const rulesQuery = useListRules()
  const rules = unwrap(rulesQuery.data)
  // **`Service.id` で重複を潰す。** 同じチャンネルを 2 サイトで受けていても
  // 選択肢は 1 つ（identity は合成 id で、site は別軸の `?site=`）。潰さないと
  // ピッカーに同名の候補が site の数だけ並び、押しても同じ id が入るだけの
  // 「押し分けられない選択肢」になる。fetch + dedupe は `condition-fields.tsx`
  // と共有する（`lib/all-sites-services.ts`）。
  const {
    services: serviceList,
    isPending: servicesPending,
    isError: servicesError,
  } = useAllSitesServices()

  const disambiguate = serviceDisambiguator(serviceList)
  // サービスの identity は `Service.id`。同じチャンネルを 2 サイトで受けていても
  // 1 つの選択肢になる（site は別軸で絞る）ので、ラベルに site は入れない。
  const serviceLabelById = new Map<number, string>()
  for (const service of serviceList) {
    const disambiguator = disambiguate(service)
    serviceLabelById.set(
      service.id,
      disambiguator === undefined || disambiguator === '' ? service.name : `${service.name} (${disambiguator})`,
    )
  }

  // 期間ボタンとチップは同じ now で `periodLabel` を呼ぶ（「今週」の判定を食い違わせない）。
  const now = new Date()
  const chips = describeRecordingsFilters(search, serviceLabelById, rules, now)
  const onlyPeriodChip = chips.every((chip) => chip.key === 'period')

  return (
    <div className="flex flex-col gap-2 border-t border-border px-4 py-2">
      <div className="flex items-center gap-1 md:flex-wrap md:gap-2">
        <KeywordField
          value={search.q ?? ''}
          onChange={(q) => onChange((s) => ({ ...s, q: q.trim() === '' ? undefined : q }))}
        />
        <RecordingPeriodMenu search={search} onChange={onChange} now={now} />
        <FilterPanel
          search={search}
          services={serviceList}
          siteNames={sites}
          servicesPending={servicesPending}
          servicesError={servicesError}
          rules={rules ?? []}
          rulesPending={rulesQuery.isPending}
          rulesError={rulesQuery.isError}
          onChange={onChange}
        />
        {children}
      </div>

      {chips.length > 0 && (
        <div
          role="group"
          aria-label="適用中の条件"
          className={cn('flex flex-wrap items-center gap-1.5', onlyPeriodChip && 'md:hidden')}
        >
          {chips.map((chip) => (
            <button
              key={chip.key}
              type="button"
              onClick={() => onChange((s) => chip.clear(s))}
              className={cn(
                'flex items-center gap-1 rounded-full border border-border bg-muted px-2.5 py-1 text-xs text-foreground transition-colors hover:bg-muted/70',
                chip.key === 'period' && 'md:hidden',
              )}
            >
              {chip.label}
              <X className="size-3" aria-hidden />
            </button>
          ))}
          <button
            type="button"
            onClick={() => onChange(clearRecordingsFilters)}
            className="rounded-full px-2 py-1 text-xs text-muted-foreground underline-offset-2 hover:underline"
          >
            条件をクリア
          </button>
        </div>
      )}
    </div>
  )
}

/**
 * KeywordField はキーワード入力欄。300ms の debounce を挟んで `onChange` を呼ぶ
 * （docs/frontend.md「debounce と URL 同期で履歴を汚さない」。呼び出し側が
 * `replace` で navigate する）。
 *
 * 外部からの変更（条件クリア・戻る・別条件からのリンク）に追従するため、
 * `value` prop が変わったら下書きを同期する。
 */
function KeywordField({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const [draft, setDraft] = useState(value)
  const wide = useMediaQuery(mdMediaQuery)

  useEffect(() => {
    setDraft(value)
  }, [value])

  useEffect(() => {
    if (draft === value) return
    const timer = setTimeout(() => onChange(draft), KEYWORD_DEBOUNCE_MS)
    return () => clearTimeout(timer)
    // value と onChange は「確定済みの値」と「確定する手段」であり、debounce の
    // 起点は draft の変化だけにする（value 変化のたびにタイマーを張り直さない）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft])

  return (
    <div className="relative min-w-0 flex-1 basis-0 md:basis-56">
      <SearchIcon
        className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
        aria-hidden
      />
      <Input
        type="search"
        aria-label="番組名・説明で検索"
        // md 未満は 3 つのアイコンと 1 行に並べるので短くする。名前（aria-label）は変えない。
        placeholder={wide ? '番組名・説明で検索' : '番組を検索'}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        className="h-11 pl-8"
      />
    </div>
  )
}

/** RecordingOrderSelect は録画一覧の並び順（放送日時の新しい順 / 古い順）。 */
export function RecordingOrderSelect({
  search,
  onChange,
}: {
  search: RecordingsPageSearch
  onChange: Update
}) {
  return (
    <ToolbarSelect
      label="並び順"
      value={search.order ?? ListRecordingsOrder.desc}
      options={[
        { value: ListRecordingsOrder.desc, label: '新しい順' },
        { value: ListRecordingsOrder.asc, label: '古い順' },
      ]}
      onChange={(order) =>
        onChange((s) => ({ ...s, order: order === ListRecordingsOrder.desc ? undefined : order }))
      }
    />
  )
}

/**
 * ToolbarSelect はツールバーの並び順。互いに排他な選択肢なので、md 以上は今の選択を出した
 * pop-up button（期間・絞り込みと同じ枠とシェブロン）、md 未満は枠なしのアイコンにする。
 * 操作は透明に重ねたネイティブの `<select>` が受ける（OS のピッカーに任せる。iOS 実機での操作は未検証）。
 *
 * **`options[0]` を既定値とし、それ以外のときアイコンに点を付ける。** md 未満はアイコンから
 * 今の並びが読めないので、既定から外れていることだけを他のボタンと同じ点で示す
 * （どの並びかは押せば分かる。読み上げは select の値で分かる）。
 */
export function ToolbarSelect<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: T
  options: readonly { value: T; label: string }[]
  onChange: (value: T) => void
}) {
  return (
    <label
      className={cn(
        toolbarButtonClass,
        'has-[select:focus-visible]:ring-3 has-[select:focus-visible]:ring-ring/50',
      )}
    >
      <ArrowUpDown className="size-5 md:hidden" aria-hidden />
      <span aria-hidden className="hidden md:inline">
        {options.find((option) => option.value === value)?.label}
      </span>
      <ChevronDown className="hidden size-4 text-muted-foreground md:block" aria-hidden />
      {value !== options[0]?.value && <ToolbarDot />}
      <select
        aria-label={label}
        value={value}
        onChange={(e) => onChange(e.target.value as T)}
        className="absolute inset-0 size-full cursor-pointer appearance-none opacity-0"
      >
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  )
}

/**
 * RuleSelect は絞り込みパネルのルール選択欄。
 *
 * `<select>` の value は文字列でも、URL へ戻す値は `parseRuleId` で検証した
 * 正の安全整数に揃える。ルール選択時の検索条件更新は `updateRuleFilter` に
 * 集約する。
 *
 * 同名のルールは `#<id>` を補助ラベルにして選択肢を押し分ける。名前が重複して
 * いないルールには補助ラベルを付けない --- 大多数の選択肢を読みやすく保つ。
 * DB の `rules.name` は一意ではなく、選択の identity は常に `rule.id` である。
 *
 * **`value` が一覧に無いとき、フォールバック option を足す。** 一覧に無い
 * `value`（削除済みルールで絞っている URL）を渡すと、React の
 * controlled `<select>` はどの option にも一致しないので先頭（「問わない」）
 * を選択状態にする（実測: jsdom で `value="99"` / `options=['', '8']` のとき
 * `selectedIndex === 0`）。適用中チップは `ルール #N` を出しているのに
 * パネルは「問わない」と表示され、URL と食い違う。ラベルはチップと同じ
 * `#N` フォールバックに揃える。
 */
function RuleSelect({
  value,
  rules,
  onChange,
}: {
  value: number | undefined
  rules: Rule[]
  onChange: (ruleId: number | undefined) => void
}) {
  const disambiguate = ruleDisambiguator(rules)
  const labelOf = (rule: Rule) => {
    const disambiguator = disambiguate(rule)
    return disambiguator === undefined ? rule.name : `${rule.name} (${disambiguator})`
  }

  return (
    <label className="flex h-11 min-w-0 items-center rounded-lg border border-border bg-background px-3 text-sm text-foreground">
      <span className="sr-only">ルール</span>
      <select
        aria-label="ルール"
        value={value === undefined ? '' : String(value)}
        onChange={(e) => onChange(parseRuleId(e.target.value))}
        className="h-6 min-w-0 flex-1 bg-transparent text-sm text-foreground outline-none"
      >
        <option value="">問わない</option>
        {value !== undefined && !rules.some((rule) => rule.id === value) && (
          <option value={String(value)}>ルール #{value}</option>
        )}
        {rules.map((rule) => (
          <option key={rule.id} value={String(rule.id)}>
            {labelOf(rule)}
          </option>
        ))}
      </select>
    </label>
  )
}

/**
 * updateRuleFilter はルール選択を検索条件へ反映する。
 *
 * 特定のルールを選んだ状態で `source=manual` / `source=unattributed` を残すと
 * 必ず 0 件になるため、`ruleId` を選んだ時点でこの 2 つだけ解除する
 * （`source='rule'` は `ruleId` と併存しても矛盾しないので残す。判定は
 * `parseRecordingsSearch` の正規化と同じ `isSourceMootWithRule` を使う）。
 */
function updateRuleFilter(search: RecordingsPageSearch, ruleId: number | undefined): RecordingsPageSearch {
  return {
    ...search,
    ruleId,
    source: ruleId !== undefined && isSourceMootWithRule(search.source) ? undefined : search.source,
  }
}

/**
 * FilterPanel は「絞り込み ▾」のパネル本体。
 *
 * チャンネル種別（`channelType`）はここに置かない --- 個々のチャンネルを選べる
 * `<ChannelPicker>` の方が細かく絞れ、issue #137 の UI 案（チャンネル / ジャンル /
 * 期間 / 状態 / 種別）にも種別独立の選択肢は無い。`qTarget`（番組名のみ /
 * 概要含む）も UI 案に無いので出さない --- 出しても検証できないコントロールを
 * 増やさない（「機能しないコントロールは置かない」の逆）。期間はここに置かず、
 * ツールバーの独立した操作にする（`RecordingPeriodMenu`）。
 */
function FilterPanel({
  search,
  services,
  siteNames,
  servicesPending,
  servicesError,
  rules,
  rulesPending,
  rulesError,
  onChange,
}: {
  search: RecordingsPageSearch
  services: Service[]
  /**
   * siteNames はレジストリの site 名（`GET /api/sites`）。
   *
   * **サービス射影から導かない。** ある site の `epg_services` がまだ空
   * （新設 site・直近の EPG 同期が失敗）だと、その site が候補から消え、
   * 一覧にはその site の録画が出ているのに絞れなくなる。site の権威は
   * レジストリであって EPG 射影ではない。
   */
  siteNames: string[]
  servicesPending: boolean
  servicesError: boolean
  rules: Rule[]
  rulesPending: boolean
  rulesError: boolean
  onChange: Update
}) {
  const [open, setOpen] = useState(false)
  const [channelOpen, setChannelOpen] = useState(false)
  const [noneSelected, setNoneSelected] = useState(false)
  const wide = useMediaQuery(mdMediaQuery)
  const selectedServices = useMemo(() => new Set(search.service ?? []), [search.service])
  const selectedGenres = useMemo(() => new Set(search.genre ?? []), [search.genre])
  const siteOptions = useMemo(
    () => [...new Set([...siteNames, ...(search.site ?? [])])].sort(),
    [siteNames, search.site],
  )
  const selectedSites = useMemo(() => new Set(search.site ?? []), [search.site])
  // site は別軸（`?site=`）なので、チャンネルの補足ラベルには入れない。
  // **同じチャンネルを 2 サイトで受けていても選択肢は 1 つ**（identity は
  // `Service.id`）なので、そこに片方の site 名を添えると誤読させる。
  const disambiguate = useMemo(() => serviceDisambiguator(services), [services])
  const secondaryLabel = (service: Service): string | undefined => {
    const label = disambiguate(service)
    return label === '' ? undefined : label
  }

  const updateServices = (next: ReadonlySet<number>) =>
    onChange((s) => ({
      ...s,
      service: next.size > 0 ? [...next].sort((a, b) => a - b) : undefined,
    }))

  const returnToFilters = () => {
    // 空選択は URL に表せず全局を意味する。親画面へ戻る時点で明示選択を解除する。
    if (noneSelected && selectedServices.size > 0) {
      onChange((s) => ({ ...s, service: undefined }))
    }
    setNoneSelected(false)
    setChannelOpen(false)
  }

  const handlePanelOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) returnToFilters()
  }

  // 点はこのパネルで選べる次元だけで判定する（期間は期間ボタン、encodeState はチップが示す）。
  const filtered = [search.service, search.site, search.genre, search.status, search.ruleId, search.source].some(
    (value) => value !== undefined,
  )

  return (
    <ToolbarPanel
      title={!wide && channelOpen ? 'チャンネル' : '絞り込み'}
      open={open}
      onOpenChange={handlePanelOpenChange}
      sheetLeading={
        !wide && channelOpen ? (
          <button
            type="button"
            aria-label="絞り込みに戻る"
            onClick={returnToFilters}
            className="h-11 justify-self-start rounded-lg px-2 text-base font-semibold text-primary hover:bg-muted"
          >
            ‹ 絞り込み
          </button>
        ) : undefined
      }
      triggerClassName={toolbarButtonClass}
      trigger={
        <>
          <ListFilter className="size-5 md:hidden" aria-hidden />
          <span className="sr-only md:not-sr-only">絞り込み</span>
          <ChevronDown className="hidden size-4 text-muted-foreground md:block" aria-hidden />
          {filtered && <ToolbarDot />}
        </>
      }
      popupWidthClassName="w-[min(22rem,90vw)]"
      bodyClassName="flex flex-col gap-4"
    >
      {!wide && channelOpen ? (
        servicesError ? (
          <p className="text-xs text-destructive">チャンネルの取得に失敗しました</p>
        ) : servicesPending ? (
          <p role="status" className="text-xs text-muted-foreground">
            読み込み中…
          </p>
        ) : (
          <ChannelPicker
            presentation="inline"
            services={services}
            selected={selectedServices}
            secondaryLabel={secondaryLabel}
            onChange={updateServices}
            onNoneSelectedChange={setNoneSelected}
          />
        )
      ) : (
        <>
          {servicesError ? (
            <p className="text-xs text-destructive">チャンネルの取得に失敗しました</p>
          ) : servicesPending ? (
            <p role="status" className="text-xs text-muted-foreground">
              読み込み中…
            </p>
          ) : wide ? (
            <section className="flex flex-col gap-1.5">
              <h3 className="text-xs font-medium text-muted-foreground">チャンネル</h3>
              <ChannelPicker
                services={services}
                selected={selectedServices}
                secondaryLabel={secondaryLabel}
                onChange={updateServices}
              />
            </section>
          ) : (
            <ChannelPicker
              presentation="filter-row"
              services={services}
              selected={selectedServices}
              secondaryLabel={secondaryLabel}
              onChange={updateServices}
              onEmbeddedOpen={() => setChannelOpen(true)}
            />
          )}

            {/* site はレジストリと現在の絞り込みの和集合が 2 サイト以上のときだけ
                出す。レジストリから消えた site も見えて外せるようにする。 */}
            {siteOptions.length > 1 && (
              <section className="flex flex-col gap-1.5">
                <h3 className="text-xs font-medium text-muted-foreground">サイト</h3>
                <div role="group" aria-label="サイト" className="flex flex-wrap gap-1.5">
                  {siteOptions.map((site) => (
                    <Chip
                      key={site}
                      active={selectedSites.has(site)}
                      onClick={() =>
                        onChange((s) => {
                          const next = selectedSites.has(site)
                            ? (s.site ?? []).filter((v) => v !== site)
                            : [...(s.site ?? []), site].sort()
                          return { ...s, site: next.length > 0 ? next : undefined }
                        })
                      }
                    >
                      {site}
                    </Chip>
                  ))}
                </div>
              </section>
            )}

            <section className="flex flex-col gap-1.5">
              <h3 className="text-xs font-medium text-muted-foreground">ジャンル</h3>
              <div role="group" aria-label="ジャンル" className="flex flex-wrap gap-1.5">
                {genreCodes.map((code) => (
                  <Chip
                    key={code}
                    active={selectedGenres.has(code)}
                    onClick={() =>
                      onChange((s) => {
                        const next = selectedGenres.has(code)
                          ? (s.genre ?? []).filter((g) => g !== code)
                          : [...(s.genre ?? []), code]
                        return { ...s, genre: next.length > 0 ? next : undefined }
                      })
                    }
                  >
                    {genreCodeLabel(code)}
                  </Chip>
                ))}
              </div>
            </section>

            <section className="flex flex-col gap-1.5">
              <h3 className="text-xs font-medium text-muted-foreground">状態</h3>
              <div role="group" aria-label="状態" className="flex flex-wrap gap-1.5">
                <Chip active={search.status === undefined} onClick={() => onChange((s) => ({ ...s, status: undefined }))}>
                  問わない
                </Chip>
                {recordingStatusValues.map((value) => (
                  <Chip
                    key={value}
                    active={search.status === value}
                    onClick={() => onChange((s) => ({ ...s, status: value }))}
                  >
                    {statusLabels[value]}
                  </Chip>
                ))}
              </div>
            </section>

            {/* ルール一覧が空でも `search.ruleId` があれば節を残す ---
                削除済みルールで絞っている状態を読めるようにするため
                （`RuleSelect` のフォールバック option と同じ理由）。
                取得中・失敗の表示はゲートの前に出す（理由が分かるようにする。
                チャンネル節と同じ流儀）。 */}
            {(rules.length > 0 || search.ruleId !== undefined || rulesPending || rulesError) && (
              <section className="flex flex-col gap-1.5">
                <h3 className="text-xs font-medium text-muted-foreground">ルール</h3>
                {rulesError ? (
                  <p className="text-xs text-destructive">ルールの取得に失敗しました</p>
                ) : rulesPending ? (
                  <p role="status" className="text-xs text-muted-foreground">
                    読み込み中…
                  </p>
                ) : (
                  <RuleSelect
                    value={search.ruleId}
                    rules={rules}
                    onChange={(ruleId) =>
                      onChange((s) => updateRuleFilter(s, ruleId))
                    }
                  />
                )}
              </section>
            )}

            <section className="flex flex-col gap-1.5">
              <h3 className="text-xs font-medium text-muted-foreground">種別</h3>
              <div role="group" aria-label="種別" className="flex flex-wrap gap-1.5">
                <Chip active={search.source === undefined} onClick={() => onChange((s) => ({ ...s, source: undefined }))}>
                  問わない
                </Chip>
                {recordingSourceValues.map((value) => (
                  <Chip
                    key={value}
                    active={search.source === value}
                    disabled={search.ruleId !== undefined && isSourceMootWithRule(value)}
                    onClick={() => onChange((s) => ({ ...s, source: value }))}
                  >
                    {sourceLabels[value]}
                  </Chip>
                ))}
              </div>
            </section>
        </>
      )}
    </ToolbarPanel>
  )
}
