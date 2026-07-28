import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { useListRecordingDropStats, type DropStat } from '@/api/generated'
import { DropStatsTable } from '@/pages/recordings'

/**
 * Harness は DropStatsTable と同じクエリキーを共有する監視用の隣接要素を描画する。
 * 「種別が無い行に何も出ない」系の確認は、クエリが解決したあとの状態を見る必要が
 * あるため（解決前だと stats が空で通ってしまう）。
 */
function Harness({ recordingId }: { recordingId: number }) {
  const query = useListRecordingDropStats(recordingId)
  return (
    <>
      <div data-testid="query-status">{query.status}</div>
      <DropStatsTable recordingId={recordingId} />
    </>
  )
}

function renderTable(stats: DropStat[], recordingId = 7) {
  globalThis.fetch = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify(stats), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ) as unknown as typeof fetch

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <Harness recordingId={recordingId} />
    </QueryClientProvider>,
  )
}

const stat = (pid: number, pidType?: string): DropStat => ({
  pid,
  packets: 100,
  drops: 0,
  errors: 0,
  scrambled: 0,
  ...(pidType === undefined ? {} : { pidType }),
})

describe('DropStatsTable', () => {
  it('PID 種別が日本語のラベルで出る', async () => {
    renderTable([stat(0x100, 'video'), stat(0x110, 'audio'), stat(0x0, 'pat')])

    expect(await screen.findByText('映像')).toBeInTheDocument()
    expect(screen.getByText('音声')).toBeInTheDocument()
    expect(screen.getByText('PAT')).toBeInTheDocument()
  })

  it('種別が無い PID は空欄扱いになり、PID 自体は出る', async () => {
    renderTable([stat(0x200)])

    await screen.findByText('success')
    expect(screen.getByText('0x0200')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.queryByText('映像')).not.toBeInTheDocument()
  })

  it('知らない種別はそのまま表示する（値の権威は Go 側）', async () => {
    renderTable([stat(0x300, 'ecm')])

    expect(await screen.findByText('ecm')).toBeInTheDocument()
  })
})
