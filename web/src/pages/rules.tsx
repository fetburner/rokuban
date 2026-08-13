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
 * 条件（テキスト・サービス・チャンネル種別・ジャンル・時間帯・無料放送・
 * 放送時間・期間）は検索画面（`/search`）と同じ `ConditionFields` /
 * `internal/rulequery` を通るので、ここでも全次元を編集できる
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
 * deleteRuleConfirmMessage は削除の確認文を組み立てる。
 *
 * 重複排除を有効にしたルールでは、削除で**履歴が比較のスコープから外れる**
 * ことを事前に伝える。`recordings.rule_id` はルール削除で NULL に落ち、同じ
 * 条件で作り直しても新しい id になるので過去の録画は 1 件もマッチしない
 * （`docs/recording/ruler.md` §3.1「ルールの削除は履歴のスコープを消す」）。
 * 帰結は「窓の中の再放送を録り直す」であって録り逃しではないが、押した後に
 * 取り消せる操作でもないので、条件を変えたいだけなら削除ではなく「編集」
 * （id を保つ上書き保存）を使えることまで書く。
 *
 * 件数は出さない。削除前に「何件の録画がスコープから外れるか」を数える API は
 * 無いうえ、件数は行動を変えない（引き継がれないという質の情報で足りる）。
 */
function deleteRuleConfirmMessage(rule: Rule): string {
  const head = `ルール「${rule.name}」を削除しますか？`
  if (!rule.dedupeEnabled) return head
  return (
    `${head}\n\n` +
    'このルールの重複排除の履歴も一緒に外れます。同じ条件で作り直しても引き継がれず、' +
    '重複排除の窓の中の再放送を録り直します。条件を変えたいだけなら「編集」で上書きしてください。'
  )
}

/**
 * deleteRuleResultMessage は削除後のトーストの文言を組み立てる。
 *
 * 削除 API が返す内訳（削除した予約 / 編集済みのため残った予約）をそのまま出す
 * （`docs/recording/reservation-model.md` §4.3「ルール削除の UX は可視化で
 * 解決する」）。残った予約は定義上「ユーザーが自分で触ったもの」だけ
 * なので、件数は常に少なく 1 件ずつ説明できる。
 *
 * 応答が読めなかった場合（`unwrap` が undefined）は内訳なしの文言に落とす。
 * 削除自体は成功しているので、そこで黙るのは間違い。
 */
function deleteRuleResultMessage(res: DeleteRuleResponse | undefined): string {
  if (!res) return 'ルールを削除しました'
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

  const remove = () => {
    if (!window.confirm(deleteRuleConfirmMessage(rule))) return
    deleteRule.mutate(
      { id: rule.id },
      {
        onSuccess: (res) => {
          toast({ message: deleteRuleResultMessage(unwrap(res)) })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
        },
        onError: (err) =>
          toast({ message: apiErrorMessage(err) ?? 'ルールの削除に失敗しました' }),
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
              <span className="rounded bg-muted px-1.5 py-0.5 text-[0.65rem] text-muted-foreground">
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
                onClick={remove}
              >
                <Trash2 />
                削除
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
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

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })

  const save = () => {
    if (formError !== undefined) return

    // 条件が 1 つも無いルールは全番組にマッチする。編集フォームは条件の
    // どの次元も必須にしていない（何も指定しない = 「絞り込まない」が
    // 正しい状態でありうる、検索画面と同じ設計）ため、保存を止めるのではなく
    // 明示的な確認を挟む。既に削除で同じ window.confirm パターンを使っており、
    // 一覧の要約表示（`summarizeRuleConditions` が空配列 → 警告バッジ）と
    // 合わせて「見えない事故」にならないよう二重に手当てする。
    if (Object.keys(buildSearchRequest(draft)).length === 0) {
      const proceed = window.confirm(
        '条件を 1 つも指定していません。このまま保存すると、すべての番組が録画対象になります。続けますか？',
      )
      if (!proceed) return
    }

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
            toast({ message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました' }),
        },
      )
    } else {
      updateRule.mutate(
        { id: props.rule.id, data },
        {
          onSuccess: () => {
            toast({ message: 'ルールを更新しました' })
            void invalidate()
            props.onSaved()
          },
          onError: (err) =>
            toast({ message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました' }),
        },
      )
    }
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
    </form>
  )
}
