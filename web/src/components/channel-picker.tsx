import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { Check, ChevronDown } from 'lucide-react'
import { useMemo, useState } from 'react'

import type { Service } from '@/api/generated'
import { orderServices } from '@/lib/epg-grid'
import { cn } from '@/lib/utils'

/**
 * ChannelPicker はチャンネル（サービス）の絞り込み UI。
 *
 * GR + BS + CS で数十局あり、`programs.tsx` の旧 `ServiceChips` は横スクロールの
 * チップ列だった。単一選択なのに選択中の値が画面外に隠れうる上、画面端からの
 * 横スワイプは Android のジェスチャーナビと衝突する（docs/frontend.md）。
 * 選択肢は非有界なので列挙を畳み、常に現在値を表示するトリガーボタンと、
 * 開いたときに縦スクロールするピッカーに変える。
 */

/** searchThreshold を超える候補数のときだけ絞り込み欄を出す。少数のときは検索欄が邪魔なだけ。 */
const searchThreshold = 15

/**
 * channelTypeLabels は channelType の日本語表記。
 *
 * 未知の種別が来てもコードをそのまま出す（lib/genre.ts と同じ規律 —
 * 分類の失敗を分類済みに見せない。「その他」に丸めない）。
 */
const channelTypeLabels: Record<string, string> = {
  GR: '地上波',
  BS: 'BS',
  CS: 'CS',
  SKY: 'SKY',
}

function channelTypeLabel(channelType: string): string {
  return channelTypeLabels[channelType] ?? channelType
}

type ChannelGroup = {
  channelType: string
  services: Service[]
}

/**
 * groupByChannelType は種別ごとの連続した塊にまとめる。
 *
 * 入力は orderServices ですでに種別順（GR → BS → CS → SKY）に並んでいる前提なので、
 * 独自の並び替えはせず、隣接する要素の channelType が変わったところで区切るだけでよい。
 * これによりグリッドの列順と食い違う 2 つ目の並び順を作らずに済む。
 */
function groupByChannelType(ordered: readonly Service[]): ChannelGroup[] {
  const groups: ChannelGroup[] = []
  for (const service of ordered) {
    const last = groups[groups.length - 1]
    if (last && last.channelType === service.channelType) {
      last.services.push(service)
    } else {
      groups.push({ channelType: service.channelType, services: [service] })
    }
  }
  return groups
}

export function ChannelPicker({
  services,
  selected,
  onChange,
}: {
  /** 候補。呼び出し側が絞り込み済みで渡す（並び順は保証されないので中で orderServices を通す）。 */
  services: Service[]
  /** 選択中の serviceId 集合。空集合は「すべて」。 */
  selected: ReadonlySet<number>
  onChange: (next: ReadonlySet<number>) => void
}): React.ReactElement {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')

  const ordered = useMemo(() => orderServices(services), [services])
  const selectedServices = useMemo(
    () => ordered.filter((s) => selected.has(s.serviceId)),
    [ordered, selected],
  )

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (q === '') return ordered
    return ordered.filter((s) => s.name.toLowerCase().includes(q))
  }, [ordered, query])

  const groups = useMemo(() => groupByChannelType(filtered), [filtered])

  // トリガーの表示は件数で切り替える。局名を並べる形にしないのは、狭い幅で
  // truncate されると件数が消えるため（2 局以上は常に「n 局を選択中」）。
  const countLabel =
    selectedServices.length === 0
      ? 'すべて'
      : selectedServices.length === 1
        ? selectedServices[0].name
        : `${selectedServices.length} 局を選択中`

  // 「すべて」は選択を空にする専用の項目。他の項目と同じくポップオーバーは閉じない
  // （複数選ぶのに毎回開き直させないため。閉じるのは外側クリック / Esc）。
  const clearAll = () => onChange(new Set())

  const toggle = (serviceId: number) => {
    const next = new Set(selected)
    if (next.has(serviceId)) next.delete(serviceId)
    else next.add(serviceId)
    onChange(next)
  }

  return (
    <PopoverPrimitive.Root
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        // 閉じたら検索語をリセットする（再度開いたときに前回の絞り込みが残っていると、
        // 「候補が減っている」ことに気付かず選びたいチャンネルが無いと誤解する）。
        if (!next) setQuery('')
      }}
    >
      <PopoverPrimitive.Trigger
        className={cn(
          'flex h-11 max-w-full items-center gap-1.5 rounded-lg border border-border bg-background px-3 text-sm text-foreground transition-colors',
          'hover:bg-muted aria-expanded:bg-muted aria-expanded:text-foreground',
        )}
      >
        {/* 見える側の値だけだと「これが何のコントロールか」が伝わらない。
            読み上げは「チャンネル: 現在値」にし、見える側は aria-hidden にして
            二重読みを避ける（components/capacity-shortfall-badge.tsx と同じ手法）。 */}
        <span className="sr-only">チャンネル: {countLabel}</span>
        <span aria-hidden="true" className="min-w-0 truncate">
          {selectedServices.length === 0 ? 'すべてのチャンネル' : countLabel}
        </span>
        <ChevronDown className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        {/* positionMethod は既定の 'absolute' ではなく 'fixed' にする。トリガーは
            sticky な PageHeader の中にあってスクロールしても動かないのに、
            'absolute' のポップアップはドキュメントと一緒に動く。この食い違いを
            ライブラリは毎スクロール JS で transform を打ち直して補正するが、
            実機のスクロールはコンポジタ側で先に動くため補正が 1 フレーム以上
            遅れ、メニューが上下に引っ張られて見える。'fixed' ならビューポート
            基準になり、トリガーと同じ動き（= 動かない）になるので補正自体が要らない。 */}
        <PopoverPrimitive.Positioner
          className="z-50 outline-none"
          positionMethod="fixed"
          side="bottom"
          align="start"
          sideOffset={6}
        >
          <PopoverPrimitive.Popup
            aria-label="チャンネル"
            className="flex max-h-[min(28rem,70vh)] w-[min(20rem,90vw)] flex-col overflow-hidden rounded-lg border border-border bg-popover text-popover-foreground shadow-md outline-none"
          >
            {ordered.length > searchThreshold && (
              <div className="shrink-0 border-b border-border p-2">
                <input
                  type="text"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="チャンネルを絞り込む"
                  aria-label="チャンネルを絞り込む"
                  className="h-9 w-full rounded-md border border-border bg-background px-2 text-sm outline-none focus-visible:border-ring"
                />
              </div>
            )}
            <div className="min-h-0 flex-1 overflow-y-auto p-1">
              <ChannelOption label="すべて" active={selected.size === 0} onClick={clearAll} />
              {groups.map((group) => (
                <div key={group.channelType}>
                  <div className="px-2 pt-2 pb-1 text-xs font-medium text-muted-foreground">
                    {channelTypeLabel(group.channelType)}
                  </div>
                  {group.services.map((s) => (
                    <ChannelOption
                      key={`${s.networkId}-${s.serviceId}`}
                      label={s.name}
                      remoteControlKeyId={
                        s.channelType === 'GR' && s.remoteControlKeyId > 0
                          ? s.remoteControlKeyId
                          : undefined
                      }
                      active={selected.has(s.serviceId)}
                      onClick={() => toggle(s.serviceId)}
                    />
                  ))}
                </div>
              ))}
              {groups.length === 0 && (
                <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                  一致するチャンネルがありません
                </p>
              )}
            </div>
          </PopoverPrimitive.Popup>
        </PopoverPrimitive.Positioner>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  )
}

function ChannelOption({
  label,
  remoteControlKeyId,
  active,
  onClick,
}: {
  label: string
  /** GR で `remoteControlKeyId > 0` のときだけ渡す。program-grid.tsx のヘッダと同じ見た目。 */
  remoteControlKeyId?: number
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        'flex min-h-11 w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors hover:bg-muted',
      )}
    >
      {/* 四角い枠 + チェックで複数選択であることを形で示す。Check アイコンだけだと
          「選ぶと他が外れる」単一選択に見える（ラジオボタンとの区別がつかない）。 */}
      <span
        aria-hidden="true"
        className={cn(
          'flex size-4 shrink-0 items-center justify-center rounded-sm border',
          active ? 'border-primary bg-primary' : 'border-border',
        )}
      >
        <Check
          className={cn('size-3', active ? 'text-primary-foreground opacity-100' : 'opacity-0')}
        />
      </span>
      {remoteControlKeyId !== undefined && (
        <span className="shrink-0 rounded bg-muted px-1 text-[10px] tabular-nums text-muted-foreground">
          {remoteControlKeyId}
        </span>
      )}
      <span className="truncate">{label}</span>
    </button>
  )
}
