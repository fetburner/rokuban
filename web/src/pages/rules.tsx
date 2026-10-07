import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ChevronRight, MoreVertical, Plus, Power, Trash2 } from 'lucide-react'
import { useMemo, useRef, useState } from 'react'

import {
  getGetRuleQueryKey,
  getGetRuleQueryOptions,
  getListReservationsQueryKey,
  getListReservationsQueryOptions,
  getListRecordingsQueryOptions,
  getListRulesQueryKey,
  useDeleteRule,
  useListCapacityOverages,
  useListReservations,
  useListRules,
  useUpdateRule,
  type DeleteRuleResponse,
  type LabelRule,
  type ListRulesQueryResult,
  type Rule,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { LabelRuleForm } from '@/components/label-rule-form'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { summarizeRuleConditions } from '@/components/rule-condition-summary'
import { useToast } from '@/components/toaster'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { keepOriginalLabel, type KeepOriginal } from '@/lib/encode-settings'
import { coveringWindow } from '@/lib/capacity'
import {
  buildRuleInput,
  conditionsToDraft,
  ruleToMeta,
} from '@/lib/program-search'
import { summarizeRuleActivity, type RuleActivitySummary } from '@/lib/rule-activity'
import { ruleDisambiguator } from '@/lib/rule-label'
import { useMediaQuery } from '@/lib/use-media-query'
import { auxActionClassName, cn } from '@/lib/utils'

/**
 * RulesPage は録画ルールの一覧・有効切替・削除を扱う。
 * 新規作成の入口は `/search` に一本化する。値札（件数・時間の見込み）と一致する番組の
 * 一覧が常に作成ボタンの近くに出る検索画面に入口を揃える。`/rules` の作成フォームには
 * そのどちらも無かった。強制はしておらず、`/search` でも検索せずに保存まで進める。
 *
 * 既存ルールの上書きは各行のルール名から `/search?ruleId=N` を開く導線に
 * 一本化する。検索側は条件に一致する番組を見ながら編集でき、UI を持たない項目も
 * `buildRuleInput` の `preserve` 引数で引き継ぐ（`RuleEditForm` の doc comment と
 * `docs/frontend/search.md` を参照）。
 *
 * `rules.name` は一意ではないため、一覧で同名のルールが並ぶときだけ `#<id>` を
 * 名前に添えて押し分ける。単独の名前には補助ラベルを付けず、通常時の一覧を短く
 * 保つ。
 */
export function RulesPage() {
  const query = useListRules()
  const rules = useMemo(() => unwrap(query.data) ?? [], [query.data])
  const reservationsQuery = useListReservations()
  // 一覧の予約が成功したときだけ件数を断定する。失敗時に前回 data が残っていても
  // 「録画予定なし」と誤読させない。
  const reservations = useMemo(
    () =>
      reservationsQuery.isSuccess
        ? (unwrap(reservationsQuery.data) ?? [])
        : undefined,
    [reservationsQuery.data, reservationsQuery.isSuccess],
  )
  const listedWindow = useMemo(
    () => (reservations === undefined ? null : coveringWindow(reservations)),
    [reservations],
  )
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(listedWindow?.startMs ?? 0).toISOString(),
      end: new Date(listedWindow?.endMs ?? 0).toISOString(),
    },
    { query: { enabled: listedWindow !== null } },
  )
  // 古い成功 data が再取得失敗後にも残る場合があるため、成功中だけ使う。
  const overages = useMemo(
    () =>
      overagesQuery.isSuccess
        ? (unwrap(overagesQuery.data) ?? [])
        : undefined,
    [overagesQuery.data, overagesQuery.isSuccess],
  )
  const activityByRule = useMemo(() => {
    if (reservations === undefined) return undefined
    return new Map<number, RuleActivitySummary>(
      rules.map((rule) => [
        rule.id,
        summarizeRuleActivity(rule.id, reservations, overages),
      ] as const),
    )
  }, [overages, reservations, rules])
  const disambiguateRule = ruleDisambiguator(rules)
  const [isCountingReservations, setIsCountingReservations] = useState(false)
  const [labelRuleEditor, setLabelRuleEditor] = useState<{
    rule?: LabelRule
    initial?: { keyword?: string; value?: string }
  }>()

  return (
    <>
      <PageHeader
        title="ルール"
        actions={
          <Button size="lg" className="hidden lg:inline-flex" render={<Link to="/search" />}>
            ルールを作成
          </Button>
        }
      />

      <PageContent className="flex flex-col gap-4 px-4 py-4">
        <Button size="lg" className="w-full lg:hidden" render={<Link to="/search" />}>
          ルールを作成
        </Button>

        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()}>ルールの取得に失敗しました</ErrorState>
        ) : query.isPending ? (
          <ListSkeleton />
        ) : rules.length === 0 ? (
          <EmptyState>ルールがありません</EmptyState>
        ) : (
          <ul className="flex flex-col gap-3">
            {rules.map((rule) => (
              <li key={rule.id}>
                <RuleRow
                  rule={rule}
                  disambiguate={disambiguateRule}
                  activity={activityByRule?.get(rule.id)}
                  isCountingReservations={isCountingReservations}
                  onCountingReservationsChange={setIsCountingReservations}
                  onCreateLabelRule={(keyword) =>
                    setLabelRuleEditor({ initial: { keyword } })
                  }
                />
              </li>
            ))}
          </ul>
        )}

      </PageContent>

      {labelRuleEditor !== undefined && (
        <LabelRuleForm
          open
          rule={labelRuleEditor.rule}
          initial={labelRuleEditor.initial}
          onOpenChange={(open) => {
            if (!open) setLabelRuleEditor(undefined)
          }}
        />
      )}
    </>
  )
}

/**
 * deleteRuleWarning は削除確認ダイアログの説明文を組み立てる。
 *
 * 重複排除を有効にしたルールでは、削除で**履歴が比較のスコープから外れる**
 * ことを事前に伝える。`recordings.rule_id` はルール削除で NULL に落ち、同じ
 * 条件で作り直しても新しい id になるので過去の録画は 1 件もマッチしない
 * （`docs/recording/ruler.md` §3.1「ルールの削除は履歴のスコープを消す」）。
 * 帰結は録り逃しではないが、押した後に取り消せる操作でもないので、条件を
 * 変えたいだけなら削除ではなくルール名からの編集（id を保つ上書き保存）を
 * 使えることまで書く。
 *
 * **被害の大きさを docs より強く書かない。** 過剰録画は一過性で、新しい
 * ルールの下で 1 本録れれば以降の再放送はまた弾かれる（`internal/ruler`
 * の `TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete` 段階 3 で測っている）。
 * 「窓の中の再放送を録り直す」と書くと UI だけ被害が大きく読める。
 *
 * 件数は出さない。削除前に「何件の録画がスコープから外れるか」を数える API は
 * 無いうえ、件数は行動を変えない（引き継がれないという質の情報で足りる）。
 *
 * 見出し（「ルール『NAME』を削除しますか？」）は `AlertDialogTitle` 側が持つ
 * ので、ここは本文（`AlertDialogDescription`）だけを返す。
 */
function deleteRuleWarning(rule: Rule): string {
  const base = 'ルールの設定を削除します。取り消せません。'
  if (!rule.dedupeEnabled) return base
  return (
    `${base}このルールの重複排除の履歴も一緒に外れます。同じ条件で作り直しても引き継がれないので、` +
    '次の再放送を録り直します（新しいルールで 1 本録れれば以降はまた弾かれます）。' +
    '条件を変えたいだけならルール名から編集して上書きしてください。'
  )
}

/**
 * deleteRuleResultMessage は削除後のトーストの文言を組み立てる。`undefined` は
 * 「言うことが無いので出さない」を意味する（呼び出し側はそのときトースト
 * 自体を出さない）。
 *
 * RulesPage はフィルタもページングも持たない一覧なので、削除された行が
 * 一覧から消えることそのものは常に画面に見える（issue #297）。**素の
 * 「ルールを削除しました」はこの可視な効果の重複でしかないので無音化する。**
 * 一方、削除 API が返す内訳（削除した予約 / 編集済みのため残った予約）は
 * RulesPage のどこにも出ない別の事実 --- 予約は /recordings 側にしかなく
 * （`docs/recording/reservation-model.md` §4.3「ルール削除の UX は可視化で
 * 解決する」）、ここで言わなければ利用者には見えない。内訳が両方 0 件
 * （＝言うことが無い）のときだけ無音にし、どちらかが 1 件以上あるときは
 * 残す。残った予約は定義上「ユーザーが自分で触ったもの」だけなので、
 * 件数は常に少なく 1 件ずつ説明できる。
 *
 * **Undo にはしない。** 削除確認ダイアログ（`deleteRuleWarning`）が既に
 * 「取り消せません」と明言しており、実際 dedupe 有効なルールは削除で
 * 履歴のスコープが失われる（同じ条件で作り直しても新しい id になり
 * 引き継がれない）。Undo ボタンで「作り直す」を提供すると、この非可逆性を
 * 覆すかのような期待を持たせてしまう。
 *
 * 応答が読めなかった場合（`unwrap` が undefined）は内訳が分からず「言う
 * ことが無い」と断定できないので、素の文言に落とす —— 削除自体は
 * 成功しているので、そこで黙るのは間違い。
 */
function deleteRuleResultMessage(res: DeleteRuleResponse | undefined): string | undefined {
  if (!res) return 'ルールを削除しました'
  if (res.deletedReservations === 0 && res.detachedReservations === 0) {
    return undefined
  }
  if (res.detachedReservations > 0) {
    return `ルールを削除しました（予約 ${res.deletedReservations} 件を削除、${res.detachedReservations} 件は編集済みのため残しました）`
  }
  return `ルールを削除しました（予約 ${res.deletedReservations} 件を削除）`
}

/**
 * RuleRow は一覧の 1 行。
 *
 * ルール名は `/search?ruleId=N` への主要な編集リンクにする。検索側では条件に
 * 一致する番組を見ながら既存ルールを上書きでき、ルール名という固有名詞自体を
 * リンクにすることで一覧を読んでいる位置から編集へ移れる。
 * **右列に同じ編集ボタンは置かない。** 編集の入口を名前に一本化し、右列には
 * 名前だけでは表せない有効切替・録画一覧・その他メニューを残す。これで同じ
 * `/search?ruleId=N` への導線をカード内に二重化しない。
 * 削除（稀・破壊的）だけを overflow メニューに寄せる（issue #227）。
 * 「無効」バッジと有効スイッチは意図的に併存させる。バッジは一覧を読み流す
 * ときの状態表示、スイッチは操作対象であり、片方だけではもう片方の役割を
 * 満たさない。
 */
function RuleRow({
  rule,
  disambiguate,
  activity,
  isCountingReservations,
  onCountingReservationsChange,
  onCreateLabelRule,
}: {
  rule: Rule
  disambiguate: (rule: Rule) => string | undefined
  activity: RuleActivitySummary | undefined
  isCountingReservations: boolean
  onCountingReservationsChange: (counting: boolean) => void
  onCreateLabelRule: (keyword: string) => void
}) {
  const finePointer = useMediaQuery('(pointer: fine)')
  const rowRef = useRef<HTMLDivElement>(null)
  const profiles = rule.encodeProfiles ?? []
  const keep = (rule.keepOriginal ?? 'always') as KeepOriginal
  const conditions = summarizeRuleConditions(rule)
  const disambiguator = disambiguate(rule)
  const displayName = disambiguator === undefined ? rule.name : `${rule.name} (${disambiguator})`
  const toast = useToast()
  const queryClient = useQueryClient()
  const updateRule = useUpdateRule()
  const deleteRule = useDeleteRule()
  const [activeReservationCount, setActiveReservationCount] = useState(0)
  const [activeRecordingCount, setActiveRecordingCount] = useState(0)
  const [disableConfirmOpen, setDisableConfirmOpen] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)

  const setEnabled = (
    enabled: boolean,
    afterSuccess?: () => void,
    refreshRule = false,
  ) => {
    const key = getListRulesQueryKey()
    void (async () => {
      let currentRule =
        queryClient
          .getQueryData<ListRulesQueryResult>(key)
          ?.data.find((item) => item.id === rule.id) ?? rule
      let previousEnabled: boolean | undefined
      let optimisticUpdate = false

      try {
        if (refreshRule) {
          // Undo は画面遷移後に使える。別画面で編集されていた場合も全置換 PATCH
          // で古い条件を戻さないよう、クリック時にサーバーの最新ルールを読む。
          const response = await queryClient.fetchQuery(
            getGetRuleQueryOptions(rule.id, { query: { staleTime: 0 } }),
          )
          const latestRule = unwrap(response)
          if (latestRule === undefined) throw new Error('ルールの最新状態を取得できませんでした')
          currentRule = latestRule
        }

        previousEnabled = currentRule.enabled
        queryClient.setQueryData<ListRulesQueryResult>(key, (current) =>
          current === undefined
            ? current
            : {
                ...current,
                data: current.data.map((item) =>
                  item.id === currentRule.id ? { ...currentRule, enabled } : item,
                ),
              },
        )
        optimisticUpdate = true

        // toast の Undo は行コンポーネントが外れた後にも押せる。アンマウント時に
        // per-call callback が失われないことは「画面遷移後の Undo 失敗も通知して
        // キャッシュを無効状態へ戻す」で確認する。
        await updateRule.mutateAsync({
          id: currentRule.id,
          data: buildRuleInput(
            conditionsToDraft(currentRule),
            { ...ruleToMeta(currentRule), enabled },
            // preserve を落とすと UI を持たない項目（description / dedupe* /
            // filenameTemplate / metadata）が UpdateRule の全置換で黙って消える。
            currentRule,
          ),
        })
        void queryClient.invalidateQueries({ queryKey: key })
        void queryClient.invalidateQueries({
          queryKey: getGetRuleQueryKey(currentRule.id),
        })
        // ruler はレベルトリガーなので、再取得しても次回評価までは予約が残る。
        void queryClient.invalidateQueries({ queryKey: getListReservationsQueryKey() })
        afterSuccess?.()
      } catch (err) {
        if (optimisticUpdate && previousEnabled !== undefined) {
          const rollbackEnabled = previousEnabled
          queryClient.setQueryData<ListRulesQueryResult>(key, (current) =>
            current === undefined
              ? current
              : {
                  ...current,
                  data: current.data.map((item) =>
                    item.id === currentRule.id
                      ? { ...item, enabled: rollbackEnabled }
                      : item,
                  ),
                },
          )
        }
        toast({
          message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました',
          kind: 'error',
        })
      }
    })()
  }

  const toggleEnabled = async () => {
    if (!rule.enabled) {
      setEnabled(true)
      return
    }

    if (isCountingReservations) return
    onCountingReservationsChange(true)
    try {
      const countActiveRecordings = async () => {
        const pageLimit = 200
        let count = 0
        let before: string | undefined
        let beforeId: number | undefined
        while (true) {
          const response = await queryClient.fetchQuery(
            getListRecordingsQueryOptions({
              status: 'recording',
              ruleId: rule.id,
              source: 'rule',
              limit: pageLimit,
              before,
              beforeId,
            }, { query: { staleTime: 0 } }),
          )
          const page = unwrap(response) ?? []
          count += page.length
          if (page.length < pageLimit) return count
          const last = page[page.length - 1]
          before = last.startAt
          beforeId = last.id
        }
      }

      const [reservationResponse, recordingCount] = await Promise.all([
        queryClient.fetchQuery(getListReservationsQueryOptions()),
        countActiveRecordings(),
      ])
      const reservationCount = (unwrap(reservationResponse) ?? []).filter(
        (reservation) =>
          reservation.ruleId === rule.id &&
          reservation.source === 'rule' &&
          reservation.state === 'active',
      ).length
      setActiveReservationCount(reservationCount)
      setActiveRecordingCount(recordingCount)
      if (recordingCount > 0) {
        setDisableConfirmOpen(true)
      } else {
        setEnabled(false, () => {
          toast({
            message: `ルール「${displayName}」を無効にしました。予約 ${reservationCount} 件が取り消されます`,
            actions: [{ label: '元に戻す', onClick: () => setEnabled(true, undefined, true) }],
          })
        })
      }
    } catch (err) {
      toast({
        message: apiErrorMessage(err) ?? '予約・録画状況の取得に失敗しました',
        kind: 'error',
      })
    } finally {
      onCountingReservationsChange(false)
    }
  }

  const remove = () => {
    // ダイアログは AlertDialogAction（AlertDialogPrimitive.Close ラップ）が
    // クリックで自動的に閉じるので、ここでは実行の確定のみ行う。
    deleteRule.mutate(
      { id: rule.id },
      {
        onSuccess: (res) => {
          const message = deleteRuleResultMessage(unwrap(res))
          if (message !== undefined) toast({ message })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの削除に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  const row = (
    <div
      ref={rowRef}
      className="rounded-lg border border-border px-3 py-3"
      tabIndex={finePointer ? -1 : undefined}
    >
      <div className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-2 md:gap-y-1">
        <div className="col-start-1 row-start-1 min-w-0">
          {/* flex-nowrap: 「無効」バッジは常に名前と同じ行に残す（旧・素の
              truncate span の挙動を維持）。flex-wrap のままだと、長い名前の
              hypothetical な主軸サイズ（flex-wrap の折返し判定は shrink 適用前の
              値を見る）だけで行いっぱいになり、shrink を足してもバッジは次行へ
              折り返る --- 実ブラウザで確認済み（同じ min-w-0 shrink のまま
              flex-wrap → flex-nowrap にした変更だけで折返りが消えた）。 */}
          <div className="flex flex-nowrap items-center gap-2">
            <Button
              variant="link"
              size="sm"
              // min-w-0 shrink: 共通 Button の base クラスが shrink-0 を持つため
              // 上書きしないと 0 まで縮まない。nowrap の行内でバッジ分の幅を
              // 譲るには、この Button 自身が縮み、中の truncate span が
              // テキストを省略できる必要がある。
              className="min-w-0 shrink justify-start overflow-hidden px-0 text-left text-base font-medium"
              render={
                <Link
                  to="/search"
                  search={{ ruleId: rule.id }}
                  aria-label={`ルール「${displayName}」を編集`}
                />
              }
            >
              <span className="truncate">{displayName}</span>
            </Button>
            {!rule.enabled && (
              /* shrink-0: nowrap 化した行の中で、名前に幅を譲って自分は潰れない
                 （文字色は text-foreground。bg-muted 小バッジの合成後コントラスト
                 対策。docs/frontend/design.md「コントラストは毎回測る」）。 */
              <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
                無効
              </span>
            )}
          </div>
        </div>

        <div className="col-span-2 row-start-2 min-w-0 md:col-start-1 md:col-span-1 md:row-start-2">
          {/* 条件が空 = 全番組にマッチする、という危険な状態を一覧でも
              見えるようにする（設定を開かないと気付けない事故を防ぐ）。 */}
          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-sm">
            {conditions.length === 0 ? (
              <span className="font-medium text-warning">
                条件なし（すべての番組にマッチ）
              </span>
            ) : (
              conditions.map((c, i) => (
                <span key={i} className="text-muted-foreground">
                  {c}
                </span>
              ))
            )}
          </div>

          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-sm text-muted-foreground pointer-coarse:pb-1">
            <span>優先度 {rule.priority}</span>
            <span>{keepOriginalLabel(keep)}</span>
            <span>
              {profiles.length === 0 ? 'エンコードなし' : profiles.join(', ')}
            </span>
            {rule.enabled && activity !== undefined && (
              activity.scheduledCount === 0 ? (
                <span className="text-muted-foreground">録画予定なし</span>
              ) : (
                <span>
                  <Link
                    to="/reservations"
                    search={{ ruleId: rule.id }}
                    // 当たり判定の 44px は箱ではなく疑似要素で取る。箱ごと 44px にすると
                    // 同じ行の文字より下がり、補助操作の段との間も空く。
                    className="relative inline-flex min-h-6 pointer-coarse:min-h-0 items-center text-primary underline-offset-4 hover:underline pointer-coarse:before:absolute pointer-coarse:before:inset-x-0 pointer-coarse:before:top-1/2 pointer-coarse:before:h-11 pointer-coarse:before:-translate-y-1/2"
                  >
                    録画予定 {activity.scheduledCount} 件
                  </Link>
                  {activity.shortfallCount !== undefined && activity.shortfallCount > 0 && (
                    <span className="text-warning">
                      （うち不足時間帯 {activity.shortfallCount}）
                    </span>
                  )}
                </span>
              )
            )}
          </div>
        </div>
        <div className="col-start-2 row-start-1 flex shrink-0 items-center gap-1 md:justify-self-end">
          <button
            type="button"
            role="switch"
            aria-checked={rule.enabled}
            aria-label={`ルール「${displayName}」を有効にする`}
            disabled={updateRule.isPending || isCountingReservations}
            className="inline-flex min-h-8 min-w-8 pointer-coarse:min-h-11 pointer-coarse:min-w-11 items-center rounded-full px-1 outline-none disabled:opacity-50 focus-visible:ring-3 focus-visible:ring-ring/50"
            onClick={() => void toggleEnabled()}
          >
            <span
              className={cn(
                'relative h-5 w-9 rounded-full transition-colors',
                rule.enabled ? 'bg-primary' : 'bg-muted-foreground',
              )}
            >
              <span
                className={cn(
                  'absolute top-0.5 left-0.5 size-4 rounded-full bg-background transition-transform',
                  rule.enabled && 'translate-x-4',
                )}
              />
            </span>
          </button>
          {/* 破壊的・稀な操作（削除）は overflow に寄せる（issue #227）。 */}
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={`ルール「${displayName}」のその他の操作`}
                />
              }
            >
              <MoreVertical />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem
                variant="destructive"
                disabled={deleteRule.isPending}
                onClick={() => setConfirmOpen(true)}
              >
                <Trash2 />
                削除
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>

        <div className="col-span-2 row-start-3 flex w-full flex-wrap gap-x-2 gap-y-1 md:col-start-2 md:col-span-1 md:row-start-2 md:row-span-2 md:w-full md:flex-col md:items-end md:pr-9">
          {/* このルール由来の録画だけに絞った /recordings への導線（issue #137）。
              条件モデルは検索（ProgramSearchRequest）と共有しないので、遷移先は
              /search ではなく /recordings?ruleId=N になる。 */}
          <Link
            to="/recordings"
            search={{ ruleId: rule.id }}
            className={cn(auxActionClassName, 'min-h-8 text-sm')}
          >
            <span>このルールの録画</span>
            <ChevronRight aria-hidden="true" className="size-4 shrink-0" />
          </Link>
          {/* 録画ルールのキーワードを分類ルール（シリーズ）へ写す導線。
              録画ルールから分類ルールを**継承はさせない**（勝者ルールだけを
              継承すると広いルールに黙って負ける。docs/data/series.md §8
              「評価結果を宛先にしない」）。このボタンは同じ /rules の
              ダイアログを開くだけで、保存は利用者が明示する。 */}
          {firstKeyword(rule) !== undefined && (
            <button
              type="button"
              className={cn(auxActionClassName, 'min-h-8 text-sm')}
              onClick={() => onCreateLabelRule(firstKeyword(rule) as string)}
            >
              <Plus aria-hidden="true" className="size-4 shrink-0" />
              <span>このキーワードで分類ルールを作る</span>
            </button>
          )}
        </div>
      </div>

      <AlertDialog open={disableConfirmOpen} onOpenChange={setDisableConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ルール「{displayName}」を無効にしますか？</AlertDialogTitle>
            <AlertDialogDescription>
              {`「${displayName}」を無効にすると、このルールによる予約 ${activeReservationCount} 件が取り消されます。手動で予約したものは残ります。録画中の ${activeRecordingCount} 件は録画が止まります。`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>キャンセル</AlertDialogCancel>
            <AlertDialogAction onClick={() => setEnabled(false)}>無効にする</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* AlertDialogTrigger ではなく open を直接制御する: トリガーは
          overflow メニューの中の menuitem であり、選択するとメニュー自体は
          閉じる。ダイアログの開閉をメニューの寿命に結び付けず、ここで
          独立に持つ（issue #295: ルール削除の確認を他の破壊的操作と同じ
          AlertDialog に揃える）。 */}
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ルール「{displayName}」を削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>{deleteRuleWarning(rule)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>キャンセル</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={remove}>
              削除する
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )

  if (!finePointer) return row

  return (
    <ContextMenu>
      <ContextMenuTrigger render={row} />
      <ContextMenuContent returnFocusRef={rowRef}>
        <ContextMenuItem
          disabled={updateRule.isPending || isCountingReservations}
          onClick={() => void toggleEnabled()}
        >
          <Power />
          {rule.enabled ? '無効にする' : '有効にする'}
        </ContextMenuItem>
        <ContextMenuItem
          variant="destructive"
          disabled={deleteRule.isPending}
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2 />
          削除
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}

/**
 * firstKeyword はルールの最初のキーワード条件の値を返す（無ければ undefined）。
 *
 * 分類ルールが持てる条件はキーワード 1 つだけなので、複数のキーワードを持つ
 * ルールでも写せるのは 1 つである。**最初の 1 つを選ぶのは任意**で、正規表現
 * モードの条件は写さない（分類ルールの方言は LIKE の部分一致だけで、正規表現を
 * 渡すと「検索では出るのに分類ルールが当たらない」になる）。
 */
function firstKeyword(rule: Rule): string | undefined {
  for (const match of rule.textMatches ?? []) {
    if (match.mode === 'keyword' && match.value !== '') return match.value
  }
  return undefined
}
