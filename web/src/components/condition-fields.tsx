import { X } from 'lucide-react'
import { useState } from 'react'

import {
  ProgramSearchRequestChannelTypesItem,
  RuleTextMatchMode,
  RuleTextMatchTarget,
  type Service,
} from '@/api/generated'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import { Field, Input, Select } from '@/components/ui/field'
import { useAllSitesServices } from '@/lib/all-sites-services'
import { serviceDisambiguator } from '@/lib/service-label'
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
 * 取得を重複して書かなくて済むようにするため。
 *
 * **サービスの選択肢は `useAllSitesServices()`（`lib/all-sites-services.ts`）が
 * 全 site から `Service.id` で畳んで返す。** 保存されたルールは全 site で
 * 評価される（`rule_sites` が空なら全サイト）のに、選択肢を 1 site の観測
 * だけから作ると、その site が受けていないチャンネルを条件として名指しできない
 * （issue #290）。site は識別子の一部ではなく存在のスコープでしかないので、
 * 識別子を問う選択肢は観測ではなく識別子の集合で答える（理由の詳細は
 * `lib/all-sites-services.ts`）。
 *
 * `disabled` はルール保存中などフォーム全体を止めたいときに使う。全ての
 * input / select / Chip / Button に伝播する。
 *
 * **`ServiceFields` は最後に置く**（issue #309。読み込み中のレイアウトシフト
 * 対策）。以前は `TextMatchFields` の直後に置いていたが、サービス一覧の取得は
 * 他の節（チャンネル種別・ジャンル・時間帯・…）と違って非同期で、「読み込み中…」
 * の 1 行からチップの複数行へ描画が入れ替わると、既にレイアウト済みの後続の節を
 * 下へ押す（Lighthouse の CLS が検索モバイルで要改善域まで出た実測）。Layout
 * Instability API は「既に描画済みの要素が動くこと」を全部数える --- 新しい
 * 要素の挿入で既描画の要素が押し下げられる場合も、既描画の要素の消失・縮小で
 * 後続が引き上げられる場合も同じだけ数える（新しく挿し込まれた要素自身は前の
 * 位置を持たないので、それ自体が「動いた」と数えられることはない）。無償なのは
 * **後ろに既描画の要素が無い位置での出現・消滅・成長だけ**。`ServiceFields` を
 * 他の節より後ろに動かすと、チップ列が伸びたときに**押される側が折り目の外に
 * 出る**（フォームの最下部なので、モバイルでは折り目の 183px 下 --- 実測で
 * 390x844 のチップ列 top=1027）。**押される既描画の兄弟が無くなるわけではない**
 * --- `pages/search.tsx` は `<form>` の後ろに値札・ルール保存・検索結果を同じ
 * 縦カラムで積んでいるので、シフトは 0 にならず小さくなるだけ（実測: 390x844 で
 * 0、1280x900 で 0.00066、縦長の 390x1180 で 0.023。しきい値 0.10 以下。issue #290
 * でサービス選択肢を全 site の union に変えた後に測り直した値。フォームの形
 * ---「読み込み中…」の 1 行からチップの複数行へ入れ替わる `ServiceFields` の
 * 位置と、それより前が同期的に描かれること---は変えていないため、これ以前の
 * 実測値と同じ桁に収まっている）。issue #531 で `SiteFields`（サイトチップ）を
 * `ServiceFields` の手前に足した後も `web/e2e/cls.mjs`（390x844 / 1280x900）を
 * 測り直したが値は変わらない --- `SiteFields` の表示可否は `useAllSitesServices()`
 * が返す `sites`（`<SiteGate>` と同じクエリキーのキャッシュを再利用するだけで
 * 追加のリクエストは発生しない）と下書きが持つ site の和集合から**同期的**に
 * 決まり、その和集合が 2 つ以上のときしか描画しない（`cls.mjs` のフィクスチャは
 * 単一サイトかつ下書きが空なので DOM に一切増えない）。
 * **フォームが短くなる変更（節の折りたたみ・サービスより下の要素を上へ移す）は
 * この前提を崩す**ので `web/e2e/cls.mjs` を測り直す。他の節との間に依存は無い
 * ので、並びを変えても意味は変わらない --- 判定は `web/e2e/cls.mjs`①（直す前の
 * 実装で実際に落ちることを確認済み）。サービスが最下部に来る動線上の代償を
 * 受け入れた判断は `docs/frontend/search.md`。
 */
export function ConditionFields({ draft, onChange, disabled }: FieldsProps): React.ReactElement {
  const { services: serviceList, sites, isPending, isError } = useAllSitesServices()

  return (
    <>
      <TextMatchFields draft={draft} onChange={onChange} disabled={disabled} />
      <ChannelTypeFields draft={draft} onChange={onChange} disabled={disabled} />
      <GenreFields draft={draft} onChange={onChange} disabled={disabled} />
      <TimeWindowFields draft={draft} onChange={onChange} disabled={disabled} />
      <ScalarFields draft={draft} onChange={onChange} disabled={disabled} />
      <SiteFields draft={draft} sites={sites} onChange={onChange} disabled={disabled} />
      <ServiceFields
        draft={draft}
        services={serviceList}
        // 取得中と失敗を区別する。空のチップ列を「サービスが無い」と
        // 読ませない（サービスは条件の一次元なので、無いのと分からないのは違う）
        isPending={isPending}
        isError={isError}
        onChange={onChange}
        disabled={disabled}
      />
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
 * 配列が空でも見かけ上の 1 行を出すが、値が空の間は `draft.textMatches`
 * に実体化しない。対象・モード・チップの選択はローカル state に保持し、値を
 * 入れた瞬間にその選択ごと実体化する。値の全消しでも選択をローカルへ戻すため、
 * 検索・保存を空値で止めず、再入力時にユーザーの選択も失わない。
 *
 * この非実体化は単一行だけに適用する。複数行の 1 行を空にする操作は編集中の
 * 位置を保つ必要があるので、行を勝手に消さない。明示的な削除（X）は選択も
 * 捨てる操作としてローカル state を既定値へ戻す。
 *
 * **配列が空の間は「条件を追加」ボタンと行の削除（X）を出さない。** 見かけ上
 * の 1 行目は既に `rows` が出しているので、実体の無い行に対してこれらの
 * ボタンを出すと「押しても何も起きない」死んだコントロールになる。
 */
function TextMatchFields({ draft, onChange, disabled }: FieldsProps) {
  const [unmaterialized, setUnmaterialized] = useState<TextMatchDraft>(newTextMatch)

  const update = (index: number, patch: Partial<TextMatchDraft>) => {
    const next = { ...(draft.textMatches[index] ?? unmaterialized), ...patch }

    if (index === 0 && draft.textMatches.length <= 1 && next.value === '') {
      setUnmaterialized(next)
      if (draft.textMatches.length === 1) {
        onChange((d) => ({ ...d, textMatches: [] }))
      }
      return
    }

    if (draft.textMatches.length === 0) {
      setUnmaterialized(newTextMatch())
      onChange((d) => ({ ...d, textMatches: [next] }))
      return
    }

    onChange((d) => ({
      ...d,
      textMatches: d.textMatches.map((m, i) => (i === index ? { ...m, ...patch } : m)),
    }))
  }

  const hasRows = draft.textMatches.length > 0
  // 配列が空でも 1 行目は見かけ上出す。「条件を追加」で新規行を作ってからで
  // ないと打てない、という 1 クリック分の迂回を無くすため。
  const rows = hasRows ? draft.textMatches : [unmaterialized]

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
                    onClick={() => {
                      if (draft.textMatches.length === 1) setUnmaterialized(newTextMatch())
                      onChange((d) => ({
                        ...d,
                        textMatches: d.textMatches.filter((_, i) => i !== index),
                      }))
                    }}
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

/**
 * SiteFields はサイト軸の入力（issue #531）。
 *
 * **`sites` は `GET /api/recordings` の `?site=` と同じ絞り込み軸**（軸内は OR、
 * 空 = 全サイト）。以前はこの次元がフォームに無く、検索は常に `useCurrentSite()`
 * （先頭サイト）だけを対象にし、保存されるルールの `sites` は UI から編集
 * できなかった。
 *
 * **チップの選択肢は「レジストリの site」と「下書きが既に持つ site」の和集合**
 * にする。`?ruleId=` で開いたルールの `rule_sites` にレジストリから消えた
 * site 名が残っていた場合でも、和集合に含めることでチップとして見え、
 * 明示的に外せる --- レジストリの一覧だけを選択肢にすると、消えた site は
 * チップごと消えて画面内で外す手段が無くなる（以前の「未解決」はこれが
 * 原因だった。`docs/frontend/search.md` 参照）。
 *
 * **表示可否もこの和集合（`options`）で判定する。** レジストリだけを見て
 * `sites.length <= 1` で隠すと、レジストリが 1 site に縮んだ環境で下書きが
 * 別の（消えた）site を持つケースが再び「見えない」に戻り、上の未解決が復活する
 * --- 単一サイト運用で下書きも空 / レジストリと同じ 1 件のときは `options` も
 * 1 件のままなので、素直な単一サイト構成では何も変わらない
 * 録画一覧の絞り込みと同じく、レジストリと現在の条件値の和集合で表示可否を
 * 判定する。
 */
function SiteFields({
  draft,
  sites,
  onChange,
  disabled,
}: FieldsProps & { sites: string[] }) {
  const options = [...new Set([...sites, ...draft.sites])].sort()

  if (options.length <= 1) return null

  return (
    <Section title="サイト">
      <div role="group" aria-label="サイト" className="flex flex-wrap gap-2">
        {options.map((site) => (
          <Chip
            key={site}
            active={draft.sites.includes(site)}
            disabled={disabled}
            onClick={() =>
              onChange((d) => ({
                ...d,
                sites: d.sites.includes(site)
                  ? d.sites.filter((s) => s !== site)
                  : [...d.sites, site],
              }))
            }
          >
            {site}
          </Chip>
        ))}
      </div>
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

  // 同じ名前のサービス（ワンセグ / サブサービス等）が並ぶとき、リモコン番号・
  // 物理チャンネル・serviceId から補助ラベルを作る（issue #306）。
  // `services` は `useAllSitesServices()` が毎レンダー新しい配列を返す
  // （identity は保証しない）ため `useMemo` は毎回不一致になり無意味だった
  // ---計算コストの実測に基づく最適化ではない（未測定）ので、素の呼び出しに
  // 戻す。再び安定させたくなったらまず実測してから戻すこと。
  const disambiguate = serviceDisambiguator(services)

  return (
    <Section title="チャンネル">
      {isError ? (
        <p className="text-xs text-destructive">チャンネルの取得に失敗しました</p>
      ) : isPending ? (
        <p role="status" className="text-xs text-muted-foreground">
          読み込み中…
        </p>
      ) : services.length === 0 ? (
        <p className="text-xs text-muted-foreground">チャンネルがありません</p>
      ) : (
        <div role="group" aria-label="チャンネル" className="flex flex-wrap gap-2">
          {services.map((service) => {
            const secondary = disambiguate(service)
            return (
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
                              s.networkId === service.networkId &&
                              s.serviceId === service.serviceId
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
                {secondary !== undefined && <span className="ml-1">（{secondary}）</span>}
              </Chip>
            )
          })}
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
