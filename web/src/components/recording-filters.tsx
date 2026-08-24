import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { useQueries } from '@tanstack/react-query'
import { ChevronDown, Search as SearchIcon, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'

import {
  getListServicesQueryOptions,
  ListRecordingsOrder,
  useListSites,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ChannelPicker } from '@/components/channel-picker'
import { Chip } from '@/components/ui/chip'
import { Field, Input } from '@/components/ui/field'
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
  const serviceQueries = useQueries({
    queries: sites.map((site) => getListServicesQueryOptions(site)),
  })

  const serviceList: Service[] = []
  const siteByService = new Map<Service, string>()
  for (const [i, query] of serviceQueries.entries()) {
    const site = sites[i]
    for (const service of unwrap(query.data) ?? []) {
      serviceList.push(service)
      siteByService.set(service, site)
    }
  }

  const disambiguate = serviceDisambiguator(serviceList)
  const serviceLabelByKey = new Map<string, string>()
  const legacyServices = new Map<string, Service[]>()
  for (const service of serviceList) {
    const site = siteByService.get(service) ?? ''
    const disambiguator = disambiguate(service)
    const suffix = [site, disambiguator].filter((part) => part !== undefined && part !== '').join('・')
    serviceLabelByKey.set(
      `${site}:${service.networkId}:${service.serviceId}`,
      suffix === '' ? service.name : `${service.name} (${suffix})`,
    )
    const legacyKey = `${site}:${service.serviceId}`
    const group = legacyServices.get(legacyKey)
    if (group) group.push(service)
    else legacyServices.set(legacyKey, [service])
  }
  for (const [key, group] of legacyServices) {
    const site = siteByService.get(group[0]) ?? ''
    serviceLabelByKey.set(
      key,
      group.length === 1
        ? `${group[0].name} (${site})`
        : `serviceId ${group[0].serviceId} (${site}・network指定なし)`,
    )
  }

  const chips = describeRecordingsFilters(search, serviceLabelByKey)

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
          siteByService={siteByService}
          servicesPending={sitesQuery.isPending || serviceQueries.some((q) => q.isPending)}
          servicesError={sitesQuery.isError || serviceQueries.some((q) => q.isError)}
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
  siteByService,
  servicesPending,
  servicesError,
  onChange,
}: {
  search: RecordingsPageSearch
  services: Service[]
  /** 各 Service がどの site の一覧に由来するか。ChannelPicker の複合キーに使う。 */
  siteByService: ReadonlyMap<Service, string>
  servicesPending: boolean
  servicesError: boolean
  onChange: Update
}) {
  const [open, setOpen] = useState(false)
  const selectedServices = useMemo(() => {
    const selected = new Set<string>()
    for (const value of search.service ?? []) {
      const parts = value.split(':')
      if (parts.length === 3) {
        selected.add(value)
        continue
      }
      if (parts.length !== 2) continue
      const [site, serviceId] = parts
      for (const service of services) {
        if (siteByService.get(service) === site && String(service.serviceId) === serviceId) {
          selected.add(`${site}:${service.networkId}:${service.serviceId}`)
        }
      }
    }
    return selected
  }, [search.service, services, siteByService])
  const selectedGenres = useMemo(() => new Set(search.genre ?? []), [search.genre])
  const multiSite = useMemo(() => new Set(siteByService.values()).size > 1, [siteByService])
  const disambiguate = useMemo(() => serviceDisambiguator(services), [services])
  const serviceKey = (service: Service) =>
    `${siteByService.get(service) ?? ''}:${service.networkId}:${service.serviceId}`
  const secondaryLabel = (service: Service): string | undefined => {
    const parts = [multiSite ? siteByService.get(service) : undefined, disambiguate(service)].filter(
      (part): part is string => part !== undefined && part !== '',
    )
    return parts.length > 0 ? parts.join('・') : undefined
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
                <p className="text-xs text-muted-foreground">読み込み中…</p>
              ) : (
                <ChannelPicker
                  services={services}
                  selected={selectedServices}
                  keyOf={serviceKey}
                  secondaryLabel={secondaryLabel}
                  onChange={(next) =>
                    onChange((s) => ({
                      ...s,
                      service: next.size > 0 ? [...next].sort() : undefined,
                    }))
                  }
                />
              )}
            </section>

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
