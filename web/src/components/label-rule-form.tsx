import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  createLabelRule,
  getListLabelRulesQueryKey,
  useGetLabelRuleValueKey,
  updateLabelRule,
  type LabelRule,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/field'
import { useToast } from '@/components/toaster'
import { labelRulesQueryKeyPrefix } from '@/lib/events'
import { shelfInputError, valueKeyMismatch } from '@/lib/shelves'

/**
 * LabelRuleForm は分類ルール 1 本を作る / 上書きするダイアログ。
 *
 * **見出しは棚の表示名（代表の録画の生タイトル）で、`value` は棚のキー**という
 * 非対称をそのまま見せる。「この棚を割る」（今の棚から一部を別のキーへ出す）と
 * 「棚を指定する」（キーワードを既存のキーへ寄せる）は同じ 1 本のルールで、
 * 違うのは value に何を書くかだけである。専用の操作を 2 つ作らない。
 *
 * 作成・更新のどちらもサーバーが同じトランザクションで全件再評価のジョブを
 * 投入する。棚は直後に古い値を一度返し、ジョブの完了後に割れる
 * （docs/data/series.md §8）。**UI はジョブの完了を待たない** --- 進捗の
 * 観測点が API に無く、待つと画面が数十秒沈黙する。
 */
export function LabelRuleForm({
  open,
  onOpenChange,
  rule,
  initial,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** rule が非 nil なら上書き（PATCH）。nil なら新規（POST）。 */
  rule?: LabelRule
  /** initial は新規作成のときの初期値（棚や録画ルールからの導線が渡す）。 */
  initial?: { keyword?: string; value?: string }
}) {
  const toast = useToast()
  const queryClient = useQueryClient()

  const [keyword, setKeyword] = useState(rule?.keyword ?? initial?.keyword ?? '')
  const [value, setValue] = useState(rule?.value ?? initial?.value ?? '')
  // 棚のキーを手で書き換えるまではキーワードに追従させる。棚を割る導線では
  // キーワードがそのまま新しい棚のキーになるのが普通なので、2 度書かせない。
  const [valueTouched, setValueTouched] = useState(rule !== undefined || initial?.value !== undefined)
  const [priority, setPriority] = useState(String(rule?.priority ?? 0))
  const [pending, setPending] = useState(false)

  const error = shelfInputError(keyword, value)

  // 値にも自動キーと同じ正規化がかかり、最初の空白で切れる。実効の棚キーは
  // サーバーの series_key だけが知っている（UI に複製しない）ので、入力中の値を
  // 問い合わせて見せる。
  const valueKeyQuery = useGetLabelRuleValueKey(
    { value },
    { query: { enabled: open && value.trim() !== '' } },
  )
  const previewKey = unwrap(valueKeyQuery.data)?.valueKey
  const valueKeyNote = valueKeyMismatch(value, previewKey)

  const close = () => {
    onOpenChange(false)
    setKeyword('')
    setValue('')
    setValueTouched(false)
    setPriority('0')
  }

  const save = async () => {
    if (error !== undefined) return
    setPending(true)
    const data = { keyword, value, priority: Number(priority) || 0 }
    try {
      if (rule !== undefined) {
        await updateLabelRule(rule.id, data)
      } else {
        await createLabelRule(data)
      }
      toast({
        message: rule !== undefined ? '分類ルールを更新しました' : '分類ルールを作成しました（棚は再評価の後に変わります）',
      })
      // 分類ルールの一覧・棚・録画一覧のすべてが変わる。SSE でも届くが、
      // 押した本人の画面は待たせない。
      void queryClient.invalidateQueries({ queryKey: getListLabelRulesQueryKey() })
      void queryClient.invalidateQueries({
        predicate: (query) => query.queryKey[0] === labelRulesQueryKeyPrefix,
      })
      close()
    } catch (err) {
      toast({ message: apiErrorMessage(err) ?? '分類ルールの保存に失敗しました', kind: 'error' })
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={(next) => (next ? onOpenChange(true) : close())}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{rule !== undefined ? '分類ルールを編集' : '分類ルールを作成'}</DialogTitle>
          <DialogDescription>
            キーワードに当たった録画を、棚のキーの棚へ移します。棚の見出しは代表の録画の
            生タイトルなので、ここで入れるのは表示名ではなくキーです。保存すると全録画の
            再評価が走り、完了後に棚が変わります。
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <Field label="キーワード（録画タイトルへの部分一致）">
            <Input
              aria-label="キーワード"
              value={keyword}
              onChange={(e) => {
                setKeyword(e.target.value)
                if (!valueTouched) setValue(e.target.value)
              }}
            />
          </Field>
          <Field label="棚のキー">
            <Input
              aria-label="棚のキー"
              value={value}
              onChange={(e) => {
                setValue(e.target.value)
                setValueTouched(true)
              }}
            />
          </Field>
          <Field label="優先度（大きいほど先に当たる）">
            <Input
              aria-label="優先度"
              type="number"
              value={priority}
              onChange={(e) => setPriority(e.target.value)}
            />
          </Field>
          {valueKeyNote !== undefined && (
            <p className="text-xs text-muted-foreground" role="status">
              {valueKeyNote}
            </p>
          )}
          {error !== undefined && <p className="text-xs text-destructive">{error}</p>}
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={close} disabled={pending}>
            キャンセル
          </Button>
          <Button type="button" onClick={() => void save()} disabled={pending || error !== undefined}>
            {rule !== undefined ? '保存' : '作成'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
