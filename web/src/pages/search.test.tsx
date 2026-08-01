import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { Program, ProgramSearchRequest, Rule, RuleInput, Service } from '@/api/generated'
import { SearchPage } from '@/pages/search'
import { renderInRouter } from '@/test/router'

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
  {
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
    hasPrograms: true,
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
 * ruleFixture は `?ruleId=7` で開くルール。条件は `news`（'ニュース7'）だけに
 * 当たるキーワード 1 つ（ハイドレーション後の自動検索が正しく動くことの検証用）。
 *
 * `description` / `dedupe*` / `filenameTemplate` / `metadata` / `sites` は
 * どれも `ConditionFields` に UI が無い項目。上書き保存で `buildRuleInput` の
 * `preserve` が効いていることを確認する（＝ `PATCH` の本文からこれらが
 * 落ちていないこと）ため、あえて意味のある値を入れておく。
 */
const ruleFixture: Rule = {
  id: 7,
  name: 'ニュースルール',
  description: 'ニュース番組をまとめて録画する',
  enabled: true,
  priority: 20,
  keepOriginal: 'always',
  encodeProfiles: [],
  textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
  dedupeEnabled: true,
  dedupeThreshold: 0.8,
  dedupeWindowSeconds: 86_400,
  filenameTemplate: '{title}_{startAt}',
  metadata: { note: 'テスト用メタデータ' },
  sites: ['default'],
  createdAt: new Date(origin).toISOString(),
  updatedAt: new Date(origin).toISOString(),
}

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
 *
 * `rules` はルール API（`GET /api/rules/{id}` / `POST /api/rules` /
 * `PATCH /api/rules/{id}`）のスタブが参照する初期データ。作成・上書きした
 * ルールはここに積まれる（`createRuleBodies` / `updateRuleBodies` で
 * リクエスト本体そのものも検証できる）。`PATCH` は `rules` 配列も更新するので、
 * 同じテスト内で連続保存したときの挙動（例: 上書き後に再取得した内容）も追える。
 */
function stubApi(options?: { rules?: Rule[] }) {
  const searchBodies: ProgramSearchRequest[] = []
  const createRuleBodies: RuleInput[] = []
  const updateRuleBodies: { id: number; data: RuleInput }[] = []
  const rules = options?.rules ? [...options.rules] : []

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(jsonResponse(services))
    }

    if (url.pathname === '/api/encode-profiles') {
      return Promise.resolve(jsonResponse([]))
    }

    const ruleDetail = /^\/api\/rules\/(\d+)$/.exec(url.pathname)
    if (ruleDetail && method === 'GET') {
      const found = rules.find((r) => r.id === Number(ruleDetail[1]))
      return Promise.resolve(
        found ? jsonResponse(found) : jsonResponse({ error: 'rule not found' }, 404),
      )
    }

    if (ruleDetail && method === 'PATCH') {
      const id = Number(ruleDetail[1])
      const idx = rules.findIndex((r) => r.id === id)
      if (idx === -1) return Promise.resolve(jsonResponse({ error: 'rule not found' }, 404))
      const body = JSON.parse(String(init?.body ?? '{}')) as RuleInput
      updateRuleBodies.push({ id, data: body })
      const updated: Rule = {
        ...rules[idx],
        ...body,
        id,
        updatedAt: new Date().toISOString(),
      }
      rules[idx] = updated
      return Promise.resolve(jsonResponse(updated))
    }

    if (url.pathname === '/api/rules' && method === 'POST') {
      const body = JSON.parse(String(init?.body ?? '{}')) as RuleInput
      createRuleBodies.push(body)
      const created: Rule = {
        ...body,
        id: 900 + createRuleBodies.length,
        enabled: body.enabled ?? true,
        priority: body.priority ?? 10,
        keepOriginal: body.keepOriginal ?? 'always',
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      }
      rules.push(created)
      return Promise.resolve(jsonResponse(created, 201))
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
  return { fetchMock, searchBodies, createRuleBodies, updateRuleBodies, rules }
}

/**
 * renderPage は SearchPage をルーター（+ QueryClient + ToastProvider）の中で描く。
 * `useSearch` / `useNavigate` はルーターの外では描けないため、既存テストも含めて
 * すべて `renderInRouter` 経由にする。
 */
function renderPage(initialEntries: string[] = ['/search']) {
  return renderInRouter(<SearchPage />, { path: '/search', initialEntries })
}

/**
 * addKeyword はテキスト条件を 1 つ足して値を入れる。
 *
 * ルーター経由の描画（`renderInRouter`）は初回マッチの解決が非同期なので、
 * `render` 直後の同期 `getByRole` は「まだ何も描かれていない」瞬間を掴みうる。
 * ここで `findByRole` にしておけば、呼び出し側で毎回 `findByRole` を先に
 * 挟まなくても安全に使える。
 */
async function addKeyword(value: string, mode: '正規表現' | 'キーワード' = 'キーワード') {
  await userEvent.click(await screen.findByRole('button', { name: '条件を追加' }))
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

  describe('この条件でルールを作成', () => {
    it('テキスト・ジャンル・時間帯の条件を落とさずに RuleInput にする（核心）', async () => {
      const { createRuleBodies } = stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: 'ドラマ' }))

      await userEvent.click(screen.getByRole('button', { name: '時間帯を追加' }))
      fireEvent.change(screen.getByLabelText('時間帯 1 の終了'), { target: { value: '23:00' } })
      await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())

      await userEvent.click(screen.getByRole('button', { name: 'この条件でルールを作成' }))
      await userEvent.type(screen.getByLabelText('名前'), 'テストルール')
      await userEvent.click(screen.getByRole('button', { name: 'ルールを作成' }))

      await waitFor(() => expect(createRuleBodies).toHaveLength(1))
      expect(createRuleBodies[0]).toEqual({
        name: 'テストルール',
        enabled: true,
        priority: 10,
        keepOriginal: 'always',
        encodeProfiles: [],
        textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
        genres: [3],
        times: [{ weekdays: 127, startSec: 0, endSec: 82_800 }],
      })
      expect(await screen.findByText('ルールを作成しました')).toBeInTheDocument()
    })

    it('名前が空だと保存できない', async () => {
      stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: 'この条件でルールを作成' }))

      expect(screen.getByRole('button', { name: 'ルールを作成' })).toBeDisabled()
      expect(screen.getByText('名前は必須です')).toBeInTheDocument()

      await userEvent.type(screen.getByLabelText('名前'), 'テスト')
      expect(screen.getByRole('button', { name: 'ルールを作成' })).not.toBeDisabled()
    })

    it('条件を1つも指定していない状態での保存は確認チェックを挟む', async () => {
      const { createRuleBodies } = stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      // 条件を何も足さずに開く（emptyDraft は draftError を持たないのでボタンは押せる）
      await userEvent.click(screen.getByRole('button', { name: 'この条件でルールを作成' }))
      await userEvent.type(screen.getByLabelText('名前'), 'なんでも')

      expect(screen.getByText(/条件を 1 つも指定していません/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'ルールを作成' })).toBeDisabled()

      await userEvent.click(
        screen.getByRole('checkbox', { name: /すべての番組が対象になることを理解した上で作成します/ }),
      )
      expect(screen.getByRole('button', { name: 'ルールを作成' })).not.toBeDisabled()

      await userEvent.click(screen.getByRole('button', { name: 'ルールを作成' }))
      await waitFor(() => expect(createRuleBodies).toHaveLength(1))
      expect(createRuleBodies[0]).toEqual({
        name: 'なんでも',
        enabled: true,
        priority: 10,
        keepOriginal: 'always',
        encodeProfiles: [],
      })
    })
  })

  describe('?ruleId=N で既存ルールの条件を開く', () => {
    it('条件が復元され、検索が自動実行される', async () => {
      stubApi({ rules: [ruleFixture] })
      renderPage(['/search?ruleId=7'])

      expect(await screen.findByText(/ニュースルール」の条件を編集中/)).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'ルール一覧に戻る' })).toBeInTheDocument()

      // ユーザーが検索ボタンを押さなくても自動実行される
      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('ニュース')
    })

    it('存在しない ruleId では無言の空白にせず 404 と分かる表示をする', async () => {
      stubApi({ rules: [] })
      renderPage(['/search?ruleId=999'])

      expect(await screen.findByText(/ルール #999 が見つかりません/)).toBeInTheDocument()
      // 見つからない以上、条件フォームは空のまま（存在しないルールの条件を
      // 捏造しない）
      expect(screen.queryByLabelText('テキスト条件 1 の値')).not.toBeInTheDocument()
    })

    it('条件を変更して上書き保存すると PATCH に変更後の条件が乗り、画面に留まる（核心）', async () => {
      const { updateRuleBodies, createRuleBodies } = stubApi({ rules: [ruleFixture] })
      const { router } = renderPage(['/search?ruleId=7'])

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      expect(
        screen.getByRole('form', { name: 'ルールの条件を編集' }),
      ).toBeInTheDocument()

      // ハイドレートされた条件（'ニュース'）を書き換える。上書き保存は
      // このフォームの下書き（画面に見えている値）をそのまま送るべきで、
      // 元のルールの条件をそのまま再送ってはならない。
      const input = screen.getByLabelText('テキスト条件 1 の値')
      await userEvent.clear(input)
      await userEvent.type(input, '深夜')

      await userEvent.click(screen.getByRole('button', { name: 'ルールを上書き保存' }))

      await waitFor(() => expect(updateRuleBodies).toHaveLength(1))
      expect(updateRuleBodies[0]?.id).toBe(7)
      expect(updateRuleBodies[0]?.data).toMatchObject({
        name: 'ニュースルール',
        textMatches: [{ target: 'name', mode: 'keyword', value: '深夜' }],
      })
      expect(
        await screen.findByText('ルール「ニュースルール」を上書き保存しました'),
      ).toBeInTheDocument()

      // 別の新しいルールとして保存する副動作は呼んでいない
      expect(createRuleBodies).toHaveLength(0)

      // /rules へは遷移せず、この画面（フォームごと）に留まる。条件を詰め直す
      // 作業の途中で画面が飛ぶと作業が切れるため。
      expect(router.state.location.pathname).toBe('/search')
      expect(
        screen.getByRole('form', { name: 'ルールの条件を編集' }),
      ).toBeInTheDocument()
    })

    it('上書き保存で UI を持たない項目が落ちない（description / dedupe* / filenameTemplate / metadata / sites）', async () => {
      const { updateRuleBodies } = stubApi({ rules: [ruleFixture] })
      renderPage(['/search?ruleId=7'])

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'ルールを上書き保存' }))

      // `preserve` が効いていないと、条件編集 UI に出ていないこれらの項目が
      // 黙って消える（`UpdateRule` は子テーブル全置換のため）。
      await waitFor(() => expect(updateRuleBodies).toHaveLength(1))
      expect(updateRuleBodies[0]?.data).toMatchObject({
        description: 'ニュース番組をまとめて録画する',
        dedupeEnabled: true,
        dedupeThreshold: 0.8,
        dedupeWindowSeconds: 86_400,
        filenameTemplate: '{title}_{startAt}',
        metadata: { note: 'テスト用メタデータ' },
        sites: ['default'],
      })
    })

    it('別の新しいルールとして保存（副動作）は POST を呼び、元のルールと同名にならない', async () => {
      const { createRuleBodies, updateRuleBodies } = stubApi({ rules: [ruleFixture] })
      renderPage(['/search?ruleId=7'])

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: '別の新しいルールとして保存' }))

      // `rules.name` に一意制約が無いので、名前をそのまま引き継ぐと同名の 2 本が
      // 一覧に並び、条件の要約でしか見分けられなくなる。
      await waitFor(() => expect(createRuleBodies).toHaveLength(1))
      expect(createRuleBodies[0]?.name).toBe('ニュースルール のコピー')
      expect(createRuleBodies[0]?.name).not.toBe(ruleFixture.name)

      // 副動作であって主動作（上書き）を兼ねない
      expect(updateRuleBodies).toHaveLength(0)
    })

    it('ハイドレーション後にユーザーが条件を編集しても巻き戻らない', async () => {
      const { rules } = stubApi({ rules: [ruleFixture] })
      const { queryClient } = renderPage(['/search?ruleId=7'])

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      const input = screen.getByLabelText('テキスト条件 1 の値')
      expect(input).toHaveValue('ニュース')

      await userEvent.clear(input)
      await userEvent.type(input, '深夜')
      expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('深夜')

      // サーバー側でルールの内容が変わったあとに再フェッチされても（＝
      // sourceRule の参照が変わっても）、同じ ruleId を一度ハイドレートした
      // あとは上書きしない（ref ガードの回帰テスト）。内容を変えずに
      // invalidate するだけだと React Query の structural sharing で
      // sourceRule の参照が変わらず、effect の依存 [ruleId, sourceRule] 自体が
      // 動かないため、ガードを壊しても検知できない空虚な成功になる。
      // 実際に内容を変えてから再フェッチさせて初めて意味のある回帰テストになる。
      rules[0] = {
        ...rules[0],
        textMatches: [{ target: 'name', mode: 'keyword', value: 'サーバー側で変更' }],
      }
      await act(async () => {
        await queryClient.invalidateQueries()
      })

      expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('深夜')
    })
  })
})
