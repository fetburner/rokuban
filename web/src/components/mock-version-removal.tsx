// PoC（参照用・マージしない）: エンコード版を 1 本ずつ削除する UI の画面案。
// window.__mock で入口の案を切り替える: 'A' 行に削除 / 'B' 行末の ⋯ / 'C' 容量を空けるモード。
import { MoreHorizontal, Trash2 } from 'lucide-react'
import { useState } from 'react'

import type { Recording } from '@/api/generated'
import { Button } from '@/components/ui/button'
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { formatBytes } from '@/lib/format'

export type MockVariant = 'A' | 'B' | 'C' | undefined

export function mockVariant(): MockVariant {
  return (window as unknown as { __mock?: MockVariant }).__mock
}

type Asset = NonNullable<Recording['encodedAssets']>[number]

const assetLabel = (a: Asset) => (a.cut ? `カット版 (${a.profile})` : a.profile)

export function removalPlan(recording: Recording, profiles: string[]) {
  const assets = recording.encodedAssets ?? []
  const removed = assets.filter((a) => profiles.includes(a.profile))
  const remaining = assets.filter((a) => !profiles.includes(a.profile))
  const hasOriginal = recording.sizeBytes !== undefined
  return {
    removed,
    remaining,
    hasOriginal,
    freed: removed.reduce((s, a) => s + (a.sizeBytes ?? 0), 0),
    refused: !hasOriginal && remaining.length === 0,
    irreversible: !hasOriginal,
    cutOnly: !hasOriginal && remaining.length > 0 && remaining.every((a) => a.cut) && removed.some((a) => !a.cut),
    clampKeep:
      hasOriginal &&
      recording.keepOriginal === 'until_encoded' &&
      (recording.encodeProfiles ?? []).every((p) => profiles.includes(p)),
  }
}

/** 最後の版（原本も無い）は消せない。 */
export function isLastCopy(recording: Recording, asset: Asset) {
  return removalPlan(recording, [asset.profile]).refused
}

function Consequences({ recording, profiles }: { recording: Recording; profiles: string[] }) {
  const plan = removalPlan(recording, profiles)
  return (
    <div className="flex flex-col gap-3 text-sm text-foreground">
      <p className="text-base font-semibold">{formatBytes(plan.freed)} が空きます</p>
      <div>
        <p className="text-xs text-muted-foreground">残る版</p>
        <ul className="mt-1 flex flex-col gap-0.5">
          {plan.remaining.map((a) => (
            <li key={a.profile} className="flex justify-between gap-4">
              <span>{assetLabel(a)}</span>
              <span className="text-muted-foreground">{formatBytes(a.sizeBytes ?? 0)}</span>
            </li>
          ))}
          {plan.hasOriginal && (
            <li className="flex justify-between gap-4">
              <span>原本 TS</span>
              <span className="text-muted-foreground">{formatBytes(recording.sizeBytes!)}</span>
            </li>
          )}
        </ul>
      </div>
      {plan.irreversible ? (
        <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-destructive">
          元に戻せません。原本 TS が削除済みのため、この版は二度と作れません。
        </p>
      ) : (
        <p className="text-muted-foreground">原本が残っているので、あとから「＋ エンコードを追加」で作り直せます。</p>
      )}
      {plan.cutOnly && (
        <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-destructive">
          残るのはカット版だけになります。CM 入りの版が無くなり、チャプターを直せなくなります。
        </p>
      )}
      {plan.clampKeep && <p className="text-muted-foreground">原本の保持は「常に保持」に切り替わります。</p>}
      <p className="text-xs text-muted-foreground">この録画だけが対象です。今後の録画はルールの設定に従います。</p>
    </div>
  )
}

export function MockRemoveDialog({
  recording,
  profiles,
  open,
  onOpenChange,
}: {
  recording: Recording
  profiles: string[]
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const assets = (recording.encodedAssets ?? []).filter((a) => profiles.includes(a.profile))
  const title = assets.length === 1 ? `「${assetLabel(assets[0])}」を削除しますか？` : `${assets.length} つの版を削除しますか？`
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription render={<div />}>
            <Consequences recording={recording} profiles={profiles} />
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>キャンセル</AlertDialogCancel>
          <AlertDialogAction variant="destructive">削除する</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/** 案 A: 行の末尾に「削除」を常置する。 */
export function MockInlineDelete({ recording, asset }: { recording: Recording; asset: Asset }) {
  const [open, setOpen] = useState(false)
  const last = isLastCopy(recording, asset)
  return (
    <>
      <Button
        type="button"
        size="sm"
        variant="ghost"
        className="text-destructive"
        disabled={last}
        aria-label={`${assetLabel(asset)}を削除`}
        onClick={() => setOpen(true)}
      >
        <Trash2 data-icon="inline-start" />
        削除
      </Button>
      <MockRemoveDialog recording={recording} profiles={[asset.profile]} open={open} onOpenChange={setOpen} />
    </>
  )
}

/** 案 B: 行末の ⋯ に入れる。 */
export function MockRowMenu({ recording, asset }: { recording: Recording; asset: Asset }) {
  const [open, setOpen] = useState(false)
  const last = isLastCopy(recording, asset)
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button type="button" variant="ghost" size="icon" aria-label={`${assetLabel(asset)}のその他の操作`} />}>
          <MoreHorizontal />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-64 p-1.5">
          <DropdownMenuItem className="min-h-9 text-destructive" disabled={last} onClick={() => setOpen(true)}>
            <Trash2 />
            <span className="flex flex-col">
              <span>この版を削除</span>
              <span className="text-xs text-muted-foreground">
                {last ? '最後の版です。録画ごと消すときはごみ箱へ' : `${formatBytes(asset.sizeBytes ?? 0)} 空きます`}
              </span>
            </span>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <MockRemoveDialog recording={recording} profiles={[asset.profile]} open={open} onOpenChange={setOpen} />
    </>
  )
}

/** 最後の版しか無い録画で、消せない理由を一度だけ出す（案 A / C）。 */
export function MockLastCopyNote({ recording }: { recording: Recording }) {
  const assets = recording.encodedAssets ?? []
  if (assets.length !== 1 || !isLastCopy(recording, assets[0])) return null
  return <p className="text-xs text-muted-foreground">最後の版なので削除できません。録画ごと消すときは ⋮ からごみ箱へ移します。</p>
}

/** 案 C: 「容量を空ける」で行に選択を出し、選んだぶんの結果をその場で見せる。 */
export function MockOrganizeBar({
  recording,
  organizing,
  setOrganizing,
  picked,
  setPicked,
}: {
  recording: Recording
  organizing: boolean
  setOrganizing: (v: boolean) => void
  picked: string[]
  setPicked: (v: string[]) => void
}) {
  const [open, setOpen] = useState(false)
  if (!organizing) {
    return (
      <div>
        <Button type="button" variant="ghost" size="sm" onClick={() => setOrganizing(true)}>
          容量を空ける…
        </Button>
      </div>
    )
  }
  const plan = removalPlan(recording, picked)
  return (
    <div className="flex flex-col gap-2 rounded-md border border-border bg-muted/40 p-3 text-sm">
      <p className="text-muted-foreground">削除する版を選んでください。原本 TS はここでは消せません。</p>
      <div className="flex flex-wrap items-center gap-2">
        <span className="mr-auto font-medium">{picked.length === 0 ? '未選択' : `${formatBytes(plan.freed)} が空きます`}</span>
        <Button type="button" size="sm" variant="ghost" onClick={() => { setOrganizing(false); setPicked([]) }}>
          やめる
        </Button>
        <Button type="button" size="sm" variant="destructive" disabled={picked.length === 0} onClick={() => setOpen(true)}>
          削除…
        </Button>
      </div>
      {picked.length > 0 && plan.irreversible && (
        <p className="text-xs text-destructive">原本 TS が削除済みのため、元に戻せません。</p>
      )}
      {picked.length > 0 && plan.cutOnly && (
        <p className="text-xs text-destructive">残るのはカット版だけになり、チャプターを直せなくなります。</p>
      )}
      <MockRemoveDialog recording={recording} profiles={picked} open={open} onOpenChange={setOpen} />
    </div>
  )
}

/** 録画一覧の一括: 選んだ録画からプロファイル単位で版を消す。 */
export function MockBulkRemoveDialog({
  recordings,
  open,
  onOpenChange,
}: {
  recordings: Recording[]
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const byProfile = new Map<string, { count: number; bytes: number; cut?: boolean }>()
  for (const r of recordings) {
    for (const a of r.encodedAssets ?? []) {
      const e = byProfile.get(a.profile) ?? { count: 0, bytes: 0 }
      byProfile.set(a.profile, { count: e.count + 1, bytes: e.bytes + (a.sizeBytes ?? 0), cut: a.cut })
    }
  }
  const [picked, setPicked] = useState<string[]>(['h265-1080p'])
  const plans = recordings
    .filter((r) => (r.encodedAssets ?? []).some((a) => picked.includes(a.profile)))
    .map((r) => removalPlan(r, picked))
  const ok = plans.filter((p) => !p.refused)
  const freed = ok.reduce((s, p) => s + p.freed, 0)
  const refused = plans.length - ok.length
  const irreversible = ok.filter((p) => p.irreversible).length
  const cutOnly = ok.filter((p) => p.cutOnly).length
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>選択した {recordings.length} 件から版を削除</AlertDialogTitle>
          <AlertDialogDescription render={<div />}>
            <div className="flex flex-col gap-3 text-sm text-foreground">
              <ul role="group" aria-label="削除する版" className="flex flex-col gap-1">
                {[...byProfile].map(([profile, e]) => (
                  <li key={profile}>
                    <label className="flex min-h-8 items-center gap-2">
                      <input
                        type="checkbox"
                        className="size-5 accent-primary"
                        checked={picked.includes(profile)}
                        onChange={() => setPicked(picked.includes(profile) ? picked.filter((p) => p !== profile) : [...picked, profile])}
                      />
                      <span className="mr-auto">{e.cut ? `カット版 (${profile})` : profile}</span>
                      <span className="text-muted-foreground">{e.count} 件・{formatBytes(e.bytes)}</span>
                    </label>
                  </li>
                ))}
              </ul>
              <p className="text-base font-semibold">{ok.length} 件で {formatBytes(freed)} が空きます</p>
              {irreversible > 0 && (
                <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-destructive">
                  {irreversible} 件は原本 TS が削除済みのため、元に戻せません。
                  {cutOnly > 0 && `うち ${cutOnly} 件はカット版だけが残り、チャプターを直せなくなります。`}
                </p>
              )}
              {refused > 0 && (
                <p className="text-muted-foreground">{refused} 件は最後の版なので削除しません。録画ごと消すときはごみ箱へ移します。</p>
              )}
              <p className="text-xs text-muted-foreground">一度に大量に消すと、削除の安全装置が止まって通知が出ます。</p>
            </div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>キャンセル</AlertDialogCancel>
          <AlertDialogAction variant="destructive" disabled={ok.length === 0}>
            {ok.length} 件から削除する
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
