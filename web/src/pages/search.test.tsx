import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { Program, ProgramSearchRequest, Service } from '@/api/generated'
import { SearchPage } from '@/pages/search'

const services: Service[] = [
  {
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
  },
  {
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
  },
]

const origin = new Date('2026-07-29T12:00:00Z').getTime()

function program(programId: number, serviceId: number, name: string): Program {
  const startAt = origin + programId * 3_600_000
  return {
    programId,
    networkId: 32736,
    serviceId,
    eventId: programId,
    startAt: new Date(startAt).toISOString(),
    endAt: new Date(startAt + 1_800_000).toISOString(),
    durationMs: 1_800_000,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

/** 30 件（pageSize）を超えるように詰め物を入れる。「さらに表示」の検証用。 */
const filler = Array.from({ length: 35 }, (_, i) => program(i + 1, 1024, `番組 ${i + 1}`))
const news = program(100, 1024, 'ニュース7')
const drama = program(101, 1032, '深夜ドラマ')
const allPrograms = [...filler, news, drama]

/**
 * invalidRegexMessage はサーバーが不正な ARE に付ける 400 の本文
 * （internal/api/search.go の `searchRegexError` と同じ形）。
 */
const invalidRegexMessage =
  'invalid regex "(" (POSIX ARE; lookbehind is not supported): ERROR: invalid regular expression: parentheses () not balanced'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * stubApi は検索を実際に評価するスタブを立てる。
 *
 * 条件を無視して固定の結果を返すスタブにすると、「条件を入れると結果が変わる」
 * という画面の主張をテストが検証できない（フォームが値をリクエストに載せていなくても
 * 通ってしまう）。そこでキーワード・ジャンル・サービス・無料の 4 次元だけ
 * ミニ実装し、不正な正規表現はサーバーと同じ 400 を返す。
 */
function stubApi() {
  const searchBodies: ProgramSearchRequest[] = []

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')

    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(jsonResponse(services))
    }

    const detail = /^\/api\/sites\/default\/programs\/(\d+)$/.exec(url.pathname)
    if (detail) {
      const found = allPrograms.find((p) => p.programId === Number(detail[1]))
      return Promise.resolve(
        found ? jsonResponse(found) : jsonResponse({ error: 'not found' }, 404),
      )
    }

    if (url.pathname === '/api/sites/default/programs/search') {
      const body = JSON.parse(String(init?.body ?? '{}')) as ProgramSearchRequest
      searchBodies.push(body)

      for (const match of body.textMatches ?? []) {
        if (match.mode === 'regex' && match.value === '(') {
          return Promise.resolve(jsonResponse({ error: invalidRegexMessage }, 400))
        }
        // EPG のローリングウィンドウから抜けた番組が結果に残る状況の再現。
        // 検索は当たるが詳細（GET /api/programs/{id}）が 404 になる
        if (match.value === '幽霊') return Promise.resolve(jsonResponse([999]))
      }

      const matched = allPrograms.filter((p) => {
        for (const match of body.textMatches ?? []) {
          const haystack = match.target === 'name' ? p.name : p.description
          const hit =
            match.mode === 'regex'
              ? new RegExp(match.value, match.caseSensitive === true ? '' : 'i').test(haystack)
              : haystack.toLowerCase().includes(match.value.toLowerCase())
          if (hit === (match.negate === true)) return false
        }
        if (body.genres !== undefined && !body.genres.some((g) => p.genres.includes(g))) {
          return false
        }
        if (
          body.services !== undefined &&
          !body.services.some((s) => s.serviceId === p.serviceId && s.networkId === p.networkId)
        ) {
          return false
        }
        if (body.isFree !== undefined && body.isFree !== null && body.isFree !== p.isFree) {
          return false
        }
        return true
      })

      return Promise.resolve(jsonResponse(matched.map((p) => p.programId).sort((a, b) => a - b)))
    }

    throw new Error(`unexpected fetch: ${url.pathname}`)
  })

  globalThis.fetch = fetchMock as unknown as typeof fetch
  return { fetchMock, searchBodies }
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <SearchPage />
    </QueryClientProvider>,
  )
}

/** addKeyword はテキスト条件を 1 つ足して値を入れる。 */
async function addKeyword(value: string, mode: '正規表現' | 'キーワード' = 'キーワード') {
  await userEvent.click(screen.getByRole('button', { name: '条件を追加' }))
  if (mode === '正規表現') {
    await userEvent.selectOptions(screen.getByLabelText('テキスト条件 1 のモード'), '正規表現')
  }
  await userEvent.type(screen.getByLabelText('テキスト条件 1 の値'), value)
}

describe('SearchPage', () => {
  it('検索前の案内と 0 件の案内を混同しない', async () => {
    stubApi()
    renderPage()

    // サービスの取得を待ってから見る（待たずに見ると、まだ何も描かれていない
    // 状態を「案内が出ている」と読み違えうる）
    expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
    expect(screen.getByText('条件を指定して検索してください')).toBeInTheDocument()
    expect(screen.queryByText('条件に一致する番組がありません')).not.toBeInTheDocument()

    await addKeyword('該当しない語')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByText('条件に一致する番組がありません')).toBeInTheDocument()
    // 検索したあとに「条件を指定してください」が残っていてはならない
    expect(screen.queryByText('条件を指定して検索してください')).not.toBeInTheDocument()
  })

  it('キーワードに一致した番組だけを出す', async () => {
    const { searchBodies } = stubApi()
    renderPage()

    await addKeyword('ニュース')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('1 件（番組 ID 順）')).toBeInTheDocument()
    expect(screen.queryByText('深夜ドラマ')).not.toBeInTheDocument()
    // サービス名と放送時間も番組リストと同じ語彙で出る（サービス名は
    // 絞り込みチップにも出るので、結果一覧の中に限って探す）
    const results = within(screen.getByTestId('search-results'))
    expect(results.getByText('NHK総合')).toBeInTheDocument()
    expect(results.getByText('30分')).toBeInTheDocument()

    expect(searchBodies).toEqual([
      { textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }] },
    ])
  })

  it('チップで選んだ条件がリクエストに乗る', async () => {
    const { searchBodies } = stubApi()
    renderPage()

    // サービスチップはサービス一覧が届いてから出る
    await userEvent.click(await screen.findByRole('button', { name: 'NHKEテレ' }))
    await userEvent.click(screen.getByRole('button', { name: 'ドラマ' }))
    await userEvent.click(screen.getByRole('button', { name: '無料のみ' }))
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    // ジャンル 3（ドラマ）で絞ったので、genres = [0] の番組は 1 件も残らない
    expect(await screen.findByText('条件に一致する番組がありません')).toBeInTheDocument()
    expect(searchBodies).toEqual([
      {
        isFree: true,
        genres: [3],
        services: [{ networkId: 32737, serviceId: 1032 }],
      },
    ])
  })

  it('不正な正規表現ではサーバーのメッセージを見せる', async () => {
    stubApi()
    renderPage()

    await addKeyword('(', '正規表現')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByText('検索に失敗しました')).toBeInTheDocument()
    // 黙って 0 件にしない。理由を出さないと「書き方が悪い」のか
    // 「該当なし」なのかが区別できない
    expect(screen.getByText(invalidRegexMessage)).toBeInTheDocument()
    expect(screen.queryByText('条件に一致する番組がありません')).not.toBeInTheDocument()
  })

  it('正しい正規表現ではエラーを出さない', async () => {
    stubApi()
    renderPage()

    await addKeyword('^ニュース', '正規表現')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    // 結果が出揃うのを待ってから不在を確認する（待たずに見ると、
    // 検索が解決する前の状態を見て通ってしまう）
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.queryByText('検索に失敗しました')).not.toBeInTheDocument()
  })

  it('曜日を 1 つも選んでいない時間帯では検索しない', async () => {
    const { searchBodies } = stubApi()
    renderPage()

    expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '時間帯を追加' }))

    const weekdays = screen.getByRole('group', { name: '時間帯 1 の曜日' })
    for (const label of ['月', '火', '水', '木', '金', '土', '日']) {
      await userEvent.click(await screen.findByRole('button', { name: label }))
      expect(weekdays).toBeInTheDocument()
    }

    // weekdays = 0 は rulequery が範囲外エラーにする（API は 400 にしないので 500）
    expect(screen.getByRole('alert')).toHaveTextContent('時間帯には曜日を 1 つ以上選んでください')
    expect(screen.getByRole('button', { name: '検索' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: '検索' }))
    expect(searchBodies).toEqual([])

    // ボタンの無効化だけに頼らない。Enter による暗黙の送信は既定ボタンが
    // 無効でも submit が届きうるので、フォーム自身も送らないことを確かめる
    fireEvent.submit(screen.getByRole('form', { name: '検索条件' }))
    expect(searchBodies).toEqual([])

    // 曜日を戻すと、次は幅ゼロの時間帯が止める（開始 = 終了 = 00:00）
    await userEvent.click(screen.getByRole('button', { name: '毎日' }))
    expect(screen.getByRole('alert')).toHaveTextContent(
      '時間帯の開始と終了には違う時刻を指定してください',
    )

    // 幅のある時間帯にすると検索できる。`type` は time 入力に 1 文字ずつ入れる
    // ため中間状態（00:59）で確定してしまうので、値の差し替えで入力する
    fireEvent.change(screen.getByLabelText('時間帯 1 の終了'), { target: { value: '23:00' } })
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    await waitFor(() => expect(searchBodies).toHaveLength(1))
    expect(searchBodies[0]).toEqual({ times: [{ weekdays: 127, startSec: 0, endSec: 82_800 }] })
  })

  it('結果が多いときは区切って表示し、さらに表示で増える', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
    // 条件なしの検索は「全番組」という正しい問い。止めない
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByText('37 件（番組 ID 順）— 30 件を表示')).toBeInTheDocument()
    expect(await screen.findByText('番組 1')).toBeInTheDocument()
    // programId 昇順なので、詰め物のあとに来る 2 件は最初のページに入らない
    expect(screen.queryByText('ニュース7')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('37 件（番組 ID 順）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'さらに表示' })).not.toBeInTheDocument()
  })

  it('詳細を取得できなかった結果を黙って落とさない', async () => {
    stubApi()
    renderPage()

    await addKeyword('幽霊')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    // 件数は 1 件と言っているのに行が 0 本、という食い違いを作らない
    expect(await screen.findByText('1 件（番組 ID 順）')).toBeInTheDocument()
    expect(await screen.findByText('番組 #999 の詳細を取得できませんでした')).toBeInTheDocument()
  })

  it('条件をクリアすると検索前の状態に戻る', async () => {
    stubApi()
    renderPage()

    await addKeyword('ニュース')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '条件をクリア' }))

    expect(screen.getByText('条件を指定して検索してください')).toBeInTheDocument()
    expect(screen.queryByText('ニュース7')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('テキスト条件 1 の値')).not.toBeInTheDocument()
  })
})
