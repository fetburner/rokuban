import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { MoreVertical, Trash2 } from 'lucide-react'
import { useState } from 'react'

import {
  getListRulesQueryKey,
  useCreateRule,
  useDeleteRule,
  useListRules,
  useUpdateRule,
  type DeleteRuleResponse,
  type Rule,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { ConditionFields } from '@/components/condition-fields'
import {
  EncodeSettingsFields,
  type EncodeSettingsValue,
} from '@/components/encode-settings-fields'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
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
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Field, Input } from '@/components/ui/field'
import { keepOriginalLabel, type KeepOriginal } from '@/lib/encode-settings'
import {
  buildRuleInput,
  buildSearchRequest,
  draftError,
  emptyDraft,
  emptyRuleMeta,
  ruleMetaError,
  ruleToDraft,
  ruleToMeta,
  type RuleMetaDraft,
  type SearchDraft,
} from '@/lib/program-search'
import { cn } from '@/lib/utils'

/**
 * RulesPage は録画ルールの一覧と作成・編集。
 *
 * 条件（テキスト・チャンネル種別・ジャンル・時間帯・無料放送・放送時間・期間・
 * サービス。列挙は DOM 順で、サービスが最後なのは読み込み中のレイアウトシフト
 * 対策 --- `condition-fields.tsx`）は検索画面（`/search`）と同じ
 * `ConditionFields` / `internal/rulequery` を通るので、ここでも全次元を編集できる
 * （M3-6 の時点では encodeProfiles / keepOriginal だけの編集に留めていたが、
 * `condition-fields.tsx` / `lib/program-search.ts` の切り出しでルール側にも
 * 同じ UI を持ち込めるようになった）。`UpdateRule` は子テーブル全置換なので、
 * UI が持たない項目（description / dedupe* / filenameTemplate / metadata /
 * sites）は `buildRuleInput` の `preserve` 引数で引き継ぐ。
 *
 * 各行の「検索しながら編集」（`/search?ruleId=N`）からも同じルールを上書き
 * 保存できる。導出ロジックが割り込む余地は無い純粋な「ユーザーの同期的な
 * 編集」同士なので、2 画面を同時に開いても単に最後に保存した方が勝つ
 * （docs/frontend.md「検索とルールは同じ条件 UI を双方向に共有する」）。
 */
export function RulesPage() {
  const query = useListRules()
  const rules = unwrap(query.data) ?? []
  const [editingId, setEditingId] = useState<number | 'new' | null>(null)

  return (
    <>
      <PageHeader title="ルール" />

      <div className="flex flex-col gap-4 px-4 py-4">
        {editingId === 'new' ? (
          <RuleForm
            mode="create"
            onCancel={() => setEditingId(null)}
            onSaved={() => setEditingId(null)}
          />
        ) : (
          <Button
            type="button"
            size="lg"
            className="w-full"
            onClick={() => setEditingId('new')}
          >
            ルールを作成
          </Button>
        )}

        {query.isError ? (
          <ErrorState>ルールの取得に失敗しました</ErrorState>
        ) : query.isPending ? (
          <ListSkeleton />
        ) : rules.length === 0 && editingId !== 'new' ? (
          <EmptyState>ルールがありません</EmptyState>
        ) : (
          <ul className="flex flex-col gap-3">
            {rules.map((rule) => (
              <li key={rule.id}>
                {editingId === rule.id ? (
                  <RuleForm
                    mode="edit"
                    rule={rule}
                    onCancel={() => setEditingId(null)}
                    onSaved={() => setEditingId(null)}
                  />
                ) : (
                  <RuleRow rule={rule} onEdit={() => setEditingId(rule.id)} />
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
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
 * 変えたいだけなら削除ではなく「編集」（id を保つ上書き保存）を使えることまで
 * 書く。
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
    '条件を変えたいだけなら「編集」で上書きしてください。'
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
 * 主操作（編集 / 検索しながら編集 / このルールの録画）は露出のまま、削除
 * （稀・破壊的）だけを overflow メニューに寄せる（issue #227）。以前は
 * 「編集」を開いた先の `RuleForm` フッタに保存・キャンセルと同格の
 * `destructive` ボタンとして置いていたが、それだと編集を開くたびに
 * 破壊的操作が主操作と並んでしまう。行を離れた overflow に置くことで、
 * 一覧を眺めているだけでは目に入らない位置にする。
 */
function RuleRow({ rule, onEdit }: { rule: Rule; onEdit: () => void }) {
  const profiles = rule.encodeProfiles ?? []
  const keep = (rule.keepOriginal ?? 'always') as KeepOriginal
  const conditions = summarizeRuleConditions(rule)
  const toast = useToast()
  const queryClient = useQueryClient()
  const deleteRule = useDeleteRule()
  const [confirmOpen, setConfirmOpen] = useState(false)

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
          <div className="flex flex-wrap items-center gap-2">
            <span className="truncate text-sm font-medium">{rule.name}</span>
            {!rule.enabled && (
              /* 文字色は text-foreground（bg-muted 小バッジの合成後コントラスト
                 対策。docs/frontend/design.md「コントラストは毎回測る」）。 */
              <span className="rounded bg-muted px-1.5 py-0.5 text-[0.65rem] text-foreground">
                無効
              </span>
            )}
          </div>

          {/* 条件が空 = 全番組にマッチする、という危険な状態を一覧でも
              見えるようにする（設定を開かないと気付けない事故を防ぐ）。 */}
          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-xs">
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

          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
            <span>優先度 {rule.priority}</span>
            <span>{keepOriginalLabel(keep)}</span>
            <span>
              {profiles.length === 0 ? 'エンコードなし' : profiles.join(', ')}
            </span>
          </div>
        </div>
        <div className="flex shrink-0 items-start gap-1">
          <div className="flex flex-col items-end gap-2">
            <Button type="button" variant="outline" size="sm" onClick={onEdit}>
              編集
            </Button>
            <Button
              variant="ghost"
              size="sm"
              render={<Link to="/search" search={{ ruleId: rule.id }} />}
            >
              検索しながら編集
            </Button>
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
          </div>
          {/* 破壊的・稀な操作（削除）は overflow に寄せる（issue #227）。 */}
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={`ルール「${rule.name}」のその他の操作`}
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

      {/* AlertDialogTrigger ではなく open を直接制御する: トリガーは
          overflow メニューの中の menuitem であり、選択するとメニュー自体は
          閉じる。ダイアログの開閉をメニューの寿命に結び付けず、ここで
          独立に持つ（issue #295: ルール削除の確認を他の破壊的操作と同じ
          AlertDialog に揃える）。 */}
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>ルール「{rule.name}」を削除しますか？</AlertDialogTitle>
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

type RuleFormProps =
  | { mode: 'create'; onCancel: () => void; onSaved: () => void }
  | { mode: 'edit'; rule: Rule; onCancel: () => void; onSaved: () => void }

function RuleForm(props: RuleFormProps) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const createRule = useCreateRule()
  const updateRule = useUpdateRule()

  const [draft, setDraft] = useState<SearchDraft>(() =>
    props.mode === 'edit' ? ruleToDraft(props.rule) : emptyDraft(),
  )
  const [meta, setMeta] = useState<RuleMetaDraft>(() =>
    props.mode === 'edit' ? ruleToMeta(props.rule) : emptyRuleMeta(),
  )

  const encodeValue: EncodeSettingsValue = {
    keepOriginal: meta.keepOriginal,
    encodeProfiles: meta.encodeProfiles,
  }
  const onEncodeChange = (next: EncodeSettingsValue) =>
    setMeta((m) => ({ ...m, keepOriginal: next.keepOriginal, encodeProfiles: next.encodeProfiles }))

  const formError = draftError(draft) ?? ruleMetaError(meta)
  const pending = createRule.isPending || updateRule.isPending
  const [matchAllConfirmOpen, setMatchAllConfirmOpen] = useState(false)

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })

  // 実際の作成/更新リクエスト。条件なしガードを通過した（or 元々条件が
  // あった）後の、保存の本体だけを持つ。
  //
  // **作成は成功トーストを残す。** `ListRules` は `ORDER BY priority DESC,
  // id ASC` で並ぶため、既定優先度（0）で作った新しい行は多くの場合
  // 一覧の下の方に入るが、作成フォームは常に一覧の先頭にある --- 実測
  // （Chromium 1280×900、既存 8 件、保存前後で scrollY は動かない）で、
  // 新しい行がビューポート外（y≈1221、フォールドの外）に出るケースを
  // 確認した。ページは新しい行へ自動スクロールしないので、この効果は
  // 画面外になりうる。issue #297 は「画面外になりうる効果」にはトースト
  // を残すことを認めており（RuleRow の削除と同じ判断基準）、無音化しない。
  //
  // **更新は成功トーストを出さない（issue #297）。** 編集フォームは対象の
  // 行をその場（ユーザーが「編集」を押した、既にスクロールして見えている
  // 位置）で置き換えるので、保存直後の画面には常に更新後の内容が現れる。
  // 作成と違って「一覧のどこか別の場所に新しく現れる」経路が無い。
  //
  // どちらも Undo（予約作成のような）にはしない。作成・更新は名前・条件・
  // エンコード設定を複数フィールド分書き込む操作で、予約のワンタップや
  // 削除のワンタップとは重みが違う --- 誤タップで即座に取り消したくなる
  // 操作ではない。取り消したければ、削除は既存の overflow メニュー +
  // 確認ダイアログが、更新のやり直しは「編集」が既にある。
  const doSave = () => {
    const data = buildRuleInput(draft, meta, props.mode === 'edit' ? props.rule : undefined)
    if (props.mode === 'create') {
      createRule.mutate(
        { data },
        {
          onSuccess: () => {
            toast({ message: 'ルールを作成しました' })
            void invalidate()
            props.onSaved()
          },
          onError: (err) =>
            toast({
              message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました',
              kind: 'error',
            }),
        },
      )
    } else {
      updateRule.mutate(
        { id: props.rule.id, data },
        {
          onSuccess: () => {
            void invalidate()
            props.onSaved()
          },
          onError: (err) =>
            toast({
              message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました',
              kind: 'error',
            }),
        },
      )
    }
  }

  // 条件が 1 つも無いルールは全番組にマッチする。編集フォームは条件の
  // どの次元も必須にしていない（何も指定しない = 「絞り込まない」が
  // 正しい状態でありうる、検索画面と同じ設計）ため、保存を止めるのではなく
  // 明示的な確認を挟む。一覧の要約表示（`summarizeRuleConditions` が
  // 空配列 → 警告バッジ）と合わせて「見えない事故」にならないよう二重に
  // 手当てする。
  //
  // 確認は `AlertDialog`（非同期）に挟むので、ここでは判定だけ行い、
  // 実際の送信（`doSave`）はダイアログの「保存する」を押した時にだけ走る。
  // キャンセルすれば `mutate` は一度も呼ばれず、フォームは編集可能なまま
  // 残る（保留中の送信も disabled のままの入力もない）。
  const save = () => {
    if (formError !== undefined) return

    if (Object.keys(buildSearchRequest(draft)).length === 0) {
      setMatchAllConfirmOpen(true)
      return
    }

    doSave()
  }

  return (
    <form
      aria-label={props.mode === 'create' ? 'ルールを作成' : 'ルールを編集'}
      className="flex flex-col gap-5 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
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

      {/* 条件が主役: 名前・有効/優先度は識別のための最小限の項目として上に
          置き、このフォームの大半は条件編集に使う。エンコード設定はさらに
          下（録画が決まったあとの後処理という位置づけ）。 */}
      <div className="flex flex-col gap-4 border-t border-border pt-4">
        <h2 className="text-sm font-semibold text-foreground">マッチ条件</h2>
        <ConditionFields draft={draft} onChange={setDraft} disabled={pending} />
      </div>

      <div className="flex flex-col gap-3 border-t border-border pt-4">
        <h2 className="text-sm font-semibold text-foreground">エンコード設定</h2>
        <EncodeSettingsFields value={encodeValue} onChange={onEncodeChange} disabled={pending} />
      </div>

      {formError !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {formError}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="lg" disabled={formError !== undefined || pending}>
          {pending ? '保存中…' : '保存'}
        </Button>
        <Button type="button" variant="outline" size="lg" disabled={pending} onClick={props.onCancel}>
          キャンセル
        </Button>
        {/* 削除はルール行の overflow メニューに移した（issue #227）。編集フォームは
            保存・キャンセルという主操作だけを持つ。 */}
      </div>

      <AlertDialog open={matchAllConfirmOpen} onOpenChange={setMatchAllConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>条件を指定せずに保存しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              条件を 1 つも指定していません。このまま保存すると、すべての番組が録画対象になります。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>キャンセル</AlertDialogCancel>
            <AlertDialogAction onClick={doSave}>保存する</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </form>
  )
}
