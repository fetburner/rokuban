import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { EncodeProfileSummary, Recording, Rule } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

function sampleRecording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 3,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: 1,
    title: '単体ページの録画',
    startAt: '2026-01-01T12:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-01T12:30:00Z',
    ...overrides,
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: body === null ? undefined : { 'Content-Type': 'application/json' },
  })
}

function sampleRule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 5,
    name: 'サンプルルール',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

/**
 * createFakeServer は `GET /api/recordings/{id}` 単体取得とその周辺
 * （削除・復元・完全削除・追加エンコード）を状態を持ってシミュレートする。
 * `recordings.test.tsx` の `createFakeRecordingsServer` は一覧
 * （`GET /api/recordings`）専用なので、単体ページのテストにそのまま使えない
 * （このページは一覧を叩かない）。
 */
function createFakeServer(options: {
  recording: Recording | null
  encodeProfiles?: EncodeProfileSummary[]
  rules?: Rule[]
  /** rulesResponse はルール一覧の解決を遅延させるテスト用。 */
  rulesResponse?: () => Promise<Response>
  // deleteResponse / restoreResponse / encodePostResponse は各操作の応答を
  // 差し替える（既定は成功）。失敗トーストや 409 翻訳の確認用。
  deleteResponse?: () => Response
  restoreResponse?: () => Response
  encodePostResponse?: () => Response
}) {
  let recording = options.recording
  const encodeProfiles = options.encodeProfiles ?? []
  const rules = options.rules ?? []
  const rulesResponse = options.rulesResponse
  const deleteResponse = options.deleteResponse
  const restoreResponse = options.restoreResponse
  const encodePostResponse = options.encodePostResponse

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(encodeProfiles))
    if (url.pathname === '/api/rules' && method === 'GET') {
      return rulesResponse ? rulesResponse() : Promise.resolve(jsonResponse(rules))
    }

    const getMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (getMatch && method === 'GET') {
      const id = Number(getMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      return Promise.resolve(jsonResponse(recording))
    }

    const deleteMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (deleteMatch && method === 'DELETE') {
      if (deleteResponse) return Promise.resolve(deleteResponse())
      const id = Number(deleteMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      recording = { ...recording, deletedAt: '2026-01-05T00:00:00Z' }
      return Promise.resolve(jsonResponse(null, 204))
    }

    const restoreMatch = /^\/api\/recordings\/(\d+)\/restore$/.exec(url.pathname)
    if (restoreMatch && method === 'POST') {
      if (restoreResponse) return Promise.resolve(restoreResponse())
      const id = Number(restoreMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      const { deletedAt: _deletedAt, ...rest } = recording
      recording = rest
      return Promise.resolve(jsonResponse(null, 204))
    }

    const purgeMatch = /^\/api\/recordings\/(\d+)\/purge$/.exec(url.pathname)
    if (purgeMatch && method === 'POST') {
      const id = Number(purgeMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      // 完全削除予約後の再取得では tombstone が 404 になる API 契約を模す。
      recording = null
      return Promise.resolve(jsonResponse(null, 204))
    }

    const encodeMatch = /^\/api\/recordings\/(\d+)\/encode-profiles$/.exec(url.pathname)
    if (encodeMatch && method === 'POST') {
      if (encodePostResponse) return Promise.resolve(encodePostResponse())
      const id = Number(encodeMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      const body: { profiles?: string[] } = init?.body ? JSON.parse(String(init.body)) : {}
      recording = {
        ...recording,
        encodeProfiles: [...(recording.encodeProfiles ?? []), ...(body.profiles ?? [])],
      }
      return Promise.resolve(jsonResponse(null, 204))
    }

    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse([]))
    }

    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  })

  globalThis.fetch = fetchMock as unknown as typeof fetch
  return { fetchMock }
}

function renderAt(path: string) {
  window.scrollTo = vi.fn()
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { queryClient, router }
}

describe('RecordingDetailPage', () => {
  // 受け入れ基準: /recordings/{id} で録画単体が開き、再生・操作が機能する
  // （issue #232。issue #311 で一覧のインライン展開を廃止したため、ここが唯一の着地先）。
  it('通常の録画は再生・サムネイル・原本リンク・削除操作が出る', async () => {
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(document.querySelector('img[src="/api/recordings/3/thumbnail"]')).toBeInTheDocument()
    // issue #236（M7-3）: ダウンロード / VLC リンクは押す前にサイズを常置する
    expect(screen.getByRole('link', { name: /ダウンロード \/ VLC \(976\.6 KB\)/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'ごみ箱へ' })).toBeInTheDocument()
  })

  it('詳細を開いただけでは video の再生を開始しない（.play() を呼ばない）', async () => {
    const playSpy = vi
      .spyOn(window.HTMLMediaElement.prototype, 'play')
      .mockResolvedValue(undefined)
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }] }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('region', { name: '再生' })
    expect(playSpy).not.toHaveBeenCalled()
  })

  it('存在しない id は「録画が見つかりません」を表示する', async () => {
    createFakeServer({ recording: null })

    renderAt('/recordings/999999')

    expect(await screen.findByText('録画が見つかりません')).toBeInTheDocument()
  })

  // ごみ箱の録画も 200 で返る（getRecording の openapi.yaml description の決定）
  // が、単体ページでは再生系を一切出さない（M3-18）。encodedAssets /
  // sizeBytes を敢えて持たせても出ないことを見て、判定が deletedAt の有無で
  // 効いていることを確かめる。
  it('ごみ箱の録画は 200 で開くが再生系を一切出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-02T00:00:00Z',
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        sizeBytes: 1_000_000,
      }),
    })

    renderAt('/recordings/3')

    // 展開内容（削除日時）が出るまで待ってから「無い」ことを確認する
    // （クエリ未解決のうちに queryBy で通ってしまう空虚な成功を避ける）
    await screen.findByText('削除日時')

    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
    expect(document.querySelector('img')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /ダウンロード \/ VLC/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  // 単体ページ固有の経路: 単体ページ自身のクエリキー（recordingDetailQueryKey）
  // は先頭要素を一覧と同じ '/api/recordings' に揃えてあるので、RecordingActions
  // の invalidate（'/api/recordings' 前方一致）がこのページのキャッシュも
  // 自動的に巻き込む。ここが効いていなければ、削除してもこの画面は古い
  // （生きている）表示のまま固まる。
  it('ごみ箱へ移すと、ナビゲーションなしで自分自身が再生系無しの表示に更新される', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('region', { name: '再生' })

    await user.click(screen.getByRole('button', { name: 'ごみ箱へ' }))

    await waitFor(() => expect(screen.getByText('復元')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
  })

  // 回帰テスト（issue #232 のレビューで実機再現）: RecordingDetail の下には
  // 削除系（RecordingActions）だけでなく事後エンコード追加
  // （AddEncodeProfilesAction）というもう 1 人の mutater がいる。単体ページの
  // 再検証を「一覧の invalidate に前方一致するクエリキー」で構造的に解決して
  // いれば、AddEncodeProfilesAction 側を一切変更しなくてもここが効くはず ---
  // それを確かめる。
  it('事後エンコードを依頼すると、単体ページ自身が再検証され「追加済み」が出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000 }), // encodeProfiles 未指定 = 追加済み無し
      encodeProfiles: [{ name: 'web' }],
    })

    renderAt('/recordings/3')

    const checkbox = await screen.findByRole('checkbox', { name: 'web' })
    await user.click(checkbox)
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    // 依頼が成功した（トースト）だけでは何も主張していない --- 単体ページ自身の
    // クエリが再検証され、「追加済み: web」が実際に描画されることを見る。
    expect(await screen.findByText('エンコードを依頼しました')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText('追加済み: web')).toBeInTheDocument())
    // 追加済みになった選択肢はチェックボックスの一覧から消える（二重依頼に見せない）。
    expect(screen.queryByRole('checkbox', { name: 'web' })).not.toBeInTheDocument()
  })
})

// 一覧の常時「再生」列とインライン展開を廃し、視聴・削除・エンコードは詳細ページに
// 寄せた（issue #311）。従来これらは録画一覧の行展開で試していたが、展開が無くなった
// ので、共有部品（RecordingActions / AddEncodeProfilesAction / RuleSection /
// RecordingPlayer）のテストは唯一の呼び出し元になった詳細ページへ移した。
describe('RecordingDetailPage 削除・復元のトースト (issue #297)', () => {
  it('ごみ箱へ移すと Undo 付きトーストが出て、「元に戻す」でライブラリ表示に戻る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    await user.click(await screen.findByRole('button', { name: 'ごみ箱へ' }))

    // 自己 invalidate（recordingDetailQueryKey の前方一致）で trash 表示に変わる
    await screen.findByRole('button', { name: '復元' })
    // ごみ箱送りは復元で即座に取り消せる安価な操作なので Undo 付きトーストにする。
    expect(await screen.findByText('ごみ箱に移しました')).toBeInTheDocument()

    // Undo（復元）でライブラリ表示（再生・ごみ箱へ）に戻る
    await user.click(screen.getByRole('button', { name: '元に戻す' }))
    expect(await screen.findByRole('button', { name: 'ごみ箱へ' })).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
  })

  it('復元しても成功トーストは出ない（効果は表示の切替で常に見える）', async () => {
    const user = userEvent.setup()
    createFakeServer({ recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }) })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: '復元' }))

    await waitFor(() => expect(screen.getByRole('button', { name: 'ごみ箱へ' })).toBeInTheDocument())
    expect(screen.queryByText('復元しました')).not.toBeInTheDocument()
  })

  it('ごみ箱へ移す操作が失敗すれば失敗トーストは出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording(),
      deleteResponse: () => jsonResponse({ error: 'server error' }, 500),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: 'ごみ箱へ' }))

    expect(await screen.findByText('削除に失敗しました')).toBeInTheDocument()
    // 失敗したので表示は変わらない
    expect(screen.getByRole('button', { name: 'ごみ箱へ' })).toBeInTheDocument()
  })

  it('復元操作が失敗すれば失敗トーストは出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
      restoreResponse: () => jsonResponse({ error: 'server error' }, 500),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: '復元' }))

    expect(await screen.findByText('復元に失敗しました')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  // 完全削除（purge）は破壊的で、issue #311 以降は詳細ページからしか到達できない。
  // 確認ダイアログを挟み、確定するまで purge を呼ばないことを固定する。
  it('「今すぐ完全削除」は確認ダイアログを挟み、確定するまで purge を呼ばない', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
    })

    renderAt('/recordings/3')

    const purgeCalls = () =>
      fetchMock.mock.calls.filter((c) => String(c[0]).includes('/purge'))

    await user.click(await screen.findByRole('button', { name: '今すぐ完全削除' }))
    // ボタンを押しただけでは purge は飛ばない（確認を挟む）
    expect(purgeCalls()).toHaveLength(0)

    // ダイアログの確定ボタンで初めて purge が飛ぶ
    await user.click(await screen.findByRole('button', { name: '完全削除を予約する' }))
    await waitFor(() => expect(purgeCalls()).toHaveLength(1))
    expect(await screen.findByText('完全削除を予約しました')).toBeInTheDocument()
    // invalidate 後の単体 GET が 404 になり、詳細表示も消える
    expect(await screen.findByText('録画が見つかりません')).toBeInTheDocument()
    // 確定後はダイアログが閉じる
    expect(screen.queryByRole('button', { name: '完全削除を予約する' })).not.toBeInTheDocument()
  })
})

describe('RecordingDetailPage サイズが取れない資産（値札、issue #236）', () => {
  it('encoded 資産の sizeBytes が省略されていても、プロファイル名は出るがサイズは出さない', async () => {
    createFakeServer({ recording: sampleRecording({ encodedAssets: [{ profile: 'web' }] }) })

    renderAt('/recordings/3')

    const region = await screen.findByRole('region', { name: '再生' })
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(within(region).getByText('web')).toBeInTheDocument()
    expect(region.textContent).not.toMatch(/\d+(\.\d+)? (B|KB|MB|GB|TB)/)
  })
})

// 事後追加のエンコード依頼（issue #133、凍結の例外）。原本の有無 / 追加済みの
// 除外 / 409 翻訳 / ごみ箱で出さない、をそれぞれ固定する（送信成功は上の
// 「事後エンコードを依頼すると…」で確認済み）。
describe('RecordingDetailPage 事後エンコード追加 (AddEncodeProfilesAction)', () => {
  it('encodeProfiles（desired）にあるものは選択肢から外し「追加済み」に出す', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }, { name: 'h265' }],
    })

    renderAt('/recordings/3')

    expect(await screen.findByText('事後エンコードの追加')).toBeInTheDocument()
    expect(screen.getByText('追加済み: h264')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'h264' })).not.toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'h265' })).toBeInTheDocument()
  })

  it('全プロファイルが追加済みなら、選択肢もボタンも出さず案内だけ出す', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    expect(await screen.findByText('すべてのエンコードプロファイルが追加済みです。')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '追加エンコードを依頼' })).not.toBeInTheDocument()
  })

  it('原本が無い（sizeBytes 省略）録画では、削除済みと断定せず中立文言を出し、チェックボックスを出さない', async () => {
    createFakeServer({
      recording: sampleRecording({ encodeProfiles: [] }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    expect(
      await screen.findByText(
        'この録画には再生可能な原本がありません。追加のエンコードは依頼できません。',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText(/削除済み/)).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  // issue #271: 409 は「原本が active でない」の hedge 文言で、サーバーは英語のまま
  // 返す。`hasOriginal` の近似が破れて 409 になっても、英語文字列を出さず日本語に
  // 翻訳することを固定する。
  it('409 応答は英語のサーバー文字列を出さず、日本語の文言に翻訳する', async () => {
    const user = userEvent.setup()
    const rawEnglishMessage =
      'original media asset not active (deleted, deleting, or not yet ingested); cannot add encode profiles'
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 500, encodeProfiles: [] }),
      encodeProfiles: [{ name: 'h264' }],
      encodePostResponse: () => jsonResponse({ error: rawEnglishMessage }, 409),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('checkbox', { name: 'h264' }))
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    expect(
      await screen.findByText(
        '原本の状態が変わったため追加できませんでした（削除済み・削除処理中・未取り込みのいずれか）。画面を更新してから再度お試しください。',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText(rawEnglishMessage)).not.toBeInTheDocument()
    expect(screen.queryByText(/deleted, deleting, or not yet ingested/)).not.toBeInTheDocument()
  })

  it('ごみ箱では追加エンコードのコントロールを一切出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-05T00:00:00Z',
        sizeBytes: 500,
        encodeProfiles: [],
      }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    await screen.findByText('削除日時')
    expect(screen.queryByText('事後エンコードの追加')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })
})

// issue #230（M6-2）: 録画 → ルールの導線。ruleId の有無で出し分け、ルール一覧に
// まだ ruleId が載っていない一時的な状態でも #N に落ちて壊れないことを固定する。
describe('RecordingDetailPage ルール導線 (issue #230)', () => {
  it('ruleId がある録画は「ルール」セクションを出し、ルール名がリンクになる', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rules: [sampleRule({ id: 5, name: 'ニュース全部' })],
    })

    renderAt('/recordings/3')

    expect(await screen.findByRole('heading', { name: 'ルール', level: 4 })).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: 'ニュース全部' })).toHaveAttribute(
      'href',
      '/search?ruleId=5',
    )
    expect(screen.getByRole('link', { name: 'このルールの録画で絞る' })).toHaveAttribute(
      'href',
      '/recordings?ruleId=5',
    )
  })

  it('ruleId が無い録画には「ルール」セクションを出さない（手動予約由来）', async () => {
    createFakeServer({ recording: sampleRecording({ source: 'manual' }) })

    renderAt('/recordings/3')

    // 種別（必ず出る dt）が出るまで待ってから「無い」ことを確認する
    await screen.findByText('種別')
    expect(screen.queryByRole('heading', { name: 'ルール', level: 4 })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'このルールの録画で絞る' })).not.toBeInTheDocument()
  })

  // ルール一覧にその id が無い（新規作成直後でキャッシュが追いついていない等の
  // 一時的な状態。ルール削除では FK の ON DELETE SET NULL で ruleId 自体が省略され
  // セクションごと消えるので、これは削除の経路ではない）。`rules.find` が空を
  // 返す間は #N 表記に落ちる。
  it('ルール一覧にまだ載っていない ruleId でも #N 表記に落ちて壊れない', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 99, source: 'rule' }),
      rules: [sampleRule({ id: 1, name: '既知のルール' })],
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: 'ルール', level: 4 })
    expect(await screen.findByRole('link', { name: '#99' })).toHaveAttribute(
      'href',
      '/search?ruleId=99',
    )
  })

  it('ルール一覧が未解決の間は #N を出し、解決後にルール名へ差し替わる', async () => {
    let resolveRules!: (response: Response) => void
    const pendingRules = new Promise<Response>((resolve) => {
      resolveRules = resolve
    })
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rulesResponse: () => pendingRules,
    })

    renderAt('/recordings/3')

    // 録画単体は解決済みだがルール一覧は未解決の状態を明示的に作る。
    await screen.findByText('単体ページの録画')
    expect(screen.getByRole('link', { name: '#5' })).toHaveAttribute('href', '/search?ruleId=5')

    resolveRules(jsonResponse([sampleRule({ id: 5, name: '後から出るルール' })]))

    expect(await screen.findByRole('link', { name: '後から出るルール' })).toHaveAttribute(
      'href',
      '/search?ruleId=5',
    )
    expect(screen.queryByRole('link', { name: '#5' })).not.toBeInTheDocument()
  })
})
