import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { MoreVertical, Pencil, Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'

import {
  getListReservationsQueryKey,
  getListReservationsQueryOptions,
  getListLabelRulesQueryKey,
  getListRecordingShelvesQueryKey,
  getListRulesQueryKey,
  useDeleteLabelRule,
  useDeleteRule,
  useListLabelRules,
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
import { Button, buttonVariants } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { keepOriginalLabel, type KeepOriginal } from '@/lib/encode-settings'
import {
  buildRuleInput,
  conditionsToDraft,
  ruleToMeta,
} from '@/lib/program-search'
import { ruleDisambiguator } from '@/lib/rule-label'
import { cn } from '@/lib/utils'

/**
 * RulesPage は録画ルールの一覧・有効切替・削除を扱う。
 * 新規作成の入口は `/search` に一本化する。検索結果の値札（件数・時間の見込み）を
 * 確認してから保存する流れに揃え、`/rules` から値札を素通りして作成できないようにする。
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
  const labelRulesQuery = useListLabelRules()
  const rules = unwrap(query.data) ?? []
  const labelRules = unwrap(labelRulesQuery.data) ?? []
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
          <Link
            to="/search"
            className={buttonVariants({ size: 'lg', className: 'hidden lg:inline-flex' })}
          >
            ルールを作成
          </Link>
        }
      />

      <PageContent className="flex flex-col gap-4 px-4 py-4">
        <Link
          to="/search"
          className={buttonVariants({ size: 'lg', className: 'w-full lg:hidden' })}
        >
          ルールを作成
        </Link>

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

        <section className="flex flex-col gap-3 border-t border-border pt-5">
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <h2 className="text-sm font-medium text-foreground">シリーズ分類</h2>
              <p className="mt-1 text-xs text-muted-foreground">
                録画タイトルのキーワードに当たった録画を、指定したシリーズキーへ分類します。
              </p>
            </div>
            <Button
              type="button"
              size="sm"
              className="shrink-0"
              onClick={() => setLabelRuleEditor({})}
            >
              <Plus />
              分類ルールを作成
            </Button>
          </div>

          {labelRulesQuery.isError ? (
            <ErrorState onRetry={() => void labelRulesQuery.refetch()}>
              分類ルールの取得に失敗しました
            </ErrorState>
          ) : labelRulesQuery.isPending ? (
            <ListSkeleton rows={2} />
          ) : labelRules.length === 0 ? (
            <EmptyState>分類ルールがありません</EmptyState>
          ) : (
            <ul className="flex flex-col gap-2">
              {labelRules.map((rule) => (
                <li key={rule.id}>
                  <LabelRuleRow
                    rule={rule}
                    onEdit={() => setLabelRuleEditor({ rule })}
                  />
                </li>
              ))}
            </ul>
          )}
        </section>
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

/** LabelRuleRow はシリーズ分類ルールの一覧 1 行。 */
function LabelRuleRow({ rule, onEdit }: { rule: LabelRule; onEdit: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const remove = useDeleteLabelRule()
  const [confirmOpen, setConfirmOpen] = useState(false)

  const doRemove = () => {
    remove.mutate(
      { id: rule.id },
      {
        onSuccess: () => {
          setConfirmOpen(false)
          toast({ message: '分類ルールを削除しました（シリーズは再評価の後に変わります）' })
          void queryClient.invalidateQueries({ queryKey: getListLabelRulesQueryKey() })
          void queryClient.invalidateQueries({ queryKey: getListRecordingShelvesQueryKey() })
        },
        onError: (err) =>
          toast({ message: apiErrorMessage(err) ?? '分類ルールの削除に失敗しました', kind: 'error' }),
      },
    )
  }

  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-border p-3">
      <div className="flex min-w-0 flex-col">
        <span className="truncate text-sm text-foreground">
          「{rule.keyword}」→ {rule.value}
        </span>
        <span className="text-xs text-muted-foreground">優先度 {rule.priority ?? 0}</span>
        {rule.valueKey !== rule.value && (
          <span className="text-xs text-muted-foreground">
            この値は棚キー {rule.valueKey} として扱われます
          </span>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <Button type="button" size="icon-sm" variant="ghost" aria-label="編集" onClick={onEdit}>
          <Pencil />
        </Button>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label="削除"
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2 />
        </Button>
      </div>

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>分類ルール「{rule.keyword}」を削除しますか？</DialogTitle>
            <DialogDescription>
              このルールが勝っていた録画は、次に当たるルールへ移ります。無ければ自動キーの
              シリーズへ戻ります（全録画の再評価が走ります）。
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setConfirmOpen(false)}>
              キャンセル
            </Button>
            <Button type="button" variant="destructive" onClick={doRemove} disabled={remove.isPending}>
              削除する
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
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
  isCountingReservations,
  onCountingReservationsChange,
  onCreateLabelRule,
}: {
  rule: Rule
  disambiguate: (rule: Rule) => string | undefined
  isCountingReservations: boolean
  onCountingReservationsChange: (counting: boolean) => void
  onCreateLabelRule: (keyword: string) => void
}) {
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
  const [disableConfirmOpen, setDisableConfirmOpen] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)

  const setEnabled = (enabled: boolean) => {
    const key = getListRulesQueryKey()
    const previousEnabled = rule.enabled
    queryClient.setQueryData<ListRulesQueryResult>(key, (current) =>
      current === undefined
        ? current
        : {
            ...current,
            data: current.data.map((item) =>
              item.id === rule.id ? { ...item, enabled } : item,
            ),
          },
    )

    updateRule.mutate(
      {
        id: rule.id,
        data: buildRuleInput(
          conditionsToDraft(rule),
          { ...ruleToMeta(rule), enabled },
          // preserve を落とすと UI を持たない項目（description / dedupe* /
          // filenameTemplate / metadata）が UpdateRule の全置換で黙って消える。
          rule,
        ),
      },
      {
        onSuccess: () => {
          void queryClient.invalidateQueries({ queryKey: key })
          // ruler はレベルトリガーなので、再取得しても次回評価までは予約が残る。
          void queryClient.invalidateQueries({ queryKey: getListReservationsQueryKey() })
        },
        onError: (err) => {
          queryClient.setQueryData<ListRulesQueryResult>(key, (current) =>
            current === undefined
              ? current
              : {
                  ...current,
                  data: current.data.map((item) =>
                    item.id === rule.id ? { ...item, enabled: previousEnabled } : item,
                  ),
                },
          )
          toast({
            message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました',
            kind: 'error',
          })
        },
      },
    )
  }

  const toggleEnabled = async () => {
    if (!rule.enabled) {
      setEnabled(true)
      return
    }

    if (isCountingReservations) return
    onCountingReservationsChange(true)
    try {
      const response = await queryClient.fetchQuery(getListReservationsQueryOptions())
      const count = (unwrap(response) ?? []).filter(
        (reservation) =>
          reservation.ruleId === rule.id &&
          reservation.source === 'rule' &&
          reservation.state === 'active',
      ).length
      setActiveReservationCount(count)
      setDisableConfirmOpen(true)
    } catch (err) {
      toast({
        message: apiErrorMessage(err) ?? '予約数の取得に失敗しました',
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

  return (
    <div className="rounded-lg border border-border px-3 py-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
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

          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-sm text-muted-foreground">
            <span>優先度 {rule.priority}</span>
            <span>{keepOriginalLabel(keep)}</span>
            <span>
              {profiles.length === 0 ? 'エンコードなし' : profiles.join(', ')}
            </span>
          </div>
        </div>
        <div className="flex shrink-0 items-start gap-1">
          <div className="flex flex-col items-end gap-2">
            <button
              type="button"
              role="switch"
              aria-checked={rule.enabled}
              aria-label={`ルール「${displayName}」を有効にする`}
              disabled={updateRule.isPending || isCountingReservations}
              className="inline-flex min-h-8 items-center rounded-full px-1 outline-none disabled:opacity-50 focus-visible:ring-3 focus-visible:ring-ring/50"
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
            {/* このルール由来の録画だけに絞った /recordings への導線（issue #137）。
                条件モデルは検索（ProgramSearchRequest）と共有しないので、遷移先は
                /search ではなく /recordings?ruleId=N になる。 */}
            <Button
              variant="ghost"
              size="sm"
              render={<Link to="/recordings" search={{ ruleId: rule.id }} />}
            >
              このルールの録画
            </Button>
            {/* 録画ルールのキーワードを分類ルール（シリーズ）へ写す導線。
                録画ルールから分類ルールを**継承はさせない**（勝者ルールだけを
                継承すると広いルールに黙って負ける。docs/data/series.md §8
                「評価結果を宛先にしない」）。このボタンは同じ /rules の
                ダイアログを開くだけで、保存は利用者が明示する。 */}
            {firstKeyword(rule) !== undefined && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => onCreateLabelRule(firstKeyword(rule) as string)}
              >
                このキーワードで分類ルールを作る
              </Button>
            )}
          </div>
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
      </div>

      <AlertDialog open={disableConfirmOpen} onOpenChange={setDisableConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ルール「{displayName}」を無効にしますか？</AlertDialogTitle>
            <AlertDialogDescription>
              {`「${displayName}」を無効にすると、このルールによる予約 ${activeReservationCount} 件が取り消されます。手動で予約したものは残ります。`}
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
