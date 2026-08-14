import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { getListCircuitBreakersQueryKey, type CircuitBreaker } from '@/api/generated'
import { CircuitBreakerBanner } from '@/components/circuit-breaker-banner'
import { ToastProvider } from '@/components/toaster'

function renderBanner() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0 },
      mutations: { retry: false },
    },
  })
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <CircuitBreakerBanner />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { ...utils, queryClient }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * fetch をトピック別に振り分けるスタブ。一覧取得と resume 呼び出しの両方に応答する。
 * `listSequence` を渡すと GET のたびに順番に返す（末尾に達したら最後の要素を使い続ける）。
 * 一覧に含まれる行の集合が GET のたびに変わる（発動・再開で増減する）ケースを模すのに使う。
 */
function stubFetch(opts: {
  list: CircuitBreaker[]
  listSequence?: CircuitBreaker[][]
  resume?: 'success' | 'error'
}) {
  let listCallCount = 0
  const fn = vi.fn((input: string | URL | Request, _init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/resume')) {
      if (opts.resume === 'error') {
        return Promise.resolve(jsonResponse({ error: '発動していません' }, 404))
      }
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (opts.listSequence) {
      const idx = Math.min(listCallCount, opts.listSequence.length - 1)
      listCallCount += 1
      return Promise.resolve(jsonResponse(opts.listSequence[idx]))
    }
    return Promise.resolve(jsonResponse(opts.list))
  })
  globalThis.fetch = fn as unknown as typeof fetch
  return fn
}

const trippedBreaker: CircuitBreaker = {
  site: 'default',
  name: 'ruler_deletes',
  trippedAt: '2026-07-25T10:00:00.000Z',
  pending: 3,
  threshold: 20,
  detail: {
    total: 3,
    programs: [
      { programId: 101, title: '朝のニュース' },
      { programId: 102, title: '昼の情報番組' },
    ],
  },
}

const excerptBreaker: CircuitBreaker = {
  ...trippedBreaker,
  name: 'reconcile_total_loss',
  pending: 200,
  detail: {
    total: 200,
    programs: trippedBreaker.detail.programs,
  },
}

describe('CircuitBreakerBanner', () => {
  it('発動中のブレーカーがあると通知が表示される', async () => {
    stubFetch({ list: [trippedBreaker] })

    renderBanner()

    expect(await screen.findByText('ルール評価による予約の削除が停止中')).toBeInTheDocument()
    expect(screen.getByText(/保留 3 件/)).toBeInTheDocument()
    expect(screen.getByText(/閾値 20/)).toBeInTheDocument()
    expect(screen.getByText(/削除が保留されています/)).toBeInTheDocument()
  })

  it('何も発動していなければ何も表示されない', async () => {
    const fetchMock = stubFetch({ list: [] })

    renderBanner()

    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    // ToastProvider 自体は常に空の aria-live コンテナを持つので、
    // バナー固有の要素・文言が無いことをピンポイントで確認する
    // （余計な枠（role="alert"）を出さないことが要件）。
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/削除が保留されています/)).not.toBeInTheDocument()
  })

  it('detail の番組（programId と title）が表示される', async () => {
    stubFetch({ list: [trippedBreaker] })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '内訳を見る' }))

    expect(screen.getByText(/101/)).toBeInTheDocument()
    expect(screen.getByText(/朝のニュース/)).toBeInTheDocument()
    expect(screen.getByText(/102/)).toBeInTheDocument()
    expect(screen.getByText(/昼の情報番組/)).toBeInTheDocument()
  })

  it('total が programs の件数より多いとき、抜粋であることが分かる表示になる', async () => {
    stubFetch({ list: [excerptBreaker] })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '内訳を見る' }))

    expect(screen.getByText(/200 件中 2 件を表示/)).toBeInTheDocument()
    expect(screen.getByText(/抜粋/)).toBeInTheDocument()
  })

  it('再開ボタンは確認を経てから API を叩く（確認せずに叩かれない）', async () => {
    const fetchMock = stubFetch({ list: [trippedBreaker], resume: 'success' })
    const user = userEvent.setup()
    renderBanner()

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    // ダイアログを開くだけでは叩かれない
    await user.click(await screen.findByRole('button', { name: '再開' }))
    expect(await screen.findByRole('button', { name: '再開する' })).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(1)

    // キャンセルしても叩かれない
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '再開する' })).not.toBeInTheDocument(),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)

    // 確認してはじめて叩かれる
    await user.click(screen.getByRole('button', { name: '再開' }))
    await user.click(await screen.findByRole('button', { name: '再開する' }))

    // 成功後に一覧を再取得するので呼び出し回数は増え続けうる。POST が
    // 実際に行われたことは呼び出し内容そのもので確認する（回数は問わない）。
    await waitFor(() => {
      const resumeCall = fetchMock.mock.calls.find(([u]) => String(u).includes('/resume'))
      expect(resumeCall).toBeDefined()
      expect(String(resumeCall![0])).toBe('/api/sites/default/breakers/ruler_deletes/resume')
      expect(resumeCall![1]).toMatchObject({ method: 'POST' })
    })

    expect(await screen.findByText(/再開しました/)).toBeInTheDocument()

    // 確定後はダイアログが閉じる。呼び出し側は個別のクローズ処理を持たず
    // AlertDialogAction（Close ラップ）に任せているので、ここで固定する（#131）。
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '再開する' })).not.toBeInTheDocument(),
    )
  })

  it('CircuitBreakerName に無い name が混ざっていても、他の発動中ブレーカーは消えない（issue #199）', async () => {
    // GET /api/breakers の name は openapi.yaml の CircuitBreakerName enum と
    // internal/breaker.All という 2 つの独立した手書きの列挙から作られており、
    // 将来またずれる可能性がある（サーバーはこのずれをエラーにせず値をその
    // まま通す設計 — internal/api/breakers.go の ListCircuitBreakers 参照）。
    // このコンポーネントは isError を見ておらず 0 件のときは何も描画しない
    // （:70 `if (breakers.length === 0) return null`）ため、もしサーバーが
    // enum 外の値を理由に一覧全体を 500 にしていたら、このバナー自体が
    // 丸ごと消えていた（ラッチの唯一の常設可視化が沈黙する、が最悪の結果）。
    // ここでは「未知の name が 1 件混ざっても、既知のブレーカーの表示は
    // 生き残る」ことを消費者側で固定する。
    const unknownNameBreaker = {
      ...trippedBreaker,
      name: 'not_a_declared_breaker',
    } as unknown as CircuitBreaker
    stubFetch({ list: [trippedBreaker, unknownNameBreaker] })

    renderBanner()

    // 既知のブレーカー（ruler_deletes）はラベル付きで表示される。
    expect(await screen.findByText('ルール評価による予約の削除が停止中')).toBeInTheDocument()
    // 未知の name も、識別子そのものへのフォールバック表示で残る
    // （describeBreakerName の未知値フォールバック。lib/breaker.ts 参照）。
    // 何も表示されない・行ごと消えるのではなく、識別子が見える形で残る
    // ことが「気付ける」の最低ラインである。
    expect(screen.getByText('not_a_declared_breakerが停止中')).toBeInTheDocument()
  })

  it('再開が失敗したときエラーが表示される（黙って成功に見せない）', async () => {
    const fetchMock = stubFetch({ list: [trippedBreaker], resume: 'error' })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '再開' }))
    await user.click(await screen.findByRole('button', { name: '再開する' }))

    expect(await screen.findByText(/再開に失敗しました/)).toBeInTheDocument()
    // 失敗しても発動中の表示自体は消えない(黙って成功したように見せない)
    expect(screen.getByText('ルール評価による予約の削除が停止中')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('同名ブレーカーが 2 サイトで発動しているとき、行の展開状態が site ごとに独立している（issue #293）', async () => {
    // name だけが同じで site が異なる 2 行。pending の値を変えておき、
    // どちらの行かを内容で追跡できるようにする。
    const siteA: CircuitBreaker = { ...trippedBreaker, site: 'default', pending: 3 }
    const siteB: CircuitBreaker = {
      ...trippedBreaker,
      site: 'second-site',
      pending: 7,
      detail: { total: 1, programs: [{ programId: 201, title: '別サイトの番組' }] },
    }
    // `GET /api/breakers` は `ORDER BY site, name`（internal/db/queries/circuit_breakers.sql）
    // で常に決定的な順序を返すので、並び順を入れ替えたりはしない。ここでは
    // site A が再開されて一覧から消える、という通常運用（resume）で集合の
    // 大きさが変わるケースを再現する。同名 (name) の別行が消えるとき、
    // 生き残った行（site B）の展開状態が正しく引き継がれるかを見る。
    const fetchMock = stubFetch({
      list: [siteA, siteB],
      listSequence: [
        [siteA, siteB],
        [siteB],
      ],
    })
    const user = userEvent.setup()
    const { queryClient } = renderBanner()

    // 内訳の <li>（programId ごと）も role="listitem" を持つので、
    // バナー行だけを「保留 N 件」の文言で絞り込む。
    const findRows = async () =>
      (await screen.findAllByRole('listitem')).filter((row) =>
        within(row).queryByText(/保留 \d+ 件/),
      )

    const rows = await findRows()
    expect(rows).toHaveLength(2)
    const rowA = rows.find((row) => within(row).queryByText(/保留 3 件/))
    const rowB = rows.find((row) => within(row).queryByText(/保留 7 件/))
    if (!rowA || !rowB) throw new Error('site A / site B の行が見つからない')

    // site B（2 番目の行）だけを展開する。site A は折りたたんだままにする。
    await user.click(within(rowB).getByRole('button', { name: '内訳を見る' }))
    expect(within(rowB).getByRole('button', { name: '内訳を隠す' })).toBeInTheDocument()
    expect(within(rowA).getByRole('button', { name: '内訳を見る' })).toBeInTheDocument()

    // SSE の breakers トピックによる invalidate 相当（不変条件 5: イベントは
    // ヒント、真実は再取得で確定する）。site A が再開されて一覧から消える。
    await queryClient.invalidateQueries({ queryKey: getListCircuitBreakersQueryKey() })
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))

    const rowsAfter = await findRows()
    expect(rowsAfter).toHaveLength(1)
    const rowBAfter = rowsAfter.find((row) => within(row).queryByText(/保留 7 件/))
    if (!rowBAfter) throw new Error('再取得後に site B の行が見つからない')

    // site A が消えても、生き残った site B の展開状態は保たれるはず
    // （key が name だけだと、消えた行の fiber が誤って site B に
    // 再利用され、展開状態が失われる）。
    expect(within(rowBAfter).getByRole('button', { name: '内訳を隠す' })).toBeInTheDocument()
  })
})
