import { Dialog as DialogPrimitive } from '@base-ui/react/dialog'
import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { Check, ChevronDown, ChevronRight, Minus } from 'lucide-react'
import { useMemo, useState } from 'react'

import type { Service } from '@/api/generated'
import { DialogOverlay } from '@/components/ui/dialog'
import { channelTypeLabel, groupByChannelType, orderServices } from '@/lib/epg-grid'
import { mdMediaQuery, useMediaQuery } from '@/lib/use-media-query'
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
 * ChannelPicker はチャンネルの複数選択。
 *
 * **選択の identity は `Service.id`**（`networkId * 100000 + serviceId`）。
 * 呼び出し側がキーの作り方を選べるようにする（総称 + `keyOf` の注入）と、
 * 画面ごとに違う複合キーが生えて区切り文字すら揃わなくなる。
 *
 * 「すべて」は三状態の親チェックボックス。URL では空集合を「全局」と定義しているため、
 * 全局から 0 局へ変える瞬間だけは選択画面内の一時状態として持つ。そこでは
 * `onChange` を呼ばず、1 局以上選ばずに閉じたら全局へ戻す。空集合を URL に書いても
 * 0 局を表せず全局になってしまい、意味の無い共有状態を作るだけだからである。
 *
 * 番組表では単独のシート（狭い画面）/ポップオーバー（広い画面）を使う。録画・シリーズの
 * 絞り込みでは `presentation` を親シートの行と一覧に切り替える。後者では親が戻る時点を
 * 「閉じる」として扱うため、空選択を全局へ戻す判断も親に委ねる。
 */
export function ChannelPicker({
  services,
  selected,
  onChange,
  secondaryLabel,
  presentation = 'standalone',
  onEmbeddedOpen,
  onNoneSelectedChange,
}: {
  /** 候補。呼び出し側が絞り込み済みで渡す（並び順は保証されないので中で orderServices を通す）。 */
  services: Service[]
  /** 選択中の `Service.id` 集合。空集合は「すべて」。 */
  selected: ReadonlySet<number>
  onChange: (next: ReadonlySet<number>) => void
  /** secondaryLabel は各候補に添える補足。 */
  secondaryLabel?: (s: Service) => string | undefined
  /** standalone は独立したシート/ポップオーバー、filter-row と inline は親シート内の表示。 */
  presentation?: 'standalone' | 'filter-row' | 'inline'
  /** filter-row が選ばれたときに親シートをチャンネル一覧へ進める。 */
  onEmbeddedOpen?: () => void
  /** inline の一時的な 0 局状態を親に知らせ、戻るときの全局復帰に使う。 */
  onNoneSelectedChange?: (noneSelected: boolean) => void
}): React.ReactElement {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [noneSelected, setNoneSelected] = useState(false)
  const wide = useMediaQuery(mdMediaQuery)

  const ordered = useMemo(() => orderServices(services), [services])
  const allIds = useMemo(() => new Set(ordered.map((s) => s.id)), [ordered])
  const selectedServices = useMemo(
    () => ordered.filter((s) => selected.has(s.id)),
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

  const checkboxState = (scope: readonly Service[]): CheckboxState =>
    getCheckboxState(scope, selected, noneSelected)

  const toggleScope = (scope: readonly Service[]) => {
    if (scope.length === 0) return

    const state = checkboxState(scope)
    // 空集合は全局を表すため、部分操作の前に services（ピッカーの全候補）を
    // 明示集合にする。検索中や種別見出しの操作でも、表示中の scope だけを変える。
    // 起点は候補（allIds）内の id だけにする。候補外の id（EPG から消えた局の
    // URL など）を残すと「すべて」が 2 状態を往復し、空集合へ戻れなくなる。
    const next = noneSelected
      ? new Set<number>()
      : selected.size === 0
        ? new Set(allIds)
        : new Set([...selected].filter((id) => allIds.has(id)))

    if (state === true) {
      for (const service of scope) next.delete(service.id)
    } else {
      for (const service of scope) next.add(service.id)
    }

    if (next.size === 0) {
      // 0 局は URL に表現できない。次の選択か、閉じて全局に戻るまでローカルに保つ。
      setNoneSelected(true)
      onNoneSelectedChange?.(true)
      return
    }

    setNoneSelected(false)
    onNoneSelectedChange?.(false)
    onChange(setsEqual(next, allIds) ? new Set() : next)
  }

  const closeStandalonePicker = () => {
    // 閉じたら検索語をリセットする（前回の絞り込みで候補が隠れていると誤解するため）。
    if (noneSelected && selected.size > 0) onChange(new Set())
    setQuery('')
    setNoneSelected(false)
    onNoneSelectedChange?.(false)
  }

  const triggerClassName = cn(
    'flex h-11 max-w-full items-center gap-1.5 rounded-lg border border-border bg-background px-3 text-sm text-foreground transition-colors',
    'hover:bg-muted aria-expanded:bg-muted aria-expanded:text-foreground',
  )

  const options = (
    <>
      {ordered.length > searchThreshold && (
        <div className="shrink-0 border-b border-border p-2">
          <input
            type="text"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="チャンネルを絞り込む"
            aria-label="チャンネルを絞り込む"
            className="h-9 w-full rounded-md border border-border bg-background px-2 text-sm outline-none focus-visible:border-ring"
          />
        </div>
      )}
      <div
        className={cn(
          'min-h-0 flex-1 overflow-y-auto p-1',
          presentation === 'inline' && 'max-h-[60dvh] flex-none',
        )}
      >
        {noneSelected && (
          <p role="status" className="px-3 py-2 text-sm text-muted-foreground">
            1 つ以上選んでください
          </p>
        )}
        <ChannelOption
          label={query.trim() === '' ? 'すべて' : '一致したものをすべて'}
          checked={checkboxState(filtered)}
          disabled={filtered.length === 0}
          onClick={() => toggleScope(filtered)}
        />
        {groups.map((group) => (
          <div key={group.channelType}>
            {groups.length > 1 ? (
              <ChannelOption
                label={channelTypeLabel(group.channelType)}
                checked={checkboxState(group.services)}
                onClick={() => toggleScope(group.services)}
                heading
              />
            ) : (
              <div className="flex min-h-11 items-center px-2 text-sm font-medium text-muted-foreground">
                {channelTypeLabel(group.channelType)}
              </div>
            )}
            {group.services.map((service) => (
              <ChannelOption
                key={service.id}
                label={service.name}
                secondary={secondaryLabel?.(service)}
                remoteControlKeyId={
                  service.channelType === 'GR' && service.remoteControlKeyId > 0
                    ? service.remoteControlKeyId
                    : undefined
                }
                checked={checkboxState([service])}
                onClick={() => toggleScope([service])}
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
    </>
  )

  if (presentation === 'filter-row') {
    return (
      <button
        type="button"
        aria-label={`チャンネル: ${countLabel}`}
        onClick={onEmbeddedOpen}
        className="flex min-h-11 w-full items-center justify-between gap-3 rounded-lg px-2 text-sm text-foreground transition-colors hover:bg-muted"
      >
        <span>チャンネル</span>
        <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
          <span className="truncate">{countLabel}</span>
          <ChevronRight className="size-4 shrink-0" aria-hidden="true" />
        </span>
      </button>
    )
  }

  if (presentation === 'inline') {
    return <div className="flex min-h-0 flex-col">{options}</div>
  }

  const trigger = (
    <>
      {/* 見える側の値だけだと何のコントロールかが伝わらないため、読み上げ側に役割を置く。 */}
      <span className="sr-only">チャンネル: {countLabel}</span>
      <span aria-hidden="true" className="min-w-0 truncate">
        {selectedServices.length === 0 ? 'すべてのチャンネル' : countLabel}
      </span>
      <ChevronDown className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
    </>
  )

  if (wide) {
    return (
      <PopoverPrimitive.Root
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) closeStandalonePicker()
        }}
      >
        <PopoverPrimitive.Trigger className={triggerClassName}>{trigger}</PopoverPrimitive.Trigger>
        <PopoverPrimitive.Portal>
          {/* sticky なトリガーに追従させるため、位置はビューポート基準の fixed にする。 */}
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
              {options}
            </PopoverPrimitive.Popup>
          </PopoverPrimitive.Positioner>
        </PopoverPrimitive.Portal>
      </PopoverPrimitive.Root>
    )
  }

  return (
    <DialogPrimitive.Root
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) closeStandalonePicker()
      }}
    >
      <DialogPrimitive.Trigger className={triggerClassName}>{trigger}</DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogOverlay />
        <DialogPrimitive.Popup className="fixed inset-x-0 bottom-0 z-50 flex max-h-[85dvh] flex-col rounded-t-2xl bg-card pt-2 pb-[max(1.5rem,env(safe-area-inset-bottom))] text-foreground shadow-lg outline-none">
          <div aria-hidden className="mx-auto h-1 w-9 shrink-0 rounded-full bg-border" />
          <div className="grid shrink-0 grid-cols-[1fr_auto_1fr] items-center px-2">
            <span />
            <DialogPrimitive.Title className="text-base font-semibold">チャンネル</DialogPrimitive.Title>
            <DialogPrimitive.Close className="h-11 justify-self-end rounded-lg px-3 text-base font-semibold text-primary hover:bg-muted">
              完了
            </DialogPrimitive.Close>
          </div>
          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4 pb-1">{options}</div>
        </DialogPrimitive.Popup>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

type CheckboxState = boolean | 'mixed'

/** 表示と操作で同じ判定を使い、検索・見出しの部分集合でも状態が食い違わないようにする。 */
function getCheckboxState(
  scope: readonly Service[],
  selected: ReadonlySet<number>,
  noneSelected: boolean,
): CheckboxState {
  if (scope.length === 0 || noneSelected) return false
  if (selected.size === 0) return true

  const selectedCount = scope.reduce((count, service) => count + Number(selected.has(service.id)), 0)
  if (selectedCount === 0) return false
  if (selectedCount === scope.length) return true
  return 'mixed'
}

function setsEqual(left: ReadonlySet<number>, right: ReadonlySet<number>): boolean {
  return left.size === right.size && [...left].every((id) => right.has(id))
}

/**
 * ChannelOption は複数選択の 1 候補。
 * フォーカスは `Button` と同じ明示リングを使い、ブラウザ既定の outline は消す。
 */
function ChannelOption({
  label,
  secondary,
  remoteControlKeyId,
  checked,
  onClick,
  heading = false,
  disabled = false,
}: {
  label: string
  /** 補足ラベル（多サイトの site 名など）。渡されたときだけ添える。 */
  secondary?: string
  /** GR で `remoteControlKeyId > 0` のときだけ渡す。program-grid.tsx のヘッダと同じ見た目。 */
  remoteControlKeyId?: number
  checked: CheckboxState
  onClick: () => void
  heading?: boolean
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={checked}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        'flex min-h-11 w-full items-center gap-2 rounded-md border border-transparent px-2 py-2 text-left text-sm transition-[color,background-color] outline-none hover:bg-muted focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50',
        heading && 'font-medium',
      )}
    >
      {/* 四角い枠 + チェックで複数選択であることを形で示す。Check アイコンだけだと
          「選ぶと他が外れる」単一選択に見える（ラジオボタンとの区別がつかない）。 */}
      <span
        aria-hidden="true"
        className={cn(
          'flex size-4 shrink-0 items-center justify-center rounded-sm border',
          checked === true || checked === 'mixed'
            ? 'border-primary bg-primary'
            : 'border-border',
        )}
      >
        {checked === 'mixed' ? (
          <Minus className="size-3 text-primary-foreground" />
        ) : (
          <Check
            className={cn(
              'size-3',
              checked === true ? 'text-primary-foreground opacity-100' : 'opacity-0',
            )}
          />
        )}
      </span>
      {remoteControlKeyId !== undefined && (
        /* 文字色は text-foreground（bg-muted 小バッジの合成後コントラスト対策。
           docs/frontend/design.md「コントラストは毎回測る」）。 */
        <span className="shrink-0 rounded bg-muted px-1 text-xs text-foreground">
          {remoteControlKeyId}
        </span>
      )}
      <span className="truncate">{label}</span>
      {secondary !== undefined && (
        <span className="ml-auto shrink-0 text-xs text-muted-foreground">{secondary}</span>
      )}
    </button>
  )
}
