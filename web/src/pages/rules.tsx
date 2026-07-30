import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  getListRulesQueryKey,
  useCreateRule,
  useDeleteRule,
  useListRules,
  useUpdateRule,
  type Rule,
  type RuleInput,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import {
  EncodeSettingsFields,
  type EncodeSettingsValue,
} from '@/components/encode-settings-fields'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/field'
import {
  encodeSettingsError,
  keepOriginalLabel,
  type KeepOriginal,
} from '@/lib/encode-settings'
import { cn } from '@/lib/utils'

/**
 * RulesPage は録画ルールの一覧と作成・編集。
 *
 * M3-6 の焦点は encodeProfiles / keepOriginal の編集可能化。条件の全次元 UI
 * （検索画面と同等）は検索で試してからルール API で書く運用を当面許容し、
 * ここでは名前・有効/無効・優先度・エンコード設定を編集できる形にする。
 * 既存ルールの更新時は子条件（textMatches 等）をそのまま往復させる
 * （UpdateRule は子テーブル全置換なので落とすと条件が消える）。
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

function RuleRow({ rule, onEdit }: { rule: Rule; onEdit: () => void }) {
  const profiles = rule.encodeProfiles ?? []
  const keep = (rule.keepOriginal ?? 'always') as KeepOriginal

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
          <div className="mt-1 flex flex-wrap gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
            <span>優先度 {rule.priority}</span>
            <span>{keepOriginalLabel(keep)}</span>
            <span>
              {profiles.length === 0 ? 'エンコードなし' : profiles.join(', ')}
            </span>
          </div>
        </div>
        <Button type="button" variant="outline" size="sm" onClick={onEdit}>
          編集
        </Button>
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
  const deleteRule = useDeleteRule()

  const initial = props.mode === 'edit' ? props.rule : undefined
  const [name, setName] = useState(initial?.name ?? '')
  const [enabled, setEnabled] = useState(initial?.enabled ?? true)
  const [priority, setPriority] = useState(String(initial?.priority ?? 10))
  const [encode, setEncode] = useState<EncodeSettingsValue>({
    keepOriginal: (initial?.keepOriginal ?? 'always') as KeepOriginal,
    encodeProfiles: initial?.encodeProfiles ? [...initial.encodeProfiles] : [],
  })

  const encodeError = encodeSettingsError(encode.keepOriginal, encode.encodeProfiles)
  const nameError = name.trim() === '' ? '名前は必須です' : undefined
  const formError = nameError ?? encodeError
  const pending = createRule.isPending || updateRule.isPending || deleteRule.isPending

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })

  const buildInput = (): RuleInput => {
    const priorityNum = Number(priority)
    const input: RuleInput = {
      name: name.trim(),
      enabled,
      priority: Number.isFinite(priorityNum) ? priorityNum : 10,
      keepOriginal: encode.keepOriginal,
      encodeProfiles: encode.encodeProfiles,
    }
    // UpdateRule は子テーブル全置換。編集時は既存の条件を落とさない。
    if (props.mode === 'edit') {
      const r = props.rule
      if (r.description !== undefined) input.description = r.description
      if (r.isFree !== undefined) input.isFree = r.isFree
      if (r.durationMinMs !== undefined) input.durationMinMs = r.durationMinMs
      if (r.durationMaxMs !== undefined) input.durationMaxMs = r.durationMaxMs
      if (r.periodStartAt !== undefined) input.periodStartAt = r.periodStartAt
      if (r.periodEndAt !== undefined) input.periodEndAt = r.periodEndAt
      if (r.textMatches !== undefined) input.textMatches = r.textMatches
      if (r.services !== undefined) input.services = r.services
      if (r.channelTypes !== undefined) input.channelTypes = r.channelTypes
      if (r.genres !== undefined) input.genres = r.genres
      if (r.times !== undefined) input.times = r.times
      if (r.sites !== undefined) input.sites = r.sites
      if (r.dedupeEnabled !== undefined) input.dedupeEnabled = r.dedupeEnabled
      if (r.dedupeThreshold !== undefined) input.dedupeThreshold = r.dedupeThreshold
      if (r.dedupeWindowSeconds !== undefined) {
        input.dedupeWindowSeconds = r.dedupeWindowSeconds
      }
      if (r.filenameTemplate !== undefined) input.filenameTemplate = r.filenameTemplate
      if (r.metadata !== undefined) input.metadata = r.metadata
    }
    return input
  }

  const save = () => {
    if (formError !== undefined) return
    const data = buildInput()
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

  const remove = () => {
    if (props.mode !== 'edit') return
    if (!window.confirm(`ルール「${props.rule.name}」を削除しますか？`)) return
    deleteRule.mutate(
      { id: props.rule.id },
      {
        onSuccess: () => {
          toast({ message: 'ルールを削除しました' })
          void invalidate()
          props.onSaved()
        },
        onError: (err) =>
          toast({ message: apiErrorMessage(err) ?? 'ルールの削除に失敗しました' }),
      },
    )
  }

  return (
    <form
      aria-label={props.mode === 'create' ? 'ルールを作成' : 'ルールを編集'}
      className="flex flex-col gap-4 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
      <Field label="名前">
        <Input
          value={name}
          disabled={pending}
          onChange={(e) => setName(e.target.value)}
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
            checked={enabled}
            disabled={pending}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          有効
        </label>
        <Field label="優先度" className="w-28">
          <Input
            type="number"
            min={0}
            value={priority}
            disabled={pending}
            onChange={(e) => setPriority(e.target.value)}
          />
        </Field>
      </div>

      <EncodeSettingsFields value={encode} onChange={setEncode} disabled={pending} />

      {/* encode 制約の詳細は EncodeSettingsFields 内。ここでは名前必須など
          フォーム全体の理由だけを出す（同じ文言の role=alert が二重にならないように）。 */}
      {nameError !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {nameError}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="lg" disabled={formError !== undefined || pending}>
          {pending ? '保存中…' : '保存'}
        </Button>
        <Button type="button" variant="outline" size="lg" disabled={pending} onClick={props.onCancel}>
          キャンセル
        </Button>
        {props.mode === 'edit' && (
          <Button
            type="button"
            variant="destructive"
            size="lg"
            disabled={pending}
            onClick={remove}
          >
            削除
          </Button>
        )}
      </div>
    </form>
  )
}
