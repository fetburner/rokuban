import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { getListReservationsQueryKey } from '@/api/generated'
import type { EncodeProfileSummary, Reservation, Rule, RuleInput, Service } from '@/api/generated'
import { summarizeRuleConditions } from '@/components/rule-condition-summary'
import { RulesPage } from '@/pages/rules'
import { renderInRouter } from '@/test/router'

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

function sampleReservation(
  id: number,
  ruleId: number,
  state: Reservation['state'] = 'active',
): Reservation {
  return {
    id,
    site: 'default',
    programId: 1000 + id,
    source: 'rule',
    ruleId,
    state,
    title: `番組 ${id}`,
    serviceName: 'NHK総合',
    channelType: 'GR',
    startAt: '2026-09-01T00:00:00Z',
    durationMs: 1_800_000,
    createdAt: '2026-08-01T00:00:00Z',
    updatedAt: '2026-08-01T00:00:00Z',
    skip: false,
  }
}

/**
 * ruleWithConditions は条件・UI を持たない項目の両方を埋めたルール。
 * 「復元される」「落ちない」を確認するテストの共通フィクスチャ。
 */
const ruleWithConditions: Rule = {
  id: 2,
  name: '平日ニュース',
  enabled: true,
  priority: 5,
  keepOriginal: 'always',
  encodeProfiles: [],
  textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース', negate: false }],
  genres: [1],
  channelTypes: ['GR'],
  times: [{ weekdays: 31, startSec: 75600, endSec: 82800 }],
  durationMinMs: 1_800_000,
  // UI を持たない項目（preserve が引き継ぐべきもの）
  dedupeEnabled: true,
  dedupeThreshold: 0.8,
  dedupeWindowSeconds: 3600,
  filenameTemplate: '{title}',
  metadata: { source: 'legacy' },
  createdAt: '2026-07-01T00:00:00Z',
  updatedAt: '2026-07-01T00:00:00Z',
}

const profiles: EncodeProfileSummary[] = [{ name: 'h264' }, { name: 'hevc' }]

const services: Service[] = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
]

function stubApi(
  initialRules: Rule[] = [sampleRule],
  // 削除 API が返す内訳。既定は 0 件（大半のテストは内訳に関心が無い）。
  deleteImpact: { deletedReservations: number; detachedReservations: number } = {
    deletedReservations: 0,
    detachedReservations: 0,
  },
  // 作成・更新・削除を意図的に失敗させる（issue #297: 無音化した成功トーストの
  // 反対側 --- 失敗トーストは従来どおり出ることを確認するため）。既定では
  // 失敗させない。
  failures: { create?: number; update?: number; delete?: number; reservations?: number } = {},
  // 行のスイッチ（無効化）が確認に出す件数の母集団。RulesPage は予約一覧と
  // 同じクエリキーで GET /api/reservations を読む。既定は空。
  reservations: Reservation[] = [],
  // `GET /api/sites` の応答（既定 `['default']`）。`<ConditionFields>` の
  // サイトチップは 2 つ以上のときだけ出るので、それを確かめるテストだけが
  // 2 つ目以降を足す（issue #531）。
  siteNames: string[] = ['default'],
) {
  const putBodies: { id: number; body: RuleInput }[] = []
  const postBodies: RuleInput[] = []
  const deletedIds: number[] = []
  // 作成・更新を状態に反映する --- invalidate 後の再取得で「新しい行が
  // 一覧に現れる」「更新後の内容が一覧に反映される」ことを確認するテストのため
  // （GET のたびに現在の状態を返す）。
  let state = [...initialRules]

  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/rules' && method === 'GET') {
      return Promise.resolve(jsonResponse(state.filter((r) => !deletedIds.includes(r.id))))
    }
    if (url.pathname === '/api/reservations' && method === 'GET') {
      if (failures.reservations !== undefined) {
        return Promise.resolve(
          jsonResponse({ error: '予約数を取得できませんでした' }, failures.reservations),
        )
      }
      return Promise.resolve(jsonResponse(reservations))
    }
    if (url.pathname === '/api/rules' && method === 'POST') {
      if (failures.create !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが作成を拒否しました' }, failures.create))
      }
      const body = JSON.parse(String(init?.body)) as RuleInput
      postBodies.push(body)
      const created: Rule = {
        ...body,
        id: 99,
        createdAt: '2026-08-01T00:00:00Z',
        updatedAt: '2026-08-01T00:00:00Z',
        // Rule は priority/keepOriginal/enabled を必須にするが RuleInput は
        // 任意（サーバー側の既定値埋めを前提にした契約）なので、フェイクでも
        // 同じ既定値をここで埋める。
        priority: body.priority ?? 0,
        keepOriginal: body.keepOriginal ?? 'always',
        enabled: body.enabled ?? true,
      }
      state = [...state, created]
      return Promise.resolve(jsonResponse(created))
    }
    const putMatch = /^\/api\/rules\/(\d+)$/.exec(url.pathname)
    if (putMatch && method === 'PATCH') {
      if (failures.update !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが更新を拒否しました' }, failures.update))
      }
      const id = Number(putMatch[1])
      const body = JSON.parse(String(init?.body)) as RuleInput
      putBodies.push({ id, body })
      const existing = state.find((r) => r.id === id)
      const updated: Rule = {
        ...body,
        id,
        createdAt: existing?.createdAt ?? '2026-07-01T00:00:00Z',
        updatedAt: '2026-08-01T00:00:00Z',
        priority: body.priority ?? 0,
        keepOriginal: body.keepOriginal ?? 'always',
        enabled: body.enabled ?? true,
      }
      state = state.map((r) => (r.id === id ? updated : r))
      return Promise.resolve(jsonResponse(updated))
    }
    if (putMatch && method === 'DELETE') {
      if (failures.delete !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが削除を拒否しました' }, failures.delete))
      }
      const id = Number(putMatch[1])
      deletedIds.push(id)
      return Promise.resolve(jsonResponse({ id, ...deleteImpact }))
    }
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(profiles))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // 条件フォームのサービス選択肢は全 site から作る（issue #290）ので、
    // 単一サイト構成でも `GET /api/sites` を経由する。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(siteNames))
    const servicesMatch = /^\/api\/sites\/([^/]+)\/services$/.exec(url.pathname)
    if (servicesMatch && siteNames.includes(servicesMatch[1])) {
      return Promise.resolve(jsonResponse(services))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch

  return { postBodies, putBodies, deletedIds }
}

function renderPage() {
  return renderInRouter(<RulesPage />)
}

async function findCreateRuleButton() {
  const content = await screen.findByTestId('bounded-page-content')
  return within(content).findByRole('button', { name: 'ルールを作成' })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('summarizeRuleConditions', () => {
  it('条件が無ければ空配列を返す', () => {
    expect(summarizeRuleConditions(sampleRule)).toEqual([])
  })

  it('テキスト・ジャンル・時間帯を要約する', () => {
    const summary = summarizeRuleConditions(ruleWithConditions)
    expect(summary).toContain('番組名に「ニュース」を含む')
    expect(summary).toContain('スポーツ')
    expect(summary).toContain('月〜金 21:00–23:00')
    expect(summary).toContain('30分以上')
  })

  // 検索・ルール条件の「サービス」は利用者向けには「チャンネル」で統一する
  // （API の `services` フィールド名自体は変えない）。
  it('サービス条件はチャンネル件数の要約になる', () => {
    const rule: Rule = {
      ...sampleRule,
      services: [
        { networkId: 1, serviceId: 1 },
        { networkId: 1, serviceId: 2 },
      ],
    }
    expect(summarizeRuleConditions(rule)).toContain('チャンネル 2 件')
  })
})

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

describe('RulesPage 条件編集', () => {
  it('新規作成で入力した条件が RuleInput に入る', async () => {
    const { postBodies } = stubApi([])
    const user = userEvent.setup()
    renderPage()

    // ルールが 0 件でも作成フォームは開ける。ルーターの初回描画は非同期なので
    // 最初の操作対象は findBy* で待つ
    await user.click(await findCreateRuleButton())

    await user.type(screen.getByLabelText('名前'), 'テストルール')

    // テキスト条件（1 行目は「条件を追加」を押さなくても常に編集できる。
    // issue #305。押すと 2 行目が増え、その値が空のまま保存できなくなる）
    await user.type(screen.getByLabelText('テキスト条件 1 の値'), 'ニュース')

    // ジャンル（サービス一覧の読み込みを待ってから操作する）
    await screen.findByText('NHK総合')
    await user.click(screen.getByRole('button', { name: 'スポーツ' }))

    // 時間帯（<input type="time"> は jsdom で userEvent.type の逐次キー入力を
    // 素直に受け付けないため、change イベントで直接値を入れる）
    await user.click(screen.getByRole('button', { name: '時間帯を追加' }))
    const startInput = screen.getByLabelText('時間帯 1 の開始')
    const endInput = screen.getByLabelText('時間帯 1 の終了')
    fireEvent.change(startInput, { target: { value: '21:00' } })
    fireEvent.change(endInput, { target: { value: '23:00' } })

    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(postBodies.length).toBe(1))
    const body = postBodies[0]
    expect(body.name).toBe('テストルール')
    expect(body.textMatches).toEqual([
      { target: 'name', mode: 'keyword', value: 'ニュース' },
    ])
    expect(body.genres).toEqual([1])
    expect(body.times).toEqual([{ weekdays: 127, startSec: 75600, endSec: 82800 }])
  })

  /**
   * `ConditionFields` は検索画面とルール画面が共有するので、そこに置く文言は
   * どちらの画面でも事実として正しくなければならない。検索向けの動詞
   * （「検索すると」）を入れると、条件ゼロで**保存**すると全番組を録り続ける
   * ルール画面では誤りになる（実際に一度入り込んで差し戻された）。
   */
  it('ルール作成フォームでも条件なしの意味が画面に依存しない文言で出る', async () => {
    stubApi([])
    const user = userEvent.setup()
    renderPage()

    await user.click(await findCreateRuleButton())

    // テキスト条件・時間帯のどちらも「指定なし（すべての…が対象）」の形で、
    // 画面ごとの動詞（検索する / 保存する）を含まない
    expect(screen.getByText('指定なし（すべての番組が対象）')).toBeInTheDocument()
    expect(screen.getByText('指定なし（すべての時間帯が対象）')).toBeInTheDocument()
    expect(screen.queryByText(/検索すると/)).not.toBeInTheDocument()
  })

  it('1 行目の値を全消しして条件ゼロに戻すと確認ダイアログを挟む', async () => {
    const { postBodies } = stubApi([])
    const user = userEvent.setup()
    renderPage()

    await user.click(await findCreateRuleButton())
    await user.type(screen.getByLabelText('名前'), '条件なしルール')
    const textInput = screen.getByLabelText('テキスト条件 1 の値')
    await user.type(textInput, 'ニュース')
    await user.clear(textInput)

    // 全消しした 1 行目は条件として残らないので、保存は確認ダイアログを開ける。
    const saveButton = screen.getByRole('button', { name: '保存' })
    expect(saveButton).not.toBeDisabled()
    await user.click(saveButton)
    expect(await screen.findByText('条件を指定せずに保存しますか？')).toBeInTheDocument()
    expect(postBodies.length).toBe(0)

    // キャンセルすると閉じるだけで送信されない。フォームも編集可能なまま
    // 残る（半端な送信状態にならない --- 「保存中…」のまま固まったり、
    // 保存ボタンが disabled のまま残って連打すらできない、ということがない）。
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    await waitFor(() =>
      expect(screen.queryByText('条件を指定せずに保存しますか？')).not.toBeInTheDocument(),
    )
    expect(postBodies.length).toBe(0)
    expect(saveButton).not.toBeDisabled()
    expect(saveButton).toHaveTextContent('保存')

    // 再度保存を押すと同じ確認が出て、今度は確定すると送信される
    await user.click(saveButton)
    await user.click(await screen.findByRole('button', { name: '保存する' }))
    await waitFor(() => expect(postBodies.length).toBe(1))
  })

  it('編集で既存ルールの条件がフォームに復元される', async () => {
    stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))

    const valueInput = await screen.findByLabelText<HTMLInputElement>('テキスト条件 1 の値')
    expect(valueInput.value).toBe('ニュース')

    const startInput = screen.getByLabelText<HTMLInputElement>('時間帯 1 の開始')
    expect(startInput.value).toBe('21:00')
    const endInput = screen.getByLabelText<HTMLInputElement>('時間帯 1 の終了')
    expect(endInput.value).toBe('23:00')

    // ジャンル（1 = スポーツ）が選択済みチップとして復元される
    const genreGroup = screen.getByRole('group', { name: 'ジャンル' })
    expect(within(genreGroup).getByRole('button', { name: 'スポーツ' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
  })

  it('編集で一部の条件だけ変えても他の条件が落ちない', async () => {
    const { putBodies } = stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))

    // ジャンルだけ増やす（テキスト条件・時間帯には触れない）
    await screen.findByLabelText('テキスト条件 1 の値')
    await user.click(screen.getByRole('button', { name: 'ドラマ' }))

    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(putBodies.length).toBe(1))
    const body = putBodies[0].body
    expect(body.textMatches).toEqual([
      { target: 'name', mode: 'keyword', value: 'ニュース' },
    ])
    expect(body.times).toEqual([{ weekdays: 31, startSec: 75600, endSec: 82800 }])
    expect(body.genres).toEqual([1, 3])
  })

  it('編集保存時に UI を持たない項目（dedupe* / filenameTemplate / metadata）が落ちない', async () => {
    const { putBodies } = stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))
    await screen.findByLabelText('テキスト条件 1 の値')

    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(putBodies.length).toBe(1))
    const body = putBodies[0].body
    expect(body.dedupeEnabled).toBe(true)
    expect(body.dedupeThreshold).toBe(0.8)
    expect(body.dedupeWindowSeconds).toBe(3600)
    expect(body.filenameTemplate).toBe('{title}')
    expect(body.metadata).toEqual({ source: 'legacy' })
  })

  /**
   * issue #531:「検索・ルールは同じ条件 UI を双方向に共有する」ので、
   * `/rules` の編集フォームでもサイトチップの復元・編集・往復を確かめる
   * （`pages/search.test.tsx` の対になるテスト）。
   */
  it('サイトチップは rule_sites から復元され、編集して保存すると変更後の sites が運ばれる', async () => {
    const ruleWithSites: Rule = { ...ruleWithConditions, sites: ['default', 'site2'] }
    const { putBodies } = stubApi([ruleWithSites], undefined, undefined, undefined, [
      'default',
      'site2',
    ])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))
    await screen.findByLabelText('テキスト条件 1 の値')

    const group = screen.getByRole('group', { name: 'サイト' })
    const defaultChip = within(group).getByRole('button', { name: 'default' })
    const site2Chip = within(group).getByRole('button', { name: 'site2' })
    expect(defaultChip).toHaveAttribute('aria-pressed', 'true')
    expect(site2Chip).toHaveAttribute('aria-pressed', 'true')

    await user.click(site2Chip)
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(putBodies.length).toBe(1))
    expect(putBodies[0].body.sites).toEqual(['default'])
  })

  it('一覧に条件の要約が出て、空のルールは「すべての番組」と分かる', async () => {
    stubApi([sampleRule, ruleWithConditions])
    renderPage()

    await screen.findByText('ニュース')
    expect(screen.getByText('条件なし（すべての番組にマッチ）')).toBeInTheDocument()
    expect(screen.getByText('番組名に「ニュース」を含む')).toBeInTheDocument()
  })

  it('無効なルールに「無効」バッジが出る', async () => {
    stubApi([{ ...sampleRule, enabled: false }])
    renderPage()

    await screen.findByText('ニュース')
    const badge = screen.getByText('無効')
    expect(badge).toBeInTheDocument()
    // text-muted-foreground だと bg-muted との合成後コントラストがライトで
    // 4.5 を割る（issue #308）。jsdom は色を測れないので、退行防止としては
    // クラス名のリテラル比較まで（実測は e2e:design の担当）。
    expect(badge.className).toContain('text-foreground')
    expect(badge.className).not.toContain('text-muted-foreground')
  })

  it('「検索しながら編集」リンクが /search?ruleId=<id> を指す', async () => {
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    const link = screen.getByRole('link', { name: '検索しながら編集' })
    expect(link).toHaveAttribute('href', '/search?ruleId=2')
  })

  // issue #137: ルールから、そのルール由来の録画だけに絞った一覧への導線。
  // 条件モデルを検索と共有しないため、遷移先は /search ではなく /recordings。
  it('「このルールの録画」リンクが /recordings?ruleId=<id> を指す', async () => {
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    const link = screen.getByRole('link', { name: 'このルールの録画' })
    expect(link).toHaveAttribute('href', '/recordings?ruleId=2')
  })
})

describe('RulesPage ルールの有効スイッチ', () => {
  it('状態を aria-checked に出し、無効化だけ active 予約数付きの確認を挟む', async () => {
    const reservations: Reservation[] = [
      sampleReservation(1, 1),
      sampleReservation(2, 1),
      sampleReservation(3, 1, 'detached'),
      sampleReservation(4, 2),
      { ...sampleReservation(5, 1), source: 'manual' },
    ]
    const { putBodies } = stubApi([sampleRule], undefined, {}, reservations)
    const user = userEvent.setup()
    const { queryClient } = renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    await user.click(toggle)

    expect(
      await screen.findByText(
        '「ニュース」を無効にすると、このルールによる予約 2 件が取り消されます。手動で予約したものは残ります。',
      ),
    ).toBeInTheDocument()
    expect(putBodies).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: '無効にする' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    // PATCH は RuleInput.name が必須で全置換する契約なので、編集フォームと同じ
    // 入力を送りつつ enabled だけを変更する。
    expect(putBodies[0]).toMatchObject({ id: 1, body: { name: 'ニュース', enabled: false } })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    // 無効化は予約一覧を invalidate するが、ruler の次回評価までは同じ予約が
    // キャッシュに残る。即座に行が消えることは期待しない。
    await waitFor(() =>
      expect(
        queryClient.getQueryState(getListReservationsQueryKey())?.isInvalidated,
      ).toBe(true),
    )
    expect(
      queryClient.getQueryData<{ data: Reservation[] }>(getListReservationsQueryKey())?.data,
    ).toEqual(reservations)
  })

  it('予約一覧は無効化を押すまで取得せず、取得完了前に 0 件の確認を出さない', async () => {
    stubApi([sampleRule])
    const baseFetch = globalThis.fetch
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/reservations') {
        return new Promise<Response>((resolve) => {
          setTimeout(() => resolve(jsonResponse([])), 50)
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    const reservationCalls = () =>
      (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/reservations',
      )
    expect(reservationCalls()).toHaveLength(0)

    await user.click(toggle)
    expect(reservationCalls()).toHaveLength(1)
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()

    expect(await screen.findByText(/を無効にすると/)).toBeInTheDocument()
  })

  it('予約一覧の取得に失敗したら件数確認を開かず、API 本文をトーストに出す', async () => {
    stubApi([sampleRule], undefined, { reservations: 500 })
    const user = userEvent.setup()
    renderPage()

    await user.click(
      await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' }),
    )

    expect(await screen.findByText('予約数を取得できませんでした')).toBeInTheDocument()
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()
  })

  it('有効化は確認を挟まず、PATCH の応答前に状態を楽観更新する', async () => {
    stubApi([{ ...sampleRule, enabled: false }])
    const baseFetch = globalThis.fetch
    let updated = false
    let resolveUpdate: (() => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/rules' && (init?.method ?? 'GET') === 'GET' && updated) {
        return Promise.resolve(jsonResponse([{ ...sampleRule, enabled: true }]))
      }
      if (url.pathname === '/api/rules/1' && init?.method === 'PATCH') {
        return new Promise<Response>((resolve) => {
          resolveUpdate = () => {
            updated = true
            resolve(jsonResponse({ ...sampleRule, enabled: true }))
          }
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    await user.click(toggle)

    await waitFor(() => expect(resolveUpdate).toBeDefined())
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()
    expect(toggle).toHaveAttribute('aria-checked', 'true')

    const updateCall = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'PATCH',
    )
    if (updateCall === undefined) throw new Error('PATCH was not recorded')
    expect(JSON.parse(String((updateCall[1] as RequestInit).body))).toMatchObject({
      name: 'ニュース',
      enabled: true,
    })
    if (resolveUpdate === undefined) throw new Error('PATCH was not started')
    resolveUpdate()
    await waitFor(() => {
      const ruleGets = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/rules' &&
          ((call[1] as RequestInit | undefined)?.method ?? 'GET') === 'GET',
      )
      expect(ruleGets.length).toBeGreaterThanOrEqual(2)
    })
  })

  it('別ルールの更新成功後に先の更新が失敗しても、成功した行を巻き戻さない', async () => {
    const otherRule: Rule = { ...sampleRule, id: 2, name: 'スポーツ', enabled: false }
    stubApi([{ ...sampleRule, enabled: false }, otherRule])
    const baseFetch = globalThis.fetch
    let failFirstUpdate: ((response: Response) => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/rules/1' && init?.method === 'PATCH') {
        return new Promise<Response>((resolve) => {
          failFirstUpdate = resolve
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const first = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    const second = screen.getByRole('switch', { name: 'ルール「スポーツ」を有効にする' })

    await user.click(first)
    await waitFor(() => expect(failFirstUpdate).toBeDefined())
    await user.click(second)
    await waitFor(() => {
      const ruleGets = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/rules' &&
          ((call[1] as RequestInit | undefined)?.method ?? 'GET') === 'GET',
      )
      expect(ruleGets.length).toBeGreaterThanOrEqual(2)
    })

    if (failFirstUpdate === undefined) throw new Error('first PATCH was not started')
    failFirstUpdate(jsonResponse({ error: '先の更新に失敗しました' }, 500))

    expect(await screen.findByText('先の更新に失敗しました')).toBeInTheDocument()
    await waitFor(() => expect(first).toHaveAttribute('aria-checked', 'false'))
    expect(second).toHaveAttribute('aria-checked', 'true')
  })

  it('予約数の取得中は全ルールのスイッチを無効にして、確認を重ねて開かせない', async () => {
    const otherRule: Rule = { ...sampleRule, id: 2, name: 'スポーツ' }
    stubApi([sampleRule, otherRule])
    const baseFetch = globalThis.fetch
    let resolveReservations: ((response: Response) => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/reservations') {
        return new Promise<Response>((resolve) => {
          resolveReservations = resolve
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const switches = await screen.findAllByRole('switch')
    await user.click(switches[0])
    await waitFor(() => expect(resolveReservations).toBeDefined())

    expect(switches[0]).toBeDisabled()
    expect(switches[1]).toBeDisabled()

    if (resolveReservations === undefined) throw new Error('reservation fetch was not started')
    resolveReservations(jsonResponse([]))
    expect(await screen.findByText(/を無効にすると/)).toBeInTheDocument()
  })

  it('更新失敗時は楽観更新を戻し、API 本文をトーストに出す', async () => {
    stubApi([{ ...sampleRule, enabled: false }], undefined, { update: 500 })
    const user = userEvent.setup()
    renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    await user.click(toggle)

    expect(await screen.findByText('サーバーが更新を拒否しました')).toBeInTheDocument()
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'false'))
  })
})

// issue #227（M5-4）: 削除（稀・破壊的）を行の overflow メニューへ寄せ、
// 編集フォームの保存・キャンセルと同格には並べない。
describe('RulesPage 削除は overflow メニュー', () => {
  it('一覧の行に「削除」ボタンが直接は出ない（overflow の中）', async () => {
    stubApi()
    renderPage()

    // 行が描画されたことを先に待ってから「無い」ことを確認する
    // （非同期の空虚な成功を避ける）。
    await screen.findByText('ニュース')
    expect(screen.queryByRole('button', { name: '削除' })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '削除' })).not.toBeInTheDocument()
  })

  it('overflow を開くと「削除」が menuitem として出て、選ぶと確認ダイアログの上で削除される', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))

    const deleteItem = await screen.findByRole('menuitem', { name: '削除' })
    await user.click(deleteItem)

    expect(await screen.findByText('ルール「ニュース」を削除しますか？')).toBeInTheDocument()
    // ダイアログを開いただけでは DELETE は飛ばない
    const deleteCallsBeforeConfirm = (
      globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    ).mock.calls.filter((call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE')
    expect(deleteCallsBeforeConfirm.length).toBe(0)

    // 取り返しがつかない操作の確定は destructive（issue #467、
    // alert-dialog.tsx の規約。variant を外すと落ちる）。
    const confirmButton = screen.getByRole('button', { name: '削除する' })
    expect(confirmButton).toHaveClass('text-destructive')

    await user.click(confirmButton)

    // 削除された行が一覧から消えることそのものが常に画面に見える
    // （RulesPage はフィルタもページングも持たない）ので、内訳が 0/0 の
    // ときは成功トーストを無音化する（issue #297）。行の消失で削除が
    // 効いたことを確認する。
    await waitFor(() => expect(screen.queryByText('ニュース')).not.toBeInTheDocument())
    expect(screen.queryByText('ルールを削除しました')).not.toBeInTheDocument()
  })

  // 削除 API の内訳（削除 N 件 / 編集済みのため残った M 件）をトーストに出す
  // （reservation-model.md §4.3「ルール削除の UX は可視化で解決する」）。
  // 残った予約は「ユーザーが自分で触ったもの」だけなので、黙って残すと
  // 一覧に見慣れないマーカー付きの行が増えた理由が分からなくなる。
  it('削除のトーストに予約の内訳（削除 / 残った件数）が出る', async () => {
    stubApi([sampleRule], { deletedReservations: 3, detachedReservations: 2 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(
      await screen.findByText('ルールを削除しました（予約 3 件を削除、2 件は編集済みのため残しました）'),
    ).toBeInTheDocument()
  })

  // detached が 0 でも削除した予約があれば内訳を出す（0 件で黙るのは
  // 「何も起きていない削除」のときだけ、という境界の反対側）。
  it('残った予約が 0 件でも、削除した予約があれば件数を出す', async () => {
    stubApi([sampleRule], { deletedReservations: 4, detachedReservations: 0 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(await screen.findByText('ルールを削除しました（予約 4 件を削除）')).toBeInTheDocument()
  })

  // issue #215: 重複排除の比較対象は「同じ rule_id の recordings」なので、
  // ルールを削除すると履歴がスコープから外れ、同じ条件で作り直しても
  // 引き継がれない（docs/recording/ruler.md §3.1）。押した後では取り返せない
  // 副作用なので、確認の時点で伝える。
  it('重複排除が有効なルールの削除確認に、履歴が外れることと「編集」への案内が出る', async () => {
    stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「平日ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))

    expect(await screen.findByText('ルール「平日ニュース」を削除しますか？')).toBeInTheDocument()
    const description = screen.getByText(/重複排除の履歴も一緒に外れます/)
    expect(description.textContent).toContain('重複排除の履歴も一緒に外れます')
    expect(description.textContent).toContain('作り直しても引き継がれない')
    expect(description.textContent).toContain('「編集」')
    // 被害の大きさを docs より強く書かない（過剰録画は一過性で、新ルールの
    // 下で 1 本録れれば以降は再び弾かれる ——
    // TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete 段階 3 の測定）。
    expect(description.textContent).toContain('1 本録れれば以降はまた弾かれます')
    expect(description.textContent).not.toContain('窓の中の再放送を録り直します')

    // 確認せずにキャンセルする（副作用の大きい操作なので、この時点では
    // まだ削除されていないことを確認する）。
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    const deleteCalls = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCalls.length).toBe(0)
  })

  // 反対方向: 重複排除を使っていないルールでは警告を出さない
  // （常に出す実装だと、無関係なルールにも意味の無い長文が付く）。
  it('重複排除が無効なルールの削除確認には履歴の警告を出さない', async () => {
    stubApi([sampleRule])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))

    await screen.findByText('ルール「ニュース」を削除しますか？')
    // 本文そのものを固定する（存在チェックだけだと、この文言が空や別の
    // 表現に変わっても気付けない --- 他の破壊的確認と同じ「取り消せません」
    // の語彙を使っていることも含めて固定する）。
    expect(
      screen.getByText('ルールの設定を削除します。取り消せません。'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/重複排除の履歴も一緒に外れます/)).not.toBeInTheDocument()
  })

  it('確認をキャンセルすると削除されない', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: 'キャンセル' }))

    // DELETE が飛んでいないことを確認する（行が残っていることでも分かるが、
    // ネットワーク呼び出しの有無を直接見て確定させる）
    await waitFor(() =>
      expect(screen.queryByText('ルール「ニュース」を削除しますか？')).not.toBeInTheDocument(),
    )
    const deleteCalls = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCalls.length).toBe(0)
    expect(screen.getByText('ニュース')).toBeInTheDocument()
  })

  it('編集フォームには削除ボタンが無い（保存・キャンセルだけが主操作）', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))

    await screen.findByLabelText('名前')
    expect(screen.queryByRole('button', { name: '削除' })).not.toBeInTheDocument()
  })
})

// issue #297: 削除・更新の効果は一覧の同じ行として画面に現れる
// （RulesPage はフィルタもページングも持たない）ので、素の成功トーストは
// 無音化する。作成は `ListRules` が `priority DESC, id ASC` で並べるため
// 新しい行がフォールドの外に入りうり、画面外になりうる効果はトーストを
// 残す（issue #297 が認める例外）。失敗は一覧からは分からない新しい情報
// なので、いずれも残す。
describe('RulesPage 成功トーストの無音化 (issue #297)', () => {
  it('作成に成功すると成功トーストが出る（新しい行は並び順次第でフォールドの外に入りうる）', async () => {
    const { postBodies } = stubApi([])
    const user = userEvent.setup()
    renderPage()

    await user.click(await findCreateRuleButton())
    await user.type(screen.getByLabelText('名前'), 'できたルール')
    await user.type(screen.getByLabelText('テキスト条件 1 の値'), 'キーワード')

    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(postBodies.length).toBe(1))

    // 新しい行が一覧に現れ、フォームが閉じて「ルールを作成」ボタンに戻る
    // こと自体は確認しつつ、その効果が画面外になりうる（優先度順で下の方に
    // 入る）ため成功トーストは残ることを確認する。
    expect(await screen.findByText('できたルール')).toBeInTheDocument()
    expect(await findCreateRuleButton()).toBeInTheDocument()
    expect(await screen.findByText('ルールを作成しました')).toBeInTheDocument()
  })

  it('更新に成功しても成功トーストは出ず、一覧の同じ行に反映される', async () => {
    const { putBodies } = stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))
    await screen.findByLabelText('テキスト条件 1 の値')

    const nameInput = screen.getByLabelText('名前')
    await user.clear(nameInput)
    await user.type(nameInput, '改名した平日ニュース')
    await user.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(putBodies.length).toBe(1))

    expect(await screen.findByText('改名した平日ニュース')).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: '編集' })).toBeInTheDocument()
    expect(screen.queryByText('ルールを更新しました')).not.toBeInTheDocument()
  })

  it('作成に失敗すれば失敗トーストは出る（フォームも開いたまま残る）', async () => {
    stubApi([], undefined, { create: 500 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await findCreateRuleButton())
    await user.type(screen.getByLabelText('名前'), '失敗するルール')
    await user.type(screen.getByLabelText('テキスト条件 1 の値'), 'キーワード')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByText('サーバーが作成を拒否しました')).toBeInTheDocument()
    // 失敗時はフォームが送信前のまま残る（半端な状態で消えない）
    expect(screen.getByLabelText('名前')).toBeInTheDocument()
  })

  it('更新に失敗すれば失敗トーストは出る', async () => {
    stubApi([ruleWithConditions], undefined, { update: 500 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: '編集' }))
    await screen.findByLabelText('テキスト条件 1 の値')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByText('サーバーが更新を拒否しました')).toBeInTheDocument()
  })

  it('削除に失敗すれば失敗トーストは出て、行は一覧に残る', async () => {
    stubApi([sampleRule], undefined, { delete: 500 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(await screen.findByText('サーバーが削除を拒否しました')).toBeInTheDocument()
    expect(screen.getByText('ニュース')).toBeInTheDocument()
  })
})
