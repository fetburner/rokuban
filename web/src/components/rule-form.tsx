import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import {
  getGetRuleQueryKey,
  getListRulesQueryKey,
  useCreateRule,
  useUpdateRule,
  type Rule,
} from '@/api/generated'
import { ApiError } from '@/api/client'
import { apiErrorMessage } from '@/api/unwrap'
import { EncodeSettingsFields } from '@/components/encode-settings-fields'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/field'
import { formatDuration } from '@/lib/format'
import {
  buildRuleInput,
  emptyRuleMeta,
  hasNoConditions,
  ruleMetaError,
  ruleToMeta,
  type RuleMetaDraft,
  type SearchDraft,
} from '@/lib/program-search'
import {
  epgWindowDays,
  ruleCostWeekDays,
  type RuleCostEstimate,
} from '@/lib/rule-cost'
import { cn } from '@/lib/utils'

/**
 * RuleCostSummary は「この条件でルールを作成」「上書き保存」の近くに常置する値札
 * （issue #237）。ルールは保存した瞬間から録画（チューナー・ストレージ）を消費し
 * 続けるが、保存前に見えるのはマッチする番組リストだけで量としてのコストが無音
 * だった、という問題への対処。
 *
 * **値札は警告ではない。** しきい値で色を変えたり保存を止めたりしない --- 多いか
 * 少ないかの判断はユーザーのもの。文字色は他の情報表示と同じ `text-muted-foreground`
 * を使う（`--warning` は「条件ゼロ」「期間指定の恒久化」用、`--destructive` は
 * 「壊れた・取り返しがつかない」用で、どちらも意味が違うので流用しない。
 * docs/frontend/design.md「色は信号のみ」）。**GB 換算もやらない** ---
 * ビットレートの実測の出所が未決で、件数と時間は検索結果だけから導出でき
 * 未決に依存しないため、そこをこの値札のスコープの切れ目にしている。
 *
 * 「未検索」（`status === 'idle'`）と「検索したが 0 件」（`status === 'success'`
 * かつ `totalCount === 0`）を同じ文言にしない --- 両方とも件数が無い状態だが、
 * 条件を指定し忘れているだけなのか、条件が正しく絞り込めているのかは区別が要る
 * （`/search` の既存規律「未検索と 0 件を混同しない」と同じ精神）。
 *
 * 件数は `totalCount`（検索 API が返す全件、ページングなし）から厳密に出せる。
 * 時間は番組ごとの `durationMs` が要るため `loadedDurationsMs`（画面が結果表示の
 * ために読み込んだ分。`programId` 昇順の先頭 N 件で、無作為抽出ではない ---
 * `lib/rule-cost.ts` の `RuleCostSample` のコメントを参照）の平均から外挿する
 * 近似値になる。母数（`totalCount`）に対して読み込みが追いついていないときは
 * `estimateRuleCost` の `isSampled` を見て「先頭 N 件」であることを文言に足す
 * （黙って過小に見せない。かつ読み込みが 1 件も済んでいない間はこの注記を出さない
 * --- `estimate.durationMsPerWeek === undefined`（算出中）のときに
 * 「0 件の平均から算出」という自己矛盾した文言を出さないため）。
 *
 * `hasPeriod` が真（`periodStartAt` / `periodEndAt` で期間を絞った検索）のときは
 * 「8 日分を 7 日換算」という根拠を出さない --- その根拠は「検索結果は EPG の
 * 前方 8 日ぶんの観測」という前提に立っており、期間を絞った検索では観測スパンが
 * その期間そのものになるため前提が崩れる（8 日分ではないのに 8 日分と言うと偽の
 * 根拠になる。issue #237 の罠「黙って過小に見せない」に反する）。代わりに
 * 「期間条件で絞っているため、週あたりの見込みは実際より小さく出ます」と明記する。
 *
 * **`hasPeriod` は `estimate` と同じ検索の産物でなければならない**
 * （呼び出し側の `searchedHasPeriod` を参照）。数値と根拠の由来が食い違うと、
 * 消したはずの偽の根拠が「フォームを触っただけ」で復活する。
 *
 * `estimate` は呼び出し側が 1 回計算したものを受け取る（`ShortfallOverlapNote`
 * も同じものを使うので、ここで計算し直さない）。
 */
export function RuleCostSummary({
  status,
  estimate,
  hasPeriod,
}: {
  status: 'idle' | 'pending' | 'error' | 'success'
  estimate: RuleCostEstimate
  hasPeriod: boolean
}) {
  if (status === 'idle') {
    return (
      <p className="px-4 py-2 text-xs text-muted-foreground">
        検索すると、この条件で保存した場合の週あたりの見込み（件数・録画時間）が表示されます
      </p>
    )
  }
  if (status === 'pending') {
    return <p className="px-4 py-2 text-xs text-muted-foreground">見込みを計算中…</p>
  }
  if (status === 'error') {
    return (
      <p className="px-4 py-2 text-xs text-muted-foreground">
        検索が失敗したため見込みを表示できません
      </p>
    )
  }

  const countText = `約 ${Math.round(estimate.countPerWeek)} 件`
  const durationText =
    estimate.durationMsPerWeek === undefined
      ? '算出中…'
      : `約 ${formatDuration(estimate.durationMsPerWeek)}`
  const basisText = hasPeriod
    ? ''
    : `（現在の EPG 実測 ${estimate.totalCount} 件・${epgWindowDays} 日分を ${ruleCostWeekDays} 日換算）`
  const periodNote = hasPeriod
    ? '（期間条件で絞っているため、週あたりの見込みは実際より小さく出ます）'
    : ''
  const sampledNote =
    estimate.durationMsPerWeek !== undefined && estimate.isSampled
      ? `（時間は先頭 ${estimate.sampleSize} 件の平均から算出）`
      : ''
  const text =
    `この条件で保存すると、週あたり見込みで${countText}・${durationText}` +
    basisText +
    periodNote +
    sampledNote

  return <p className="px-4 py-2 text-xs text-muted-foreground">{text}</p>
}

/**
 * ShortfallOverlapNote は検索結果のうち放送時間帯が既存のチューナー不足区間と
 * 交差する番組の件数を値札の隣に出す（判定 (b)。docs/frontend/search.md
 * 「保存前の値札」）。**0 件のときは何も描画しない**（`CapacityShortfallBadge`
 * と同じ「沈黙は保証ではない」規律。緑にも「収まります」にもしない）。上限で
 * 切れているときは値札の他の注記と同じ形で「先頭 N 件のうち」と明記する。
 */
export function ShortfallOverlapNote({
  count,
  sampleSize,
  isSampled,
}: {
  count: number
  sampleSize: number
  isSampled: boolean
}) {
  if (count === 0) return null

  const scope = isSampled ? `先頭 ${sampleSize} 件のうち、` : ''
  return (
    <p className="px-4 py-2 text-xs text-muted-foreground">
      {scope}既にチューナー不足の区間と重なる番組が {count} 件あります
    </p>
  )
}

/**
 * RuleSourceBanner は `?ruleId=N` で開いたときの由来表示。
 *
 * 読み込み中・404・その他の失敗・成功を区別する。存在しない ruleId で
 * 無言の空白（フォームが何事もなく空のまま出る）にしないため、失敗も明示する。
 */
export function RuleSourceBanner({
  ruleId,
  rule,
  isPending,
  isError,
  error,
}: {
  ruleId: number
  rule: Rule | undefined
  isPending: boolean
  isError: boolean
  error: unknown
}) {
  if (isPending) {
    return (
      <div
        role="status"
        className="border-b border-border bg-muted/40 px-4 py-2 text-xs text-muted-foreground"
      >
        ルール #{ruleId} の条件を読み込み中…
      </div>
    )
  }

  if (isError) {
    // ApiError.status を見て 404 と他の失敗（ネットワーク断など）を区別する。
    // どちらも「無言の空白」にはしない
    const notFound = error instanceof ApiError && error.status === 404
    return (
      <div
        role="alert"
        className="border-b border-border bg-muted/40 px-4 py-2 text-xs text-destructive"
      >
        {notFound
          ? `ルール #${ruleId} が見つかりません（削除された可能性があります）`
          : (apiErrorMessage(error) ?? `ルール #${ruleId} の取得に失敗しました`)}
      </div>
    )
  }

  if (rule === undefined) return null

  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-muted/40 px-4 py-2 text-xs">
      <span>
        ルール「{rule.name}」の条件を編集中です。「ルールを上書き保存」で保存すると、このルール自体が書き換わります。元のルールを残したい場合は「別の新しいルールとして保存」を使ってください。
      </span>
      <Button type="button" variant="outline" size="sm" render={<Link to="/rules" />}>
        ルール一覧に戻る
      </Button>
    </div>
  )
}

/** CreateRuleSection は「この条件でルールを作成」の入口を表示する。 */
export function CreateRuleSection({
  draft,
  draftError: draftHasError,
}: {
  draft: SearchDraft
  draftError: string | undefined
}) {
  const [open, setOpen] = useState(false)

  if (!open) {
    return (
      <div className="border-b border-border px-4 py-4">
        <Button
          type="button"
          variant="outline"
          size="lg"
          className="w-full"
          disabled={draftHasError !== undefined}
          onClick={() => setOpen(true)}
        >
          この条件でルールを作成
        </Button>
      </div>
    )
  }

  return (
    <div className="border-b border-border px-4 py-4">
      <CreateRuleForm
        draft={draft}
        draftHasError={draftHasError !== undefined}
        onCancel={() => setOpen(false)}
        onDone={() => setOpen(false)}
      />
    </div>
  )
}

/**
 * CreateRuleForm は条件以外のメタ情報（名前・有効・優先度・エンコード設定）を
 * 入力して `POST /api/rules` に送る。`?ruleId` を伴わない通常の検索からのみ
 * 使われる（既存ルールのフォークは `RuleEditSection` の「別の新しいルールとして
 * 保存」に一本化した）ので、`preserve` は渡さない — UI を持たない項目
 * （`description` 等）を推測して埋めることはしない。
 */
export function CreateRuleForm({
  draft,
  draftHasError,
  onCancel,
  onDone,
}: {
  draft: SearchDraft
  draftHasError: boolean
  onCancel: () => void
  onDone: () => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const createRule = useCreateRule()

  const [meta, setMeta] = useState<RuleMetaDraft>(emptyRuleMeta)
  const [confirmedEmpty, setConfirmedEmpty] = useState(false)

  const metaError = ruleMetaError(meta)
  const noConditions = hasNoConditions(draft)
  const hasPeriod = draft.periodStartAt !== '' || draft.periodEndAt !== ''
  const pending = createRule.isPending
  const blocked =
    draftHasError || metaError !== undefined || (noConditions && !confirmedEmpty) || pending

  const save = () => {
    if (blocked) return
    const input = buildRuleInput(draft, meta)
    createRule.mutate(
      { data: input },
      {
        onSuccess: () => {
          toast({ message: 'ルールを作成しました' })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
          onDone()
          void navigate({ to: '/rules' })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  return (
    <form
      aria-label="この条件でルールを作成"
      className="flex flex-col gap-4 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
      {hasPeriod && (
        <p className="text-xs text-warning">
          期間を指定したまま作成すると、ルールの恒久的な期間制限になります。「いまだけ絞り込みたい」
          場合は、上の条件フォームで期間を空にしてから作成してください。
        </p>
      )}

      {noConditions && (
        <div className="flex flex-col gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5">
          <p role="alert" className="text-xs text-destructive">
            条件を 1 つも指定していません。このまま作成すると、放送されるすべての番組が対象になります。
          </p>
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              type="checkbox"
              className="size-4 accent-primary"
              checked={confirmedEmpty}
              disabled={pending}
              onChange={(e) => setConfirmedEmpty(e.target.checked)}
            />
            すべての番組が対象になることを理解した上で作成します
          </label>
        </div>
      )}

      <Field label="名前">
        <Input
          value={meta.name}
          disabled={pending}
          onChange={(e) => setMeta((m) => ({ ...m, name: e.target.value }))}
          placeholder="例: ニュース全部"
          required
        />
      </Field>

      <div className="flex flex-wrap items-center gap-4">
        <label
          className={cn(
            'flex items-center gap-2 text-sm',
            pending && 'pointer-events-none opacity-50',
          )}
        >
          <input
            type="checkbox"
            className="size-4 accent-primary"
            checked={meta.enabled}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, enabled: e.target.checked }))}
          />
          有効
        </label>
        <Field label="優先度" className="w-28">
          <Input
            type="number"
            min={0}
            value={meta.priority}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, priority: e.target.value }))}
          />
        </Field>
      </div>

      <EncodeSettingsFields
        value={{ keepOriginal: meta.keepOriginal, encodeProfiles: meta.encodeProfiles }}
        onChange={(next) => setMeta((m) => ({ ...m, ...next }))}
        disabled={pending}
      />

      {metaError !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {metaError}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="lg" disabled={blocked}>
          {pending ? '作成中…' : 'ルールを作成'}
        </Button>
        <Button type="button" variant="outline" size="lg" disabled={pending} onClick={onCancel}>
          キャンセル
        </Button>
      </div>
    </form>
  )
}

/**
 * RuleEditSection は `?ruleId=N` で開いたときの保存 UI。
 *
 * `CreateRuleSection` と違って折りたたまない —— ユーザーは「試している」の
 * ではなく、既にあるルールを編集する目的でこの画面を開いている（マッチする
 * 番組を見ながら条件を詰められるこの画面が実質のルール編集画面になる、という
 * 判断）。`key={sourceRule.id}` を親（`pages/search.tsx`）で付けているので、
 * `ruleId` を切り替えて別のルールを開き直したときはこのコンポーネントごと
 * 作り直され、下の `RuleEditForm` の `meta` / `confirmedEmpty` が古いルールの
 * 値のまま残らない。
 */
export function RuleEditSection({
  draft,
  draftError: draftHasError,
  sourceRule,
}: {
  draft: SearchDraft
  draftError: string | undefined
  sourceRule: Rule
}) {
  return (
    <div className="border-b border-border px-4 py-4">
      <RuleEditForm
        draft={draft}
        draftHasError={draftHasError !== undefined}
        sourceRule={sourceRule}
      />
    </div>
  )
}

/** RuleEditForm は `?ruleId=N` で開いたルールを上書き、または複製保存する。 */
export function RuleEditForm({
  draft,
  draftHasError,
  sourceRule,
}: {
  draft: SearchDraft
  draftHasError: boolean
  sourceRule: Rule
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const updateRule = useUpdateRule()
  const createRule = useCreateRule()

  const [meta, setMeta] = useState<RuleMetaDraft>(() => ruleToMeta(sourceRule))
  const [confirmedEmpty, setConfirmedEmpty] = useState(false)

  const metaError = ruleMetaError(meta)
  const noConditions = hasNoConditions(draft)
  const hasPeriod = draft.periodStartAt !== '' || draft.periodEndAt !== ''
  const pending = updateRule.isPending || createRule.isPending
  const blocked =
    draftHasError || metaError !== undefined || (noConditions && !confirmedEmpty) || pending

  const overwrite = () => {
    if (blocked) return
    // preserve に sourceRule を渡す。渡し忘れると UI を持たない項目
    // （description / dedupe* / filenameTemplate / metadata）が
    // `UpdateRule` の子テーブル全置換で黙って消える。
    const input = buildRuleInput(draft, meta, sourceRule)
    updateRule.mutate(
      { id: sourceRule.id, data: input },
      {
        onSuccess: () => {
          toast({ message: `ルール「${meta.name.trim()}」を上書き保存しました` })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
          void queryClient.invalidateQueries({ queryKey: getGetRuleQueryKey(sourceRule.id) })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  const saveAsNew = () => {
    if (blocked) return
    // `rules.name` に一意制約は無い（rules テーブル定義）ので、名前を
    // そのまま引き継ぐと一覧に同名の 2 本が並び、条件の要約でしか見分けられ
    // なくなる。押した時点で名前が元のルールと同じままなら `〜 のコピー` を
    // 付ける。名前欄そのものは書き換えない（上書き保存に戻ったときに元の名前
    // のままであってほしいため）。ユーザーが既に名前を変えているなら、
    // その意図（別の名前を選んだ）を尊重してそのまま使う。
    const trimmed = meta.name.trim()
    const name = trimmed === sourceRule.name.trim() ? `${trimmed} のコピー` : trimmed
    // `draft.sites` は `POST /api/rules` に載る。API の「保存済み site 名は
    // レジストリ照合を免除する」はルール単位で PATCH にしか効かないので、
    // レジストリから消えた site を含んだまま POST するとこの経路だけ 400
    // `unknown site` になりうる --- ただし `sites` は issue #531 で条件 UI の
    // 次元になったため、`<ConditionFields>` のサイトチップ（レジストリと下書きの
    // 和集合を選択肢にする）でユーザーが画面内で外せる。落として送るのは禁止
    // —— 絞り込みが無音で全サイトに反転する。
    const input = buildRuleInput(draft, { ...meta, name }, sourceRule)
    createRule.mutate(
      { data: input },
      {
        onSuccess: () => {
          toast({ message: `「${name}」として新しいルールを保存しました` })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  return (
    <form
      aria-label="ルールの条件を編集"
      className="flex flex-col gap-4 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        overwrite()
      }}
    >
      {hasPeriod && (
        <p className="text-xs text-warning">
          期間を指定したまま保存すると、ルールの恒久的な期間制限になります。「いまだけ絞り込みたい」
          場合は、上の条件フォームで期間を空にしてから保存してください。
        </p>
      )}

      {noConditions && (
        <div className="flex flex-col gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5">
          <p role="alert" className="text-xs text-destructive">
            条件を 1 つも指定していません。このまま保存すると、放送されるすべての番組が対象になります。
          </p>
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              type="checkbox"
              className="size-4 accent-primary"
              checked={confirmedEmpty}
              disabled={pending}
              onChange={(e) => setConfirmedEmpty(e.target.checked)}
            />
            すべての番組が対象になることを理解した上で保存します
          </label>
        </div>
      )}

      <Field label="名前">
        <Input
          value={meta.name}
          disabled={pending}
          onChange={(e) => setMeta((m) => ({ ...m, name: e.target.value }))}
          placeholder="例: ニュース全部"
          required
        />
      </Field>

      <div className="flex flex-wrap items-center gap-4">
        <label
          className={cn(
            'flex items-center gap-2 text-sm',
            pending && 'pointer-events-none opacity-50',
          )}
        >
          <input
            type="checkbox"
            className="size-4 accent-primary"
            checked={meta.enabled}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, enabled: e.target.checked }))}
          />
          有効
        </label>
        <Field label="優先度" className="w-28">
          <Input
            type="number"
            min={0}
            value={meta.priority}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, priority: e.target.value }))}
          />
        </Field>
      </div>

      <EncodeSettingsFields
        value={{ keepOriginal: meta.keepOriginal, encodeProfiles: meta.encodeProfiles }}
        onChange={(next) => setMeta((m) => ({ ...m, ...next }))}
        disabled={pending}
      />

      {metaError !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {metaError}
        </p>
      )}

      <div className="flex flex-col gap-2 border-t border-border pt-3">
        <div className="flex flex-wrap gap-2">
          <Button type="submit" size="lg" disabled={blocked}>
            {updateRule.isPending ? '上書き保存中…' : 'ルールを上書き保存'}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="lg"
            disabled={blocked}
            onClick={saveAsNew}
          >
            {createRule.isPending ? '保存中…' : '別の新しいルールとして保存'}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          「ルールを上書き保存」はルール「{sourceRule.name}」自体を書き換えます。元のルールを
          残したまま試したい場合は「別の新しいルールとして保存」を使ってください（元のルールは
          変更されません）。
        </p>
      </div>
    </form>
  )
}
