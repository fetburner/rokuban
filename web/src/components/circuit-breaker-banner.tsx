import { useQueryClient } from '@tanstack/react-query'
import { useLayoutEffect, useRef, useState } from 'react'

import {
  getListCircuitBreakersQueryKey,
  useListCircuitBreakers,
  useResumeCircuitBreaker,
  type CircuitBreaker,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
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
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { describeBreakerName, describeBreakerReason } from '@/lib/breaker'
import { formatDateTime } from '@/lib/format'

/**
 * CircuitBreakerBanner は発動中の大量削除サーキットブレーカーを全画面に出す
 * 居座り型の通知（docs/recording.md §3.2、internal/breaker のラッチ設計）。
 *
 * トーストにしない理由: ブレーカーは人間が再開するまで止まり続けるラッチであり、
 * 一定時間で消えるトーストと組み合わせると「気付かないまま放置される」という
 * 最悪の結末になりうる。app-shell.tsx から呼び、ルートに関わらず常に見える
 * 位置に置く。
 *
 * `GET /api/breakers` は発動中のものだけを返す（空配列が正常系）ので、
 * このコンポーネントは 0 件のとき何も描画しない（余計な枠を出さない）。
 *
 * SSE の `breakers` トピック（lib/events.ts）を受けてこのクエリが invalidate
 * されると自動で再取得される。ここではマウント時の取得（TanStack Query の
 * 既定動作）にのみ依存し、SSE の取りこぼしを前提にしない
 * （不変条件 5: イベントはヒント、真実は再取得）。
 */
export function CircuitBreakerBanner() {
  const query = useListCircuitBreakers()
  const breakers = unwrap(query.data) ?? []
  const ref = useRef<HTMLDivElement>(null)

  // PageHeader（components/page.tsx）はこのバナーの高さぶん sticky の
  // top をずらす必要があるので、実測して --breaker-banner-height に書き出す。
  // 発動件数や内訳の展開/折りたたみで高さが変わるため ResizeObserver で追従する。
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) {
      document.documentElement.style.setProperty('--breaker-banner-height', '0px')
      return
    }
    const publish = () => {
      document.documentElement.style.setProperty('--breaker-banner-height', `${el.offsetHeight}px`)
    }
    publish()
    const observer = new ResizeObserver(publish)
    observer.observe(el)
    return () => {
      observer.disconnect()
      document.documentElement.style.setProperty('--breaker-banner-height', '0px')
    }
  }, [breakers.length])

  if (breakers.length === 0) return null

  return (
    <div
      ref={ref}
      role="alert"
      className="sticky top-0 z-20 border-b border-destructive/30 bg-destructive/10 text-destructive"
    >
      <p className="px-4 pt-2 text-xs">
        削除が保留されています。予約の作成と録画は続いています。
      </p>
      <ul className="divide-y divide-destructive/20">
        {breakers.map((breaker) => (
          <BreakerRow key={`${breaker.site}:${breaker.name}`} breaker={breaker} />
        ))}
      </ul>
    </div>
  )
}

function BreakerRow({ breaker }: { breaker: CircuitBreaker }) {
  const [expanded, setExpanded] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const toast = useToast()
  const queryClient = useQueryClient()
  const resume = useResumeCircuitBreaker()

  const label = describeBreakerName(breaker.name)
  const reason = describeBreakerReason(breaker.name)
  const total = breaker.detail.total
  const programs = breaker.detail.programs ?? []
  const isExcerpt = total > programs.length

  const handleConfirm = () => {
    // ダイアログは AlertDialogAction（AlertDialogPrimitive.Close ラップ）が
    // クリックで自動的に閉じる。ここでは実行の確定のみ行う。結果はトーストで
    // 伝える（黙って成功したように見せない。失敗時も必ずトーストを出す）。
    resume.mutate(
      { site: breaker.site, name: breaker.name },
      {
        onSuccess: () => {
          toast({ message: `${label}を再開しました` })
          void queryClient.invalidateQueries({ queryKey: getListCircuitBreakersQueryKey() })
        },
        onError: () => {
          toast({ message: `${label}の再開に失敗しました` })
        },
      },
    )
  }

  return (
    <li className="px-4 py-2.5 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <p className="font-medium">{label}が停止中</p>
          {reason && <p className="text-xs text-destructive/80">{reason}</p>}
          <p className="text-xs text-destructive/80">
            {formatDateTime(breaker.trippedAt)}から · 保留 {breaker.pending} 件
            {/* 全損シグネチャのブレーカーは件数の閾値を持たない（threshold は 0）。
                「閾値 0」と出すと設定ミスに見えるので、閾値がある場合だけ添える。 */}
            {breaker.threshold > 0 && `（閾値 ${breaker.threshold}）`}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            className="text-destructive hover:bg-destructive/10 hover:text-destructive"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
          >
            {expanded ? '内訳を隠す' : '内訳を見る'}
          </Button>
          <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
            <AlertDialogTrigger
              render={
                <Button variant="destructive" size="sm" disabled={resume.isPending}>
                  再開
                </Button>
              }
            />
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>{label}を再開しますか？</AlertDialogTitle>
                <AlertDialogDescription>
                  保留されていた削除 {breaker.pending} 件が実行されます。取り消せません。
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>キャンセル</AlertDialogCancel>
                <AlertDialogAction onClick={handleConfirm}>再開する</AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </div>
      </div>

      {expanded && (
        <div className="mt-2 rounded-md bg-background/60 p-2 text-xs text-foreground">
          <p className="mb-1 text-muted-foreground">
            {isExcerpt
              ? `${total} 件中 ${programs.length} 件を表示（抜粋）`
              : `${total} 件`}
          </p>
          {programs.length === 0 ? (
            <p className="text-muted-foreground">詳細はありません</p>
          ) : (
            <ul className="flex flex-col gap-1">
              {programs.map((program) => (
                <li key={program.programId} className="truncate">
                  #{program.programId} {program.title || '（番組名なし）'}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  )
}
