import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { ChevronDown, Search as SearchIcon, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'

import { ListRecordingsOrder, useListSites, type Service } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ChannelPicker } from '@/components/channel-picker'
import { Chip } from '@/components/ui/chip'
import { Field, Input } from '@/components/ui/field'
import { useAllSitesServices } from '@/lib/all-sites-services'
import { genreCodeLabel, genreCodes } from '@/lib/program-search'
import { serviceDisambiguator } from '@/lib/service-label'
import {
  clearRecordingsFilters,
  describeRecordingsFilters,
  isoToLocalDateTimeInput,
  localDateTimeInputToIso,
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
 * RecordingFilters は録画検索の条件 UI（issue #137）。
 *
 * 状態は一切持たない（キーワード入力欄の debounce 用の下書きを除く）。条件は
 * すべて呼び出し側（`pages/recordings.tsx`）が URL の search として持ち、
 * ここは表示と `onChange` 呼び出しに徹する --- 条件の永続化・共有・戻るボタン
 * との整合は URL 側の責務であり、ここに複製しない。
 */
export function RecordingFilters({
  search,
  onChange,
}: {
  search: RecordingsPageSearch
  onChange: Update
}) {
  const sitesQuery = useListSites()
  const sites = unwrap(sitesQuery.data) ?? []
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

  const chips = describeRecordingsFilters(search, serviceLabelById)

  return (
    <div className="flex flex-col gap-2 border-t border-border px-4 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <KeywordField
          value={search.q ?? ''}
          onChange={(q) => onChange((s) => ({ ...s, q: q.trim() === '' ? undefined : q }))}
        />
        <FilterPanel
          search={search}
          services={serviceList}
          siteNames={sites}
          servicesPending={servicesPending}
          servicesError={servicesError}
          onChange={onChange}
        />
        <OrderSelect
          value={search.order ?? ListRecordingsOrder.desc}
          onChange={(order) =>
            onChange((s) => ({ ...s, order: order === ListRecordingsOrder.desc ? undefined : order }))
          }
        />
      </div>

      {chips.length > 0 && (
        <div role="group" aria-label="適用中の条件" className="flex flex-wrap items-center gap-1.5">
          {chips.map((chip) => (
            <button
              key={chip.key}
              type="button"
              onClick={() => onChange((s) => chip.clear(s))}
              className="flex items-center gap-1 rounded-full border border-border bg-muted px-2.5 py-1 text-xs text-foreground transition-colors hover:bg-muted/70"
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
    <div className="relative min-w-0 flex-1 basis-56">
      <SearchIcon
        className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
        aria-hidden
      />
      <Input
        type="search"
        aria-label="番組名・説明で検索"
        placeholder="番組名・説明で検索"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        className="h-11 pl-8"
      />
    </div>
  )
}

function OrderSelect({
  value,
  onChange,
}: {
  value: ListRecordingsOrder
  onChange: (order: ListRecordingsOrder) => void
}) {
  return (
    <label className="flex h-11 shrink-0 items-center rounded-lg border border-border bg-background px-3 text-sm text-foreground">
      <span className="sr-only">並び順</span>
      <select
        aria-label="並び順"
        value={value}
        onChange={(e) => onChange(e.target.value as ListRecordingsOrder)}
        className="bg-transparent text-sm text-foreground outline-none"
      >
        <option value={ListRecordingsOrder.desc}>新しい順</option>
        <option value={ListRecordingsOrder.asc}>古い順</option>
      </select>
    </label>
  )
}

/**
 * FilterPanel は「絞り込み ▾」のポップオーバー本体。
 *
 * チャンネル種別（`channelType`）はここに置かない --- 個々のチャンネルを選べる
 * `<ChannelPicker>` の方が細かく絞れ、issue #137 の UI 案（チャンネル / ジャンル /
 * 期間 / 状態 / 種別）にも種別独立の選択肢は無い。`qTarget`（番組名のみ /
 * 概要含む）も UI 案に無いので出さない --- 出しても検証できないコントロールを
 * 増やさない（「機能しないコントロールは置かない」の逆）。
 */
function FilterPanel({
  search,
  services,
  siteNames,
  servicesPending,
  servicesError,
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
  onChange: Update
}) {
  const [open, setOpen] = useState(false)
  const selectedServices = useMemo(() => new Set(search.service ?? []), [search.service])
  const selectedGenres = useMemo(() => new Set(search.genre ?? []), [search.genre])
  const selectedSites = useMemo(() => new Set(search.site ?? []), [search.site])
  // site は別軸（`?site=`）なので、チャンネルの補足ラベルには入れない。
  // **同じチャンネルを 2 サイトで受けていても選択肢は 1 つ**（identity は
  // `Service.id`）なので、そこに片方の site 名を添えると誤読させる。
  const disambiguate = useMemo(() => serviceDisambiguator(services), [services])
  const secondaryLabel = (service: Service): string | undefined => {
    const label = disambiguate(service)
    return label === '' ? undefined : label
  }

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={setOpen}>
      <PopoverPrimitive.Trigger
        className="flex h-11 shrink-0 items-center gap-1.5 rounded-lg border border-border bg-background px-3 text-sm text-foreground transition-colors hover:bg-muted aria-expanded:bg-muted aria-expanded:text-foreground"
      >
        絞り込み
        <ChevronDown className="size-4 text-muted-foreground" aria-hidden="true" />
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        {/* positionMethod は 'fixed'。理由は components/channel-picker.tsx と同じ
            （sticky なトリガーと 'absolute' ポップアップのスクロール追従のずれ）。 */}
        <PopoverPrimitive.Positioner
          className="z-50 outline-none"
          positionMethod="fixed"
          side="bottom"
          align="start"
          sideOffset={6}
        >
          <PopoverPrimitive.Popup
            aria-label="絞り込み"
            className="flex max-h-[min(34rem,80vh)] w-[min(22rem,90vw)] flex-col gap-4 overflow-y-auto rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-md outline-none"
          >
            <section className="flex flex-col gap-1.5">
              <h3 className="text-xs font-medium text-muted-foreground">チャンネル</h3>
              {servicesError ? (
                <p className="text-xs text-destructive">チャンネルの取得に失敗しました</p>
              ) : servicesPending ? (
                <p role="status" className="text-xs text-muted-foreground">
                  読み込み中…
                </p>
              ) : (
                <ChannelPicker
                  services={services}
                  selected={selectedServices}
                  secondaryLabel={secondaryLabel}
                  onChange={(next) =>
                    onChange((s) => ({
                      ...s,
                      service: next.size > 0 ? [...next].sort((a, b) => a - b) : undefined,
                    }))
                  }
                />
              )}
            </section>

            {/* site は 2 サイト以上のときだけ出す。1 サイト構成では選択肢が
                1 つしかなく、絞る意味がない（機能しないコントロールは置かない）。 */}
            {siteNames.length > 1 && (
              <section className="flex flex-col gap-1.5">
                <h3 className="text-xs font-medium text-muted-foreground">サイト</h3>
                <div role="group" aria-label="サイト" className="flex flex-wrap gap-1.5">
                  {siteNames.map((site) => (
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
              <h3 className="text-xs font-medium text-muted-foreground">期間（番組開始時刻）</h3>
              <div className="flex flex-wrap gap-2">
                <Field label="開始日時" className="min-w-0 flex-1">
                  <Input
                    type="datetime-local"
                    value={isoToLocalDateTimeInput(search.from)}
                    onChange={(e) =>
                      onChange((s) => ({ ...s, from: localDateTimeInputToIso(e.target.value) }))
                    }
                  />
                </Field>
                <Field label="終了日時" className="min-w-0 flex-1">
                  <Input
                    type="datetime-local"
                    value={isoToLocalDateTimeInput(search.to)}
                    onChange={(e) =>
                      onChange((s) => ({ ...s, to: localDateTimeInputToIso(e.target.value) }))
                    }
                  />
                </Field>
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
                    onClick={() => onChange((s) => ({ ...s, source: value }))}
                  >
                    {sourceLabels[value]}
                  </Chip>
                ))}
              </div>
            </section>
          </PopoverPrimitive.Popup>
        </PopoverPrimitive.Positioner>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  )
}
