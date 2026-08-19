import { X } from 'lucide-react'
import { useMemo } from 'react'

import {
  useListServices,
  ProgramSearchRequestChannelTypesItem,
  RuleTextMatchMode,
  RuleTextMatchTarget,
  type Service,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import { Field, Input, Select } from '@/components/ui/field'
import { useCurrentSite } from '@/lib/site'
import {
  allWeekdays,
  genreCodeLabel,
  genreCodes,
  hasWeekday,
  newTextMatch,
  newTimeWindow,
  secToTimeValue,
  timeValueToSec,
  toggleWeekday,
  weekdayLabels,
  type SearchDraft,
  type TextMatchDraft,
  type TimeWindowDraft,
} from '@/lib/program-search'

const textTargetLabels: Record<RuleTextMatchTarget, string> = {
  name: '番組名',
  description: '概要',
  extended: '詳細',
}

const textModeLabels: Record<RuleTextMatchMode, string> = {
  keyword: 'キーワード',
  regex: '正規表現',
}

const channelTypes = [
  ProgramSearchRequestChannelTypesItem.GR,
  ProgramSearchRequestChannelTypesItem.BS,
  ProgramSearchRequestChannelTypesItem.CS,
  ProgramSearchRequestChannelTypesItem.SKY,
]

type FieldsProps = {
  draft: SearchDraft
  onChange: (update: (draft: SearchDraft) => SearchDraft) => void
  disabled?: boolean
}

/**
 * ConditionFields は検索条件（= ルール条件）の全次元の入力 UI。
 *
 * `internal/rulequery` を通る条件は検索（`ProgramSearchRequest`）とルール
 * （`RuleInput`）でフィールド名まで完全に同形なので、両方の画面がこの 1 つの
 * コンポーネントを使う（検索: 条件を試す / ルール: 条件を編集する）。
 * サービス一覧の取得をここに閉じ込めているのは、呼び出し側 2 箇所が同じ
 * `useListServices` を重複して書かなくて済むようにするため。
 *
 * `disabled` はルール保存中などフォーム全体を止めたいときに使う。全ての
 * input / select / Chip / Button に伝播する。
 */
export function ConditionFields({ draft, onChange, disabled }: FieldsProps): React.ReactElement {
  const site = useCurrentSite()
  const services = useListServices(site)
  const serviceList = useMemo(() => unwrap(services.data) ?? [], [services.data])

  return (
    <>
      <TextMatchFields draft={draft} onChange={onChange} disabled={disabled} />
      <ServiceFields
        draft={draft}
        services={serviceList}
        // 取得中と失敗を区別する。空のチップ列を「サービスが無い」と
        // 読ませない（サービスは条件の一次元なので、無いのと分からないのは違う）
        isPending={services.isPending}
        isError={services.isError}
        onChange={onChange}
        disabled={disabled}
      />
      <ChannelTypeFields draft={draft} onChange={onChange} disabled={disabled} />
      <GenreFields draft={draft} onChange={onChange} disabled={disabled} />
      <TimeWindowFields draft={draft} onChange={onChange} disabled={disabled} />
      <ScalarFields draft={draft} onChange={onChange} disabled={disabled} />
    </>
  )
}

function Section({
  title,
  children,
  action,
}: {
  title: string
  children: React.ReactNode
  action?: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-xs font-medium text-muted-foreground">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  )
}

/**
 * TextMatchFields はテキスト条件の入力。
 *
 * **1 行目は「条件を追加」を押さなくても常に編集できる**（issue #305）。
 * 以前は `draft.textMatches` が空の間、この節は「指定なし」という文言だけを
 * 出し、実際に打てる欄は「条件を追加」を押した先にしかなかった --- 検索・
 * ルールいずれの画面でもこの節は `ConditionFields` の先頭に来るのに、
 * 初画面で打てる場所が無いという回帰だった。
 *
 * 配列が空でも `rows`（下）に見かけ上の 1 行を出し、そこへの入力（`update`）
 * が起きた瞬間だけ `draft.textMatches` に実体化する。触れないままなら
 * 「指定なし（すべての番組が対象）」のまま送られる、という既存の意味は
 * 変えない（`draftError` は配列に実体が無い間は何も見ない）。
 *
 * **配列が空の間は「条件を追加」ボタンと行の削除（X）を出さない。** 見かけ上
 * の 1 行目は既に `rows` が出しているので、実体の無い行に対してこれらの
 * ボタンを出すと「押しても何も起きない」死んだコントロールになる（レビュー
 * 指摘）。実体化（`index < draft.textMatches.length`）した行にだけ出す ---
 * これは「チップだけ押して実体化し、値が空のまま `draftError` に落ちた」
 * ときの戻り道にもなる（X が効くようになる）。
 */
function TextMatchFields({ draft, onChange, disabled }: FieldsProps) {
  const update = (index: number, patch: Partial<TextMatchDraft>) =>
    onChange((d) =>
      index < d.textMatches.length
        ? {
            ...d,
            textMatches: d.textMatches.map((m, i) => (i === index ? { ...m, ...patch } : m)),
          }
        : // まだ実体の無い見かけ上の行（下の `rows` のフォールバック）への
          // 最初の操作。実体化して配列に加える。
          { ...d, textMatches: [...d.textMatches, { ...newTextMatch(), ...patch }] },
    )

  const hasRows = draft.textMatches.length > 0
  // 配列が空でも 1 行目は見かけ上出す。「条件を追加」で新規行を作ってからで
  // ないと打てない、という 1 クリック分の迂回を無くすため。
  const rows = hasRows ? draft.textMatches : [newTextMatch()]

  return (
    <Section
      title="テキスト条件"
      action={
        // 配列が空の間はボタン自体を出さない。1 行目は上の `rows` が既に
        // 見かけ上出しているので、押しても増えない（＝何も起きない）ボタンを
        // 見せない。1 行目に何か入力して実体化すれば現れ、2 行目以降を追加
        // できる。
        hasRows ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={disabled}
            onClick={() =>
              onChange((d) => ({ ...d, textMatches: [...d.textMatches, newTextMatch()] }))
            }
          >
            条件を追加
          </Button>
        ) : undefined
      }
    >
      {!hasRows && (
        // 「時間帯」節が空のときと同じ形にする。`ConditionFields` は検索と
        // ルールの両方が使うので、動詞（検索する／保存する）を含めると
        // 片方の画面で事実として誤る（ルール画面で条件ゼロは「検索したら
        // 全件」ではなく「全番組を録り続ける」）。
        <p className="text-xs text-muted-foreground">指定なし（すべての番組が対象）</p>
      )}
      <ul className="flex flex-col gap-3">
        {rows.map((match, index) => (
          <li key={index} className="flex flex-col gap-2 rounded-lg border border-border p-3">
            <div className="flex gap-2">
              {/* 見出しは短く、読み上げの名前は行番号込みにする（同じ見出しの
                  入力が行ごとに増えるため、名前が重複すると指し示せない） */}
              <Field label="対象" className="w-28 shrink-0">
                <Select
                  aria-label={`テキスト条件 ${index + 1} の対象`}
                  value={match.target}
                  disabled={disabled}
                  onChange={(e) => update(index, { target: e.target.value as RuleTextMatchTarget })}
                >
                  {Object.entries(textTargetLabels).map(([value, label]) => (
                    <option key={value} value={value}>
                      {label}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="モード" className="w-28 shrink-0">
                <Select
                  aria-label={`テキスト条件 ${index + 1} のモード`}
                  value={match.mode}
                  disabled={disabled}
                  onChange={(e) => update(index, { mode: e.target.value as RuleTextMatchMode })}
                >
                  {Object.entries(textModeLabels).map(([value, label]) => (
                    <option key={value} value={value}>
                      {label}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="値" className="min-w-0 flex-1">
                <Input
                  aria-label={`テキスト条件 ${index + 1} の値`}
                  value={match.value}
                  placeholder={match.mode === 'regex' ? '^ニュース' : 'ニュース'}
                  disabled={disabled}
                  onChange={(e) => update(index, { value: e.target.value })}
                />
              </Field>
              {
                // 実体の無い見かけ上の行には出さない。無いものを消す
                // ボタンは「押しても何も起きない」死んだコントロールになる
                // （レビュー指摘）。
                index < draft.textMatches.length && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={`テキスト条件 ${index + 1} を削除`}
                    className="mt-4 shrink-0"
                    disabled={disabled}
                    onClick={() =>
                      onChange((d) => ({
                        ...d,
                        textMatches: d.textMatches.filter((_, i) => i !== index),
                      }))
                    }
                  >
                    <X />
                  </Button>
                )
              }
            </div>
            <div className="flex flex-wrap gap-2">
              <Chip
                active={match.caseSensitive}
                disabled={disabled}
                onClick={() => update(index, { caseSensitive: !match.caseSensitive })}
              >
                大文字小文字を区別
              </Chip>
              <Chip
                active={match.negate}
                disabled={disabled}
                onClick={() => update(index, { negate: !match.negate })}
              >
                除外
              </Chip>
            </div>
            {match.mode === 'regex' && (
              <p className="text-xs text-muted-foreground">
                Postgres の POSIX ARE。先読み・後読みは使えません
              </p>
            )}
          </li>
        ))}
      </ul>
    </Section>
  )
}

function ServiceFields({
  draft,
  services,
  isPending,
  isError,
  onChange,
  disabled,
}: FieldsProps & { services: Service[]; isPending: boolean; isError: boolean }) {
  const selected = (service: Service) =>
    draft.services.some(
      (s) => s.networkId === service.networkId && s.serviceId === service.serviceId,
    )

  return (
    <Section title="サービス">
      {isError ? (
        <p className="text-xs text-destructive">サービスの取得に失敗しました</p>
      ) : isPending ? (
        <p className="text-xs text-muted-foreground">読み込み中…</p>
      ) : services.length === 0 ? (
        <p className="text-xs text-muted-foreground">サービスがありません</p>
      ) : (
        <div role="group" aria-label="サービス" className="flex flex-wrap gap-2">
          {services.map((service) => (
            <Chip
              key={`${service.networkId}-${service.serviceId}`}
              active={selected(service)}
              disabled={disabled}
              onClick={() =>
                onChange((d) => ({
                  ...d,
                  services: selected(service)
                    ? d.services.filter(
                        (s) =>
                          !(
                            s.networkId === service.networkId && s.serviceId === service.serviceId
                          ),
                      )
                    : [
                        ...d.services,
                        { networkId: service.networkId, serviceId: service.serviceId },
                      ],
                }))
              }
            >
              {service.name}
            </Chip>
          ))}
        </div>
      )}
    </Section>
  )
}

function ChannelTypeFields({ draft, onChange, disabled }: FieldsProps) {
  return (
    <Section title="チャンネル種別">
      <div role="group" aria-label="チャンネル種別" className="flex flex-wrap gap-2">
        {channelTypes.map((type) => (
          <Chip
            key={type}
            active={draft.channelTypes.includes(type)}
            disabled={disabled}
            onClick={() =>
              onChange((d) => ({
                ...d,
                channelTypes: d.channelTypes.includes(type)
                  ? d.channelTypes.filter((t) => t !== type)
                  : [...d.channelTypes, type],
              }))
            }
          >
            {type}
          </Chip>
        ))}
      </div>
    </Section>
  )
}

function GenreFields({ draft, onChange, disabled }: FieldsProps) {
  return (
    <Section title="ジャンル">
      <div role="group" aria-label="ジャンル" className="flex flex-wrap gap-2">
        {genreCodes.map((code) => (
          <Chip
            key={code}
            active={draft.genres.includes(code)}
            disabled={disabled}
            onClick={() =>
              onChange((d) => ({
                ...d,
                genres: d.genres.includes(code)
                  ? d.genres.filter((g) => g !== code)
                  : [...d.genres, code],
              }))
            }
          >
            {genreCodeLabel(code)}
          </Chip>
        ))}
      </div>
    </Section>
  )
}

function TimeWindowFields({ draft, onChange, disabled }: FieldsProps) {
  const update = (index: number, patch: Partial<TimeWindowDraft>) =>
    onChange((d) => ({
      ...d,
      times: d.times.map((t, i) => (i === index ? { ...t, ...patch } : t)),
    }))

  return (
    <Section
      title="時間帯"
      action={
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={disabled}
          onClick={() => onChange((d) => ({ ...d, times: [...d.times, newTimeWindow()] }))}
        >
          時間帯を追加
        </Button>
      }
    >
      {draft.times.length === 0 ? (
        <p className="text-xs text-muted-foreground">指定なし（すべての時間帯が対象）</p>
      ) : (
        <ul className="flex flex-col gap-3">
          {draft.times.map((time, index) => (
            <li key={index} className="flex flex-col gap-2 rounded-lg border border-border p-3">
              <div
                role="group"
                aria-label={`時間帯 ${index + 1} の曜日`}
                className="flex flex-wrap gap-1.5"
              >
                {weekdayLabels.map((label, bit) => (
                  <Chip
                    key={label}
                    active={hasWeekday(time.weekdays, bit)}
                    disabled={disabled}
                    onClick={() => update(index, { weekdays: toggleWeekday(time.weekdays, bit) })}
                  >
                    {label}
                  </Chip>
                ))}
                <Chip
                  active={time.weekdays === allWeekdays}
                  disabled={disabled}
                  onClick={() => update(index, { weekdays: allWeekdays })}
                >
                  毎日
                </Chip>
              </div>
              <div className="flex items-end gap-2">
                <Field label="開始" className="w-28">
                  <Input
                    type="time"
                    aria-label={`時間帯 ${index + 1} の開始`}
                    value={secToTimeValue(time.startSec)}
                    disabled={disabled}
                    onChange={(e) => update(index, { startSec: timeValueToSec(e.target.value) })}
                  />
                </Field>
                <Field label="終了" className="w-28">
                  <Input
                    type="time"
                    aria-label={`時間帯 ${index + 1} の終了`}
                    value={secToTimeValue(time.endSec)}
                    disabled={disabled}
                    onChange={(e) => update(index, { endSec: timeValueToSec(e.target.value) })}
                  />
                </Field>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={`時間帯 ${index + 1} を削除`}
                  disabled={disabled}
                  onClick={() =>
                    onChange((d) => ({ ...d, times: d.times.filter((_, i) => i !== index) }))
                  }
                >
                  <X />
                </Button>
              </div>
              {/* 終了 < 開始 は翌日跨ぎ（internal/rulequery）。00:00 を終了に指定すれば
                  「開始から深夜 0 時まで」になるので、その説明を出す */}
              {time.endSec < time.startSec && (
                <p className="text-xs text-muted-foreground">翌日にまたがる時間帯として扱います</p>
              )}
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

function ScalarFields({ draft, onChange, disabled }: FieldsProps) {
  return (
    <div className="flex flex-col gap-5">
      <Section title="無料放送">
        <div role="group" aria-label="無料放送" className="flex flex-wrap gap-2">
          {(
            [
              ['any', '問わない'],
              ['yes', '無料のみ'],
              ['no', '有料のみ'],
            ] as const
          ).map(([value, label]) => (
            <Chip
              key={value}
              active={draft.isFree === value}
              disabled={disabled}
              onClick={() => onChange((d) => ({ ...d, isFree: value }))}
            >
              {label}
            </Chip>
          ))}
        </div>
      </Section>

      <Section title="放送時間">
        <div className="flex gap-2">
          <Field label="下限（分）" className="w-28">
            <Input
              type="number"
              min={0}
              inputMode="numeric"
              value={draft.durationMinMinutes}
              disabled={disabled}
              onChange={(e) => onChange((d) => ({ ...d, durationMinMinutes: e.target.value }))}
            />
          </Field>
          <Field label="上限（分）" className="w-28">
            <Input
              type="number"
              min={0}
              inputMode="numeric"
              value={draft.durationMaxMinutes}
              disabled={disabled}
              onChange={(e) => onChange((d) => ({ ...d, durationMaxMinutes: e.target.value }))}
            />
          </Field>
        </div>
      </Section>

      <Section title="期間">
        <div className="flex flex-wrap gap-2">
          <Field label="開始日時" className="w-56">
            <Input
              type="datetime-local"
              value={draft.periodStartAt}
              disabled={disabled}
              onChange={(e) => onChange((d) => ({ ...d, periodStartAt: e.target.value }))}
            />
          </Field>
          <Field label="終了日時" className="w-56">
            <Input
              type="datetime-local"
              value={draft.periodEndAt}
              disabled={disabled}
              onChange={(e) => onChange((d) => ({ ...d, periodEndAt: e.target.value }))}
            />
          </Field>
        </div>
      </Section>
    </div>
  )
}
