import { Dialog, DialogContent } from '@/components/ui/dialog'
import { ProgramDialogPanel } from '@/components/program-dialog-panel'
import { renderInRouter, testSite } from '@/test/router'
import { screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

const program = {
  site: testSite,
  programId: 1,
  networkId: 32736,
  serviceId: 1024,
  startAt: new Date(Date.now() + 60 * 60_000).toISOString(),
  endAt: new Date(Date.now() + 2 * 60 * 60_000).toISOString(),
  durationMs: 60 * 60_000,
  name: '番組パネルのテスト',
  isFree: true,
}

function stubFetch() {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(new Response(JSON.stringify({ live: false })))
    }
    if (url.pathname === '/api/encode-profiles') {
      return Promise.resolve(new Response(JSON.stringify([])))
    }
    return Promise.resolve(new Response(JSON.stringify({})))
  }) as unknown as typeof fetch
}

describe('ProgramDialogPanel', () => {
  it('ProgramRow の chrome なしで見出し・詳細・常時表示の予約操作を描く', async () => {
    stubFetch()
    renderInRouter(
      <Dialog open>
        <DialogContent>
          <ProgramDialogPanel
            program={program}
            reserved={false}
            pending={false}
            reservationStateUnknown={false}
            onReserve={vi.fn()}
            onCancel={vi.fn()}
          />
        </DialogContent>
      </Dialog>,
    )

    const dialog = await screen.findByRole('dialog', { name: program.name })
    expect(within(dialog).getByRole('heading', { name: program.name })).toBeInTheDocument()
    expect(within(dialog).queryByTestId('program-row')).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('button', { expanded: true })).not.toBeInTheDocument()
    expect(within(dialog).getByTestId('program-dialog-actions')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '予約' })).toHaveClass('w-full')
    expect(await within(dialog).findByText('エンコードプロファイル')).toBeInTheDocument()
  })
})
