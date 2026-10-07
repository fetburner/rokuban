import { Calendar, Check, ChevronDown, ChevronLeft, ChevronRight } from 'lucide-react'
import { useState } from 'react'

import { ToolbarDot, ToolbarPanel, toolbarButtonClass } from '@/components/toolbar-panel'
import { Chip } from '@/components/ui/chip'
import { Field, Input } from '@/components/ui/field'
import {
  courOf,
  courRange,
  currentCour,
  dateInputToFrom,
  dateInputToTo,
  fromToDateInput,
  isSelectedPreset,
  periodLabel,
  periodPresets,
  seasonNames,
  toToDateInput,
  type PeriodRange,
} from '@/lib/recording-period'
import type { RecordingsPageSearch } from '@/lib/recording-search'
import { cn } from '@/lib/utils'

type Update = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => void

/**
 * RecordingPeriodMenu はツールバーの期間ボタンとそのパネル。ボタンには今の期間
 * （`periodLabel`。チップと同じ関数）を出す（HIG の pop-up button「選んだ後は表示を変えて
 * 今の選択を示す」）。md 未満はアイコンだけにして、期間はチップで読ませる。
 *
 * **上段とクールは押すと閉じ、日付の入力は閉じない（「完了」・外側で閉じる）。** 上段と
 * クールは 1 回の選択で範囲が決まるメニューの項目である。日付は開始と終了の 2 欄を
 * 続けて入れるので、1 欄目で閉じると 2 欄目のために開き直させることになる。
 */
export function RecordingPeriodMenu({
  search,
  onChange,
  now,
}: {
  search: RecordingsPageSearch
  onChange: Update
  now: Date
}) {
  const [open, setOpen] = useState(false)
  const label = periodLabel({ from: search.from, to: search.to }, now)

  return (
    <ToolbarPanel
      title="期間"
      open={open}
      onOpenChange={setOpen}
      triggerClassName={toolbarButtonClass}
      trigger={
        <>
          <Calendar className="size-5 md:hidden" aria-hidden />
          {/* aria-label で上書きしない --- md 以上は見えている「2026 夏」がそのまま名前になる。 */}
          <span className="sr-only md:not-sr-only">{label ?? '期間'}</span>
          <ChevronDown className="hidden size-4 text-muted-foreground md:block" aria-hidden />
          {label !== undefined && <ToolbarDot />}
        </>
      }
      popupWidthClassName="w-[min(20rem,90vw)]"
      bodyClassName="flex flex-col gap-3"
    >
      <PeriodOptions search={search} onChange={onChange} now={now} onPicked={() => setOpen(false)} />
    </ToolbarPanel>
  )
}

function PeriodOptions({
  search,
  onChange,
  now,
  onPicked,
}: {
  search: RecordingsPageSearch
  onChange: Update
  now: Date
  onPicked: () => void
}) {
  const range: PeriodRange = { from: search.from, to: search.to }
  const selectedCour = courOf(range)
  const nowCour = currentCour(now)
  // 開くたびに中身がマウントし直されるので、初期値は開いた時点の選択から取れば足りる。
  const [year, setYear] = useState(selectedCour?.year ?? nowCour.year)
  const pick = (next: PeriodRange) => {
    onChange((s) => ({ ...s, from: next.from, to: next.to }))
    onPicked()
  }

  return (
    <>
      <ul className="flex flex-col">
        {periodPresets(now).map((preset) => {
          const selected = isSelectedPreset(preset, range)
          return (
            <li key={preset.label}>
              <button
                type="button"
                aria-pressed={selected}
                onClick={() => pick(preset.range)}
                className={cn(
                  'flex h-11 w-full items-center gap-2 rounded-md px-2 text-left text-sm hover:bg-muted md:h-9',
                  selected && 'font-medium',
                )}
              >
                <Check className={cn('size-4 shrink-0', selected ? 'text-primary' : 'invisible')} aria-hidden />
                <span className="flex-1">{preset.label}</span>
                {preset.detail !== undefined && (
                  <span className="text-xs text-muted-foreground">{preset.detail}</span>
                )}
              </button>
            </li>
          )
        })}
      </ul>

      <section className="flex flex-col gap-2 border-t border-border pt-3">
        <div className="flex items-center justify-between">
          <h3 className="text-xs font-medium text-muted-foreground">クールを選ぶ</h3>
          <div className="flex items-center">
            <button
              type="button"
              aria-label="前の年"
              onClick={() => setYear(year - 1)}
              className="flex size-9 items-center justify-center rounded-md hover:bg-muted"
            >
              <ChevronLeft className="size-4" aria-hidden />
            </button>
            <span className="w-12 text-center text-sm">{year}</span>
            <button
              type="button"
              aria-label="次の年"
              disabled={year >= nowCour.year}
              onClick={() => setYear(year + 1)}
              className="flex size-9 items-center justify-center rounded-md hover:bg-muted disabled:opacity-30"
            >
              <ChevronRight className="size-4" aria-hidden />
            </button>
          </div>
        </div>
        <div role="group" aria-label={`${year} 年のクール`} className="grid grid-cols-4 gap-1.5">
          {seasonNames.map((name, season) => (
            <Chip
              key={name}
              active={selectedCour?.year === year && selectedCour.season === season}
              // まだ始まっていない期は押せない（録画が 1 件も無いことが確定している）。
              disabled={year > nowCour.year || (year === nowCour.year && season > nowCour.season)}
              onClick={() => pick(courRange({ year, season }))}
            >
              {name}
            </Chip>
          ))}
        </div>
      </section>

      <section className="flex flex-col gap-2 border-t border-border pt-3">
        <h3 className="text-xs font-medium text-muted-foreground">日付で指定（放送日）</h3>
        <div className="flex gap-2">
          {/* iOS は 16px 未満の入力欄にフォーカスすると拡大するので、md 未満は text-base にする。 */}
          <Field label="開始日" className="min-w-0 flex-1">
            <Input
              type="date"
              value={fromToDateInput(search.from)}
              onChange={(e) => onChange((s) => ({ ...s, from: dateInputToFrom(e.target.value) }))}
              className="h-11 text-base md:h-9 md:text-sm"
            />
          </Field>
          <Field label="終了日（この日を含む）" className="min-w-0 flex-1">
            <Input
              type="date"
              value={toToDateInput(search.to)}
              onChange={(e) => onChange((s) => ({ ...s, to: dateInputToTo(e.target.value) }))}
              className="h-11 text-base md:h-9 md:text-sm"
            />
          </Field>
        </div>
      </section>
    </>
  )
}
