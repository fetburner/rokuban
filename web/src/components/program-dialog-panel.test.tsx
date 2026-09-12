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

function stubFetch(live = false) {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(new Response(JSON.stringify({ live })))
    }
    if (url.pathname === '/api/encode-profiles') {
      return Promise.resolve(new Response(JSON.stringify([])))
    }
    return Promise.resolve(new Response(JSON.stringify({})))
  }) as unknown as typeof fetch
}

describe('ProgramDialogPanel', () => {
  it('ProgramRow の chrome なしで見出し・詳細・閉じるボタン左側の予約操作を描く', async () => {
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
    const summaryRow = within(dialog).getByTestId('program-dialog-summary-row')
    const actions = within(summaryRow).getByTestId('program-dialog-actions')
    expect(actions.parentElement).toBe(summaryRow)
    expect(actions).toHaveClass('shrink-0', 'border-l', 'box-content', 'mr-5', 'w-20')
    expect(actions).not.toHaveClass('mt-4')
    expect(within(dialog).getByRole('heading', { name: program.name })).not.toHaveClass('pr-14')
    expect(within(actions).getByRole('button', { name: '予約' })).toHaveClass(
      'min-h-11',
      'w-full',
    )
    expect(await within(dialog).findByText('エンコードプロファイル')).toBeInTheDocument()
  })

  it('放送中も閉じるボタンを避けた同じ要約行でライブと予約を操作できる', async () => {
    stubFetch(true)
    const airingProgram = {
      ...program,
      startAt: new Date(Date.now() - 30 * 60_000).toISOString(),
      endAt: new Date(Date.now() + 30 * 60_000).toISOString(),
      name: '放送中の番組パネルのテスト',
    }

    renderInRouter(
      <Dialog open>
        <DialogContent>
          <ProgramDialogPanel
            program={airingProgram}
            reserved={false}
            pending={false}
            reservationStateUnknown={false}
            onReserve={vi.fn()}
            onCancel={vi.fn()}
          />
        </DialogContent>
      </Dialog>,
    )

    const dialog = await screen.findByRole('dialog', { name: airingProgram.name })
    await within(dialog).findByRole('link', { name: 'ライブで見る' })
    const summaryRow = within(dialog).getByTestId('program-dialog-summary-row')
    const actions = within(summaryRow).getByTestId('program-dialog-actions')
    expect(actions).toHaveClass('shrink-0', 'border-l', 'box-content', 'mr-5', 'w-[7.75rem]')
    expect(actions).not.toHaveClass('mt-4')
    expect(within(actions).getByRole('link', { name: 'ライブで見る' })).toHaveClass(
      'min-h-11',
      'min-w-11',
    )
    expect(within(actions).getByRole('button', { name: '予約' })).toHaveClass(
      'min-h-11',
      'w-full',
    )
  })
})
