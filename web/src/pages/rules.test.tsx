import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { EncodeProfileSummary, Rule, RuleInput, Service } from '@/api/generated'
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

function stubApi(rules: Rule[] = [sampleRule]) {
  const putBodies: { id: number; body: RuleInput }[] = []
  const postBodies: RuleInput[] = []

  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/rules' && method === 'GET') {
      return Promise.resolve(jsonResponse(rules))
    }
    if (url.pathname === '/api/rules' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as RuleInput
      postBodies.push(body)
      return Promise.resolve(
        jsonResponse({ ...body, id: 99, createdAt: '2026-08-01T00:00:00Z', updatedAt: '2026-08-01T00:00:00Z' }),
      )
    }
    const putMatch = /^\/api\/rules\/(\d+)$/.exec(url.pathname)
    if (putMatch && method === 'PATCH') {
      const id = Number(putMatch[1])
      const body = JSON.parse(String(init?.body)) as RuleInput
      putBodies.push({ id, body })
      return Promise.resolve(
        jsonResponse({ ...body, id, createdAt: '2026-07-01T00:00:00Z', updatedAt: '2026-08-01T00:00:00Z' }),
      )
    }
    if (putMatch && method === 'DELETE') {
      return Promise.resolve(jsonResponse({ id: Number(putMatch[1]), deletedReservations: 0, detachedReservations: 0 }))
    }
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(profiles))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/sites/default/services') return Promise.resolve(jsonResponse(services))
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch

  return { postBodies, putBodies }
}

function renderPage() {
  return renderInRouter(<RulesPage />)
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
    await user.click(await screen.findByRole('button', { name: 'ルールを作成' }))

    await user.type(screen.getByLabelText('名前'), 'テストルール')

    // テキスト条件
    await user.click(screen.getByRole('button', { name: '条件を追加' }))
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

  it('条件が無いまま保存しようとすると確認を挟む', async () => {
    const { postBodies } = stubApi([])
    const user = userEvent.setup()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    renderPage()

    await user.click(await screen.findByRole('button', { name: 'ルールを作成' }))
    await user.type(screen.getByLabelText('名前'), '条件なしルール')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(confirmSpy).toHaveBeenCalled()
    // confirm が false を返したので送信されていない
    expect(postBodies.length).toBe(0)

    confirmSpy.mockReturnValue(true)
    await user.click(screen.getByRole('button', { name: '保存' }))
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

  it('一覧に条件の要約が出て、空のルールは「すべての番組」と分かる', async () => {
    stubApi([sampleRule, ruleWithConditions])
    renderPage()

    await screen.findByText('ニュース')
    expect(screen.getByText('条件なし（すべての番組にマッチ）')).toBeInTheDocument()
    expect(screen.getByText('番組名に「ニュース」を含む')).toBeInTheDocument()
  })

  it('「検索しながら編集」リンクが /search?ruleId=<id> を指す', async () => {
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    const link = screen.getByRole('link', { name: '検索しながら編集' })
    expect(link).toHaveAttribute('href', '/search?ruleId=2')
  })
})
