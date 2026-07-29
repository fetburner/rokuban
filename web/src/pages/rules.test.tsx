import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { EncodeProfileSummary, Rule } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { RulesPage } from '@/pages/rules'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const sampleRule: Rule = {
  id: 1,
  name: 'ニュース',
  enabled: true,
  priority: 10,
  keepOriginal: 'always',
  encodeProfiles: [],
  createdAt: '2026-07-01T00:00:00Z',
  updatedAt: '2026-07-01T00:00:00Z',
}

const profiles: EncodeProfileSummary[] = [{ name: 'h264' }, { name: 'hevc' }]

function stubApi(rules: Rule[] = [sampleRule]) {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(profiles))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    throw new Error(`unexpected fetch: ${url.pathname}`)
  }) as unknown as typeof fetch
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <RulesPage />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('RulesPage encode settings', () => {
  it('until_encoded でプロファイル空なら保存できない', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    expect(await screen.findByText('ニュース')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '編集' }))

    // フォームが出てから keepOriginal を until_encoded に
    const keepSelect = await screen.findByLabelText('原本の保持')
    await user.selectOptions(keepSelect, 'until_encoded')

    // プロファイルは選ばない → エラー表示 + 保存 disabled。
    // 同じ文言が EncodeSettingsFields とフォームフッタの両方に出る。
    await waitFor(() => {
      expect(
        screen.getAllByText(/エンコード後に原本を削除するには、プロファイルを 1 つ以上/).length,
      ).toBeGreaterThan(0)
    })
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
  })

  it('プロファイルを選べば until_encoded で保存できる', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))

    const keepSelect = await screen.findByLabelText('原本の保持')
    await user.selectOptions(keepSelect, 'until_encoded')
    // チェックボックスの accessible name はラベル内テキスト（h264）
    await user.click(screen.getByRole('checkbox', { name: 'h264' }))

    await waitFor(() => {
      expect(screen.getByRole('button', { name: '保存' })).not.toBeDisabled()
    })
  })
})
