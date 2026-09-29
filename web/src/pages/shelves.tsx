import { useQueryClient } from '@tanstack/react-query'
import { Link, useSearch } from '@tanstack/react-router'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'

import {
  getListLabelRulesQueryKey,
  getListRecordingShelvesQueryKey,
  ListRecordingShelvesKey,
  useDeleteLabelRule,
  useListLabelRules,
  useListRecordingShelves,
  type LabelRule,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { LabelRuleForm } from '@/components/label-rule-form'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { buildShelfRows, type ShelfRow } from '@/lib/shelves'

/**
 * ShelvesPage はシリーズ棚と分類ルールを 1 画面に置く。
 *
 * **2 つの画面に割らない。** 棚が間違っていると気付く場所と直す場所が同じでないと、
 * 「巨大な棚が目に見えるので直す場所が分かる」（docs/data/series.md §8）が
 * 成立しない。棚は自動キーの結果で、分類ルールはその上に重ねる上書きである。
 *
 * 棚の値（`value`）は正規化の産物なので表示名にならない。見出しには代表の録画の
 * 生タイトルを出す（`title`）。分類ルールの value に渡すのは見出しではなく値である。
 */
export function ShelvesPage() {
  const search = useSearch({ from: '/shelves' })
  const queryClient = useQueryClient()
  const shelvesQuery = useListRecordingShelves({
    key: ListRecordingShelvesKey.series,
  })
  const rulesQuery = useListLabelRules()

  const shelves = unwrap(shelvesQuery.data) ?? []
  const rules = unwrap(rulesQuery.data) ?? []
  const rows = buildShelfRows(shelves)

  const [creating, setCreating] = useState(search.keyword !== undefined)
  const [editing, setEditing] = useState<LabelRule | undefined>()
  const [formOpen, setFormOpen] = useState(search.keyword !== undefined)
  const [initial, setInitial] = useState<{ keyword?: string; value?: string } | undefined>(
    search.keyword !== undefined ? { keyword: search.keyword, value: search.value } : undefined,
  )

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: getListRecordingShelvesQueryKey() })
    void queryClient.invalidateQueries({ queryKey: getListLabelRulesQueryKey() })
  }

  const openCreate = (from?: { keyword?: string; value?: string }) => {
    setEditing(undefined)
    setCreating(true)
    setInitial(from)
    setFormOpen(true)
  }

  return (
    <>
      <PageHeader
        title="シリーズ棚"
        actions={
          <Button type="button" onClick={() => openCreate()}>
            <Plus />
            分類ルールを作成
          </Button>
        }
      />

      <PageContent className="flex flex-col gap-6 px-4 py-4">
        {shelvesQuery.isError ? (
          <ErrorState onRetry={() => void shelvesQuery.refetch()}>
            棚の取得に失敗しました
          </ErrorState>
        ) : shelvesQuery.isPending ? (
          <ListSkeleton />
        ) : rows.length === 0 ? (
          <EmptyState>再生できる録画がまだありません</EmptyState>
        ) : (
          <ShelfList rows={rows} onSplit={(row) => openCreate({ value: row.value ?? undefined })} />
        )}

        <section className="flex flex-col gap-3">
          <h2 className="text-sm font-medium text-foreground">分類ルール</h2>
          <p className="text-xs text-muted-foreground">
            自動キーの棚を割る・寄せるための上書き。キーワード 1 つと棚のキー 1 つで、
            先に当たったものが勝ちます。
          </p>
          {rulesQuery.isError ? (
            <ErrorState onRetry={() => void rulesQuery.refetch()}>
              分類ルールの取得に失敗しました
            </ErrorState>
          ) : rulesQuery.isPending ? (
            <ListSkeleton rows={2} />
          ) : rules.length === 0 ? (
            <EmptyState>分類ルールがありません</EmptyState>
          ) : (
            <ul className="flex flex-col gap-2">
              {rules.map((rule) => (
                <li key={rule.id}>
                  <LabelRuleRow
                    rule={rule}
                    onEdit={() => {
                      setEditing(rule)
                      setCreating(false)
                      setFormOpen(true)
                    }}
                    onDeleted={invalidate}
                  />
                </li>
              ))}
            </ul>
          )}
        </section>
      </PageContent>

      {/* **開いている間だけ描く。** フォームの初期値は state なので、常時マウント
          すると「棚を割る」で開いても前回の入力が残る（`initial` は初回マウントで
          しか読まれない）。開くたびに作り直すのが一番安い。 */}
      {formOpen && (
        <LabelRuleForm
          open
          onOpenChange={(next) => {
            if (next) return
            setFormOpen(false)
            setEditing(undefined)
            setCreating(false)
          }}
          rule={creating ? undefined : editing}
          initial={creating ? initial : undefined}
        />
      )}
    </>
  )
}

/**
 * ShelfList は棚の行。押すと分類ルールの作成へ入る（棚のキーを見出しに添える）。
 *
 * 録画一覧への導線は**置かない**。`GET /api/recordings` は実効シリーズで
 * 絞れない（キーセットの軸は `program_start_at` で、棚は行の同一性を変えない。
 * docs/api.md）。棚の中身は代表の録画から辿る。
 */
function ShelfList({ rows, onSplit }: { rows: ShelfRow[]; onSplit: (row: ShelfRow) => void }) {
  return (
    <ul className="flex flex-col gap-2">
      {rows.map((row) => (
        <li
          key={row.value ?? '__other__'}
          className="flex items-center justify-between gap-3 rounded-lg border border-border p-3"
        >
          <div className="flex min-w-0 flex-col">
            <span className="truncate text-sm text-foreground">{row.title}</span>
            <span className="truncate text-xs text-muted-foreground">
              {row.value ?? '（棚のキーなし）'} · {row.count} 件
            </span>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              // 実体は <a>（Link）なので base-ui の button の意味論を主張しない。
              nativeButton={false}
              render={<Link to="/recordings/$id" params={{ id: String(row.representativeId) }} />}
            >
              代表を見る
            </Button>
            {!row.isOther && row.value !== null && (
              <Button size="sm" variant="outline" onClick={() => onSplit(row)}>
                この棚を割る・指定する
              </Button>
            )}
          </div>
        </li>
      ))}
    </ul>
  )
}

/** LabelRuleRow は分類ルール 1 本（勝者順）。 */
function LabelRuleRow({
  rule,
  onEdit,
  onDeleted,
}: {
  rule: LabelRule
  onEdit: () => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const remove = useDeleteLabelRule()
  const [confirmOpen, setConfirmOpen] = useState(false)

  const doRemove = () => {
    remove.mutate(
      { id: rule.id },
      {
        onSuccess: () => {
          setConfirmOpen(false)
          // 削除も全件再評価を投入するので、値の棚は再評価の完了後に次点へ移る。
          toast({ message: '分類ルールを削除しました（棚は再評価の後に変わります）' })
          onDeleted()
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
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <Button size="icon-sm" variant="ghost" aria-label="編集" onClick={onEdit}>
          <Pencil />
        </Button>
        <Button
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
              棚へ戻ります（全録画の再評価が走ります）。
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
