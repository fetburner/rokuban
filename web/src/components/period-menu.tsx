import { Calendar, Check, ChevronDown, ChevronLeft, ChevronRight } from 'lucide-react'
import { useState } from 'react'

import { ResponsivePanel } from '@/components/responsive-panel'
import { Chip } from '@/components/ui/chip'
import { Field, Input } from '@/components/ui/field'
import type { RecordingsPageSearch } from '@/lib/recording-search'
import { cn } from '@/lib/utils'

// ponytail: UI ラフ用の仮実装。区切りは日本時間（UTC+9、夏時間なし）で固定。
const JST = 9 * 3600_000
const DAY = 86_400_000
const SEASONS = ['冬', '春', '夏', '秋'] as const

type Range = { from?: string; to?: string }
type Update = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => void

const jstMidnight = (y: number, m: number, d: number) => new Date(Date.UTC(y, m, d) - JST).toISOString()
const jst = (iso: string | Date) => {
  const t = new Date(new Date(iso).getTime() + JST)
  return { y: t.getUTCFullYear(), m: t.getUTCMonth(), d: t.getUTCDate(), wd: t.getUTCDay() }
}
const coolRange = (y: number, s: number): Range => ({ from: jstMidnight(y, s * 3, 1), to: jstMidnight(y, s * 3 + 3, 1) })
const md = (iso: string) => {
  const p = jst(iso)
  return `${p.m + 1}/${p.d}`
}
const ymd = (iso: string) => {
  const p = jst(iso)
  return `${p.y}-${String(p.m + 1).padStart(2, '0')}-${String(p.d).padStart(2, '0')}`
}
const sameRange = (a: Range, b: Range) => a.from === b.from && a.to === b.to

function presets(now: Date) {
  const t = jst(now)
  const s = Math.floor(t.m / 3)
  const [py, ps] = s === 0 ? [t.y - 1, 3] : [t.y, s - 1]
  return [
    { label: 'すべての期間', range: {} as Range },
    { label: '今週', range: { from: jstMidnight(t.y, t.m, t.d - ((t.wd + 6) % 7)) } },
    { label: '今月', range: { from: jstMidnight(t.y, t.m, 1) } },
    { label: '今クール', sub: `${t.y} ${SEASONS[s]}`, range: coolRange(t.y, s) },
    { label: '前クール', sub: `${py} ${SEASONS[ps]}`, range: coolRange(py, ps) },
  ]
}

/** coolOf は範囲がちょうど 1 クールならその年と期を返す。 */
function coolOf(r: Range): { y: number; s: number } | undefined {
  if (r.from === undefined) return undefined
  const p = jst(r.from)
  if (p.d !== 1 || p.m % 3 !== 0) return undefined
  const s = p.m / 3
  return sameRange(r, coolRange(p.y, s)) ? { y: p.y, s } : undefined
}

export function triggerLabel(r: Range, now: Date): string {
  if (r.from === undefined && r.to === undefined) return '期間'
  const cool = coolOf(r)
  if (cool) return `${cool.y} ${SEASONS[cool.s]}`
  const preset = presets(now).find((p) => sameRange(p.range, r))
  if (preset) return preset.label
  const end = r.to === undefined ? '' : md(new Date(new Date(r.to).getTime() - DAY).toISOString())
  return `${r.from === undefined ? '' : md(r.from)}〜${end}`
}

/** PeriodMenu はツールバーに置く期間の選択（案 B のラフ）。 */
export function PeriodMenu({ search, onChange }: { search: RecordingsPageSearch; onChange: Update }) {
  const now = new Date()
  const current: Range = { from: search.from, to: search.to }
  const active = current.from !== undefined || current.to !== undefined
  const [open, setOpen] = useState(false)

  return (
    <ResponsivePanel
      open={open}
      onOpenChange={setOpen}
      title="期間"
      triggerClassName={cn(
        'relative flex h-11 w-11 shrink-0 items-center justify-center gap-1.5 rounded-lg text-sm transition-colors md:w-auto md:border md:px-3 aria-expanded:bg-muted',
        active
          ? 'md:border-primary/40 md:bg-primary/10 text-foreground hover:bg-primary/15'
          : 'md:border-border md:bg-background text-foreground hover:bg-muted',
      )}
      trigger={
        <>
          <Calendar className="size-5 text-foreground md:size-4 md:text-muted-foreground" aria-hidden />
          <span className="sr-only md:not-sr-only">{triggerLabel(current, now)}</span>
          <ChevronDown className="hidden size-4 text-muted-foreground md:block" aria-hidden />
          {active && <span className="absolute top-2 right-2 size-2 rounded-full bg-primary md:hidden" aria-hidden />}
        </>
      }
      popupClassName="flex max-h-[min(34rem,80vh)] w-[min(20rem,90vw)] flex-col gap-3 overflow-y-auto rounded-lg border border-border bg-popover p-2 text-popover-foreground shadow-md outline-none"
    >
      <PeriodOptions search={search} onChange={onChange} onPicked={() => setOpen(false)} />
    </ResponsivePanel>
  )
}

/** PeriodOptions は期間の選択肢。デスクトップはツールバーのメニュー、スマホは絞り込みパネルの先頭に置く。 */
export function PeriodOptions({
  search,
  onChange,
  onPicked,
}: {
  search: RecordingsPageSearch
  onChange: Update
  onPicked?: () => void
}) {
  const now = new Date()
  const current: Range = { from: search.from, to: search.to }
  const [year, setYear] = useState(() => coolOf(current)?.y ?? jst(now).y)
  const set = (r: Range) => onChange((s) => ({ ...s, from: r.from, to: r.to }))
  const pick = (r: Range) => {
    set(r)
    onPicked?.()
  }
  const nowCool = { y: jst(now).y, s: Math.floor(jst(now).m / 3) }
  const selectedCool = coolOf(current)

  return (
    <>
      <ul className="flex flex-col">
        {presets(now).map((p) => {
          const on = sameRange(p.range, current)
          return (
            <li key={p.label}>
              <button
                type="button"
                onClick={() => pick(p.range)}
                className={cn(
                  'flex h-10 w-full items-center gap-2 rounded-md px-2 text-left text-sm hover:bg-muted',
                  on && 'font-medium',
                )}
              >
                <Check className={cn('size-4 shrink-0', on ? 'text-primary' : 'invisible')} aria-hidden />
                <span className="flex-1">{p.label}</span>
                {p.sub && <span className="text-xs text-muted-foreground">{p.sub}</span>}
              </button>
            </li>
          )
        })}
      </ul>

      <section className="flex flex-col gap-2 border-t border-border px-2 pt-3">
        <div className="flex items-center justify-between">
          <h3 className="text-xs font-medium text-muted-foreground">クールを選ぶ</h3>
          <div className="flex items-center gap-1">
            <button type="button" aria-label="前の年" onClick={() => setYear(year - 1)} className="rounded-md p-1 hover:bg-muted">
              <ChevronLeft className="size-4" />
            </button>
            <span className="w-12 text-center text-sm tabular-nums">{year}</span>
            <button
              type="button"
              aria-label="次の年"
              disabled={year >= nowCool.y}
              onClick={() => setYear(year + 1)}
              className="rounded-md p-1 hover:bg-muted disabled:opacity-30"
            >
              <ChevronRight className="size-4" />
            </button>
          </div>
        </div>
        <div className="grid grid-cols-4 gap-1.5">
          {SEASONS.map((name, s) => (
            <Chip
              key={name}
              active={selectedCool?.y === year && selectedCool.s === s}
              disabled={year > nowCool.y || (year === nowCool.y && s > nowCool.s)}
              onClick={() => pick(coolRange(year, s))}
            >
              {name}
            </Chip>
          ))}
        </div>
      </section>

      <section className="flex flex-col gap-2 border-t border-border px-2 pt-3 pb-1">
        <h3 className="text-xs font-medium text-muted-foreground">日付で指定（放送日）</h3>
        <div className="flex gap-2">
          <Field label="開始日" className="min-w-0 flex-1">
            <Input
              type="date"
              value={current.from === undefined ? '' : ymd(current.from)}
              onChange={(e) => {
                const [y, m, d] = e.target.value.split('-').map(Number)
                set({ ...current, from: e.target.value === '' ? undefined : jstMidnight(y, m - 1, d) })
              }}
            />
          </Field>
          <Field label="終了日（含む）" className="min-w-0 flex-1">
            <Input
              type="date"
              value={current.to === undefined ? '' : ymd(new Date(new Date(current.to).getTime() - DAY).toISOString())}
              onChange={(e) => {
                const [y, m, d] = e.target.value.split('-').map(Number)
                set({ ...current, to: e.target.value === '' ? undefined : jstMidnight(y, m - 1, d + 1) })
              }}
            />
          </Field>
        </div>
      </section>
    </>
  )
}
