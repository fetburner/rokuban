import { useQueryClient } from '@tanstack/react-query'
import { ScanLine, Trash2 } from 'lucide-react'

import {
  getListCMLogosQueryKey,
  useDeleteCMLogo,
  useListCMLogos,
  type CMLogoState,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, EmptyState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { formatDateTime } from '@/lib/format'
import { mutationErrorMessage } from '@/lib/mutation-error-message'

function stateLabel(state: CMLogoState['state']): string {
  switch (state) {
    case 'learned':
      return '学習済み'
    case 'failed':
      return '失敗あり'
    default:
      return '未学習'
  }
}

function LogoRow({ logo }: { logo: CMLogoState }) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const remove = useDeleteCMLogo()
  const busy = remove.isPending

  return (
    <li className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4 sm:flex-row sm:items-center">
      <div className="flex size-16 shrink-0 items-center justify-center overflow-hidden rounded border border-border bg-muted">
        {logo.previewPng ? (
          <img
            src={`data:image/png;base64,${logo.previewPng}`}
            alt={`${logo.serviceName} のロゴ`}
            className="max-h-full max-w-full object-contain"
          />
        ) : (
          <ScanLine aria-hidden="true" className="size-6 text-muted-foreground" />
        )}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <h2 className="font-medium">{logo.serviceName}</h2>
          <span className="text-xs text-muted-foreground">
            Network {logo.networkId} / Service {logo.serviceId}
          </span>
        </div>
        <p className="mt-1 text-sm text-muted-foreground">
          {stateLabel(logo.state)} · 録画 {logo.recordingCount} 件
          {logo.failedCount > 0 && ` · 検出失敗 ${logo.failedCount} 件`}
          {logo.learnedAt && ` · 学習 ${formatDateTime(logo.learnedAt)}`}
        </p>
        {logo.state === 'failed' && (
          <p className="mt-1 text-sm text-destructive">
            検出に失敗した録画は、録画詳細から再試行できます。
          </p>
        )}
      </div>
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={busy || !logo.learnedAt}
        onClick={() => {
          remove.mutate(
            { networkId: logo.networkId, serviceId: logo.serviceId },
            {
              onSuccess: () => {
                void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
                toast({ message: `${logo.serviceName} のロゴを削除しました。次の検出で再学習します。` })
              },
              onError: (error) =>
                toast({
                  message: mutationErrorMessage('ロゴの削除に失敗しました', error),
                  kind: 'error',
                }),
            },
          )
        }}
      >
        <Trash2 data-icon="inline-start" />
        ロゴを削除
      </Button>
    </li>
  )
}

/** CMLogosPage lists learned station logos and station-level detection failures. */
export function CMLogosPage() {
  const query = useListCMLogos()
  const logos = unwrap(query.data) ?? []

  return (
    <>
      <PageHeader title="CM ロゴ" />
      <PageContent className="flex flex-col gap-4 px-4 py-4">
        <p className="text-sm text-muted-foreground">
          学習済みロゴは局ごとに共有されます。削除すると、次の CM 検出でロゴを学習し直します。
        </p>
        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()}>CM ロゴの取得に失敗しました</ErrorState>
        ) : query.isPending ? (
          <ListSkeleton />
        ) : logos.length === 0 ? (
          <EmptyState>録画局がありません</EmptyState>
        ) : (
          <ul className="flex flex-col gap-3">
            {logos.map((logo) => (
              <LogoRow key={`${logo.networkId}-${logo.serviceId}`} logo={logo} />
            ))}
          </ul>
        )}
      </PageContent>
    </>
  )
}
