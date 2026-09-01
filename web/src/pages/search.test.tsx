import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type {
  CapacityOverage,
  Program,
  ProgramSearchRequest,
  Rule,
  RuleInput,
  Service,
} from '@/api/generated'
import { SearchPage } from '@/pages/search'
import { renderInRouter } from '@/test/router'

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
  {
    id: 3273701032,
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

/**
 * 容量ノートの問い合わせ窓は `Date.now()` 由来（`pages/search.tsx`）。フィクスチャは
 * `origin` 基準の相対オフセットなので、実行日の壁時計のままだと窓が届かず揺れる。
 * `vi.useFakeTimers()` は呼ばない（`waitFor` / `userEvent` の実タイマーは動かす）。
 */
beforeEach(() => {
  vi.setSystemTime(origin)
})
afterEach(() => {
  vi.useRealTimers()
  localStorage.clear()
})

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
function stubApi(options?: {
  rules?: Rule[]
  holdProgramDetails?: boolean
  overages?: CapacityOverage[]
  /**
   * `/api/capacity/overages` の 2 回目以降を保留する（`pages/home.test.tsx` の
   * `pendingAfterFirstCall` と同じ仕掛け）。即答させると「キーが進んだ直後の
   * 消えた一瞬」が assert より先に終わり、壊れていても緑になる。
   */
  holdOveragesAfterFirst?: boolean
  /**
   * `allPrograms` に無い番組を検索・詳細取得の対象に足す（終了未定番組
   * `durationMs = 0` を混ぜるテストなど）。`allPrograms` 自体を変えると既存
   * テストの件数の期待値が全部ずれるため、影響を局所化する。
   */
  extraPrograms?: Program[]
}) {
  const searchBodies: ProgramSearchRequest[] = []
  const createRuleBodies: RuleInput[] = []
  const updateRuleBodies: { id: number; data: RuleInput }[] = []
  const programs = [...allPrograms, ...(options?.extraPrograms ?? [])]
  // 値札（RuleCostSummary）用の useQueries が `SearchResultList` と同じクエリキー
  // を再利用しても追加のリクエストが発生しないことを実測するための記録
  // （`pages/search.tsx` のコメント「値札のために追加の HTTP リクエストは
  // 発生しない」の裏付け。下の「値札用の useQueries を足しても...」テストで使う）。
  const programDetailRequests: number[] = []
  // `holdProgramDetails` が true のとき、番組の詳細（GET /api/programs/{id}）を
  // 即座に解決せず保留する。「検索は解決したが durationMs は 1 件も届いていない」
  // 瞬間（`loadedDurationsMs` が空のまま `totalCount > 0`）を確実に再現するための
  // 仕掛け --- 実タイマーに依存すると環境差でその瞬間を取りこぼしうる。
  // `releaseProgramDetails()` で保留分をまとめて解決する。`releaseOneProgramDetail()`
  // は先頭の 1 件だけ --- 詳細が 1 件ずつ別のレンダーへ届く実ネットワークを模す。
  const pendingProgramDetails: (() => void)[] = []
  function releaseProgramDetails() {
    const toRelease = pendingProgramDetails.splice(0, pendingProgramDetails.length)
    for (const resolve of toRelease) resolve()
  }
  function releaseOneProgramDetail() {
    const resolve = pendingProgramDetails.shift()
    resolve?.()
  }
  const rules = options?.rules ? [...options.rules] : []
  // 容量ノート（`ShortfallOverlapNote`）用のリクエスト記録。窓が点滅する回帰を
  // このリクエスト回数と `start` の種類数で固定する。
  const overagesRequests: string[] = []
  const pendingOverages: (() => void)[] = []

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(jsonResponse(services))
    }

    if (url.pathname === '/api/encode-profiles') {
      return Promise.resolve(jsonResponse([]))
    }

    if (url.pathname === '/api/capacity/overages') {
      overagesRequests.push(url.toString())
      const isSecondOrLater = overagesRequests.length > 1
      // 実サーバー（`internal/api/capacity.go`）と同じ挙動にする。半開区間の交差で
      // 絞り、`end` が `start` より後でなければ 400 --- 素通りさせると窓が退化する
      // 回帰にテストが無防備になる。
      const startParam = url.searchParams.get('start')
      const endParam = url.searchParams.get('end')
      const startMs = startParam === null ? NaN : Date.parse(startParam)
      const endMs = endParam === null ? NaN : Date.parse(endParam)
      if (!(endMs > startMs)) {
        return Promise.resolve(jsonResponse({ error: 'end must be after start' }, 400))
      }
      const inWindow = (options?.overages ?? []).filter((o) => {
        const oStart = Date.parse(o.startAt)
        const oEnd = Date.parse(o.endAt)
        return oEnd > startMs && oStart < endMs
      })
      if (options?.holdOveragesAfterFirst && isSecondOrLater) {
        return new Promise<Response>((resolve) => {
          pendingOverages.push(() => resolve(jsonResponse(inWindow)))
        })
      }
      return Promise.resolve(jsonResponse(inWindow))
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
      programDetailRequests.push(Number(detail[1]))
      const found = programs.find((p) => p.programId === Number(detail[1]))
      const response = found ? jsonResponse(found) : jsonResponse({ error: 'not found' }, 404)
      if (options?.holdProgramDetails) {
        return new Promise<Response>((resolve) => {
          pendingProgramDetails.push(() => resolve(response))
        })
      }
      return Promise.resolve(response)
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

      const matched = programs.filter((p) => {
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
  return {
    fetchMock,
    searchBodies,
    createRuleBodies,
    updateRuleBodies,
    rules,
    programDetailRequests,
    releaseProgramDetails,
    releaseOneProgramDetail,
    overagesRequests,
    /** 未解決の `/api/capacity/overages` の本数（保留の仕掛けが効いていることの確認用）。 */
    unresolvedOverages: () => pendingOverages.length,
  }
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
 * addKeyword はテキスト条件の 1 行目に値を入れる（既存の唯一の条件として使う
 * ヘルパー。2 行目以降を足すテストは別に「条件を追加」を明示的に押す）。
 *
 * 1 行目は「条件を追加」を押さなくても常に編集できる（issue #305）ため、
 * ここでボタンを押す必要は無い --- 押すと 2 行目が増えてしまい、その 2 行目
 * の値が空のまま `draftError` に落ちて検索できなくなる。ルーター経由の描画
 * （`renderInRouter`）は初回マッチの解決が非同期なので、`render` 直後の同期
 * `getByLabelText` は「まだ何も描かれていない」瞬間を掴みうる。ここで
 * `findByLabelText` にしておけば、呼び出し側で毎回 `findByRole` を先に
 * 挟まなくても安全に使える。
 */
async function addKeyword(value: string, mode: '正規表現' | 'キーワード' = 'キーワード') {
  if (mode === '正規表現') {
    await userEvent.selectOptions(
      await screen.findByLabelText('テキスト条件 1 のモード'),
      '正規表現',
    )
  }
  await userEvent.type(await screen.findByLabelText('テキスト条件 1 の値'), value)
}

describe('SearchPage', () => {
  /**
   * issue #305: 初画面で「条件を追加」を押さなくてもテキスト条件が打てて、
   * その入力欄がサービスのチップ列より DOM 順で前に来ることを確認する。
   *
   * `addKeyword`（上の helper）は「条件を追加」を押してから打つ経路なので、
   * ここでは意図的に使わず `screen.getByLabelText` で直接見つけて打つ ---
   * それ自体がこのテストの主張（「条件を追加」を経由しなくても届く）になる。
   */
  it('「条件を追加」を押さなくてもテキスト条件が打て、サービスチップより前に来る', async () => {
    const { searchBodies } = stubApi()
    renderPage()

    const serviceChip = await screen.findByRole('button', { name: 'NHK総合' })
    // 「条件を追加」を押さずに、常時出ているはずの 1 行目を直接見つけて打つ。
    const textInput = screen.getByLabelText('テキスト条件 1 の値')

    // DOM 順でテキスト条件がサービスチップより前に来る（この画面は縦に
    // 積むだけのレイアウトなので、DOM 順がそのまま見た目の上下関係になる。
    // 実レイアウトでの可視性・重なりは jsdom では測れないため、その部分は
    // web/e2e/search-mobile.mjs が担う）。
    expect(
      textInput.compareDocumentPosition(serviceChip) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).not.toBe(0)

    await userEvent.type(textInput, 'ニュース')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(searchBodies).toEqual([
      { textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }] },
    ])
  })

  /**
   * レビュー指摘: 主操作を `ConditionFields` の前に移した変更は `pnpm test` に
   * 一切掛かっていなかった（並びだけ元に戻しても 29/29 green だった）。上の
   * テストの `compareDocumentPosition` はテキスト欄とサービスチップの比較で、
   * 主操作の位置は見ていない。座標ではなく DOM 順なので jsdom で測れる ---
   * 将来の refactor で黙って末尾に戻るのを CI で止める。
   */
  it('主操作（検索）は条件の先頭セクション（テキスト条件）より DOM 順で前にある', async () => {
    stubApi()
    renderPage()

    const searchButton = await screen.findByRole('button', { name: '検索' })
    const firstSectionHeading = screen.getByRole('heading', { name: 'テキスト条件' })

    expect(
      searchButton.compareDocumentPosition(firstSectionHeading) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).not.toBe(0)
  })

  /**
   * issue #309: 検索モバイルの読み込み CLS が要改善域だった。サービス一覧の
   * 取得は非同期（「読み込み中…」からチップの複数行へ描画が入れ替わる）だが、
   * チャンネル種別・ジャンル・時間帯・放送時間などの他の節は同期的に描かれる。
   * サービスがチャンネル種別等より DOM 順で前にあると、サービスの高さが変わる
   * たびに既に描画済みの後続の節を下へ押す（Layout Instability API が捉える
   * シフト）。`<ConditionFields>` の並びをサービスが最後になるよう変えたので、
   * それを固定する --- jsdom は実際のシフト量を測れないので、判定は DOM 順の
   * 主張だけに留める（実測は `web/e2e/cls.mjs`）。
   *
   * **比較相手は同期節の最初（`チャンネル種別`）と最後（`期間`）の両方**。
   * `放送時間` だけと比べると、`ScalarFields` の中（無料放送 → 放送時間 →
   * 期間）に `ServiceFields` が差し込まれる「半端に戻る」変異を素通りさせる
   * （その変異を実際に作って、`放送時間` との比較は通るのに `期間` との比較が
   * AssertionError で落ちることを確認した）。
   */
  it('サービスのチップ列は同期的に描かれる節（チャンネル種別 … 期間）より DOM 順で後にある（issue #309）', async () => {
    stubApi()
    renderPage()

    const serviceGroup = await screen.findByRole('group', { name: 'チャンネル' })
    // 同期節の最初と最後。サービスはこの両方より後ろでなければならない。
    const channelTypeHeading = screen.getByRole('heading', { name: 'チャンネル種別' })
    const periodHeading = screen.getByRole('heading', { name: '期間' })

    expect(
      channelTypeHeading.compareDocumentPosition(serviceGroup) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).not.toBe(0)
    expect(
      periodHeading.compareDocumentPosition(serviceGroup) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).not.toBe(0)
  })

  /**
   * レビュー指摘（issue #305 の差し戻し）: 主操作を上に出しただけでは「押した
   * 結果」（値札・件数・結果）が縦カラムの末尾に残り、390px では押しても折り目の
   * 中で何も変わらない（実測: クリック後も `scrollY = 0`、件数行は折り目の 335px
   * 下）。送信の決着時に結果の先頭へスクロールとフォーカスを移して対にする。
   *
   * **jsdom はスクロール位置を測れない**（`scrollIntoView` はスタブ。
   * test/setup.ts）。ここで固定するのは「呼ばれる相手が結果セクションであること」
   * と「フォーカスが移ること」だけで、実際に折り目の中に入るかは
   * `web/e2e/search-mobile.mjs` の④が実ブラウザで測る。
   */
  it('検索の決着時に結果セクションへスクロールとフォーカスを移す', async () => {
    stubApi()
    const scrolled: Element[] = []
    const spy = vi
      .spyOn(Element.prototype, 'scrollIntoView')
      .mockImplementation(function (this: Element) {
        scrolled.push(this)
      })

    try {
      renderPage()
      await screen.findByRole('button', { name: 'NHK総合' })

      const results = screen.getByRole('region', { name: '検索結果' })
      // 描画だけでは動かさない（押していないのにスクロールが起きるのは別の欠陥）
      expect(scrolled).toEqual([])

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))
      expect(await screen.findByText('ニュース7')).toBeInTheDocument()

      expect(scrolled).toEqual([results])
      expect(results).toHaveFocus()
    } finally {
      spy.mockRestore()
    }
  })

  /**
   * 上の裏側（両方向）: `?ruleId=N` で開いたときの自動検索では移さない。
   * ユーザーが押していない検索で飛ばすと、ページを開いた瞬間に条件フォームが
   * 画面外へ出る。
   */
  it('?ruleId の自動検索では結果セクションへ移さない', async () => {
    stubApi({ rules: [ruleFixture] })
    const scrolled: Element[] = []
    const spy = vi
      .spyOn(Element.prototype, 'scrollIntoView')
      .mockImplementation(function (this: Element) {
        scrolled.push(this)
      })

    try {
      renderPage(['/search?ruleId=7'])

      // 自動検索が済んだ（結果が出ている）状態まで待ってから見る。待たずに
      // 見ると「まだ検索が解決していない」だけで通る空虚な成功になる。
      expect(await screen.findByText('ニュース7')).toBeInTheDocument()

      expect(scrolled).toEqual([])
      expect(screen.getByRole('region', { name: '検索結果' })).not.toHaveFocus()
    } finally {
      spy.mockRestore()
    }
  })

  /**
   * レビュー指摘（issue #305 の差し戻し）: 見かけ上の 1 行目は実体が無い間、
   * 「条件を追加」ボタンも削除（X）ボタンも出さない。実体の無い行に対して
   * これらを出すと、押しても・消しても何も起きない死んだコントロールになる。
   */
  it('テキスト条件が空の間は「条件を追加」も削除（X）も出さない', async () => {
    stubApi()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })

    expect(screen.queryByRole('button', { name: '条件を追加' })).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).not.toBeInTheDocument()
    // 触れないままなら「指定なし」になる、という意味を文言で示す
    // （「時間帯」節が空のときに出す文言と同じ形。`ConditionFields` は
    // ルール画面も使うので画面固有の動詞を入れない ---
    // rules.test.tsx「ルール作成フォームでも…」が同じ文言を固定している）
    expect(screen.getByText('指定なし（すべての番組が対象）')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('テキスト条件 1 の値'), 'ニュース')
    expect(screen.queryByText('指定なし（すべての番組が対象）')).not.toBeInTheDocument()
  })

  it('1 行目に入力すると実体化し、「条件を追加」で 2 行目が増える', async () => {
    stubApi()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })

    // 実体化前: 見かけ上の行は 1 本、削除ボタンは無い
    expect(screen.getAllByLabelText(/^テキスト条件 \d+ の値$/)).toHaveLength(1)
    expect(
      screen.queryByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).not.toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('テキスト条件 1 の値'), 'ニュース')

    // 実体化後: 削除ボタンが現れ、「条件を追加」も現れる
    expect(
      await screen.findByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).toBeInTheDocument()
    const addButton = await screen.findByRole('button', { name: '条件を追加' })
    expect(screen.getAllByLabelText(/^テキスト条件 \d+ の値$/)).toHaveLength(1)

    await userEvent.click(addButton)

    // 実際に行が増える（この分岐を `false` に固定しても全テスト green だった
    // ---レビュー指摘 --- ので、行数をリテラルで固定する）
    expect(screen.getAllByLabelText(/^テキスト条件 \d+ の値$/)).toHaveLength(2)
    expect(screen.getByLabelText('テキスト条件 2 の値')).toHaveValue('')
  })

  it('1 行目の値を全消しすると未実体化に戻り、検索できる', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })
    const input = screen.getByLabelText('テキスト条件 1 の値')
    await user.type(input, 'ニュース')
    expect(screen.getByRole('button', { name: '検索' })).not.toBeDisabled()

    await user.clear(input)

    expect(screen.queryByText('テキスト条件の値を入力してください')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '検索' })).not.toBeDisabled()
    expect(screen.getByText('指定なし（すべての番組が対象）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '条件を追加' })).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).not.toBeInTheDocument()
  })

  it('値が空の 1 行目で除外・対象・モードを変えても未実体化のまま検索できる', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })
    const searchButton = screen.getByRole('button', { name: '検索' })

    await user.click(screen.getByRole('button', { name: '除外' }))
    expect(searchButton).not.toBeDisabled()
    await user.selectOptions(screen.getByLabelText('テキスト条件 1 の対象'), 'description')
    expect(searchButton).not.toBeDisabled()
    await user.selectOptions(screen.getByLabelText('テキスト条件 1 のモード'), 'regex')
    expect(searchButton).not.toBeDisabled()

    expect(screen.queryByText('テキスト条件の値を入力してください')).not.toBeInTheDocument()
    expect(screen.getByText('指定なし（すべての番組が対象）')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '条件を追加' })).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).not.toBeInTheDocument()
  })

  it('未実体化の 1 行目で選んだ除外を、値の入力時に保持して送る', async () => {
    const { searchBodies } = stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })
    await user.click(screen.getByRole('button', { name: '除外' }))
    await user.type(screen.getByLabelText('テキスト条件 1 の値'), 'ニュース')
    await user.click(screen.getByRole('button', { name: '検索' }))

    await waitFor(() => expect(searchBodies).toHaveLength(1))
    expect(searchBodies[0]).toEqual({
      textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース', negate: true }],
    })
  })

  it('複数行では 1 行目の値を全消ししても行を残す', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByRole('button', { name: 'NHK総合' })
    await user.type(screen.getByLabelText('テキスト条件 1 の値'), 'ニュース')
    await user.click(screen.getByRole('button', { name: '条件を追加' }))
    await user.type(screen.getByLabelText('テキスト条件 2 の値'), 'ドラマ')

    await user.clear(screen.getByLabelText('テキスト条件 1 の値'))

    expect(screen.getAllByLabelText(/^テキスト条件 \d+ の値$/)).toHaveLength(2)
    expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('')
    expect(screen.getByLabelText('テキスト条件 2 の値')).toHaveValue('ドラマ')
    expect(
      screen.getByRole('button', { name: 'テキスト条件 1 を削除' }),
    ).toBeInTheDocument()
  })

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

  it('検索結果の件数を status として通知する', async () => {
    stubApi()
    renderPage()

    await addKeyword('ニュース')
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    expect(await screen.findByRole('status')).toHaveTextContent('1 件（番組 ID 順）')
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
    // テキスト条件の 1 行目は「条件を追加」を押さなくても常に見かけ上出ている
    // ため存在チェックはできない（issue #305）。クリア後に入っていた値まで
    // 引き継いでいないことを、値が空であることで確かめる。
    expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('')
  })

  describe('値札（コストの見込み）', () => {
    it('未検索と 0 件を混同しない', async () => {
      stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      expect(
        screen.getByText(
          '検索すると、この条件で保存した場合の週あたりの見込み（件数・録画時間）が表示されます',
        ),
      ).toBeInTheDocument()

      await addKeyword('該当しない語')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      // 0 件でも「まだ検索していない」の文言には戻らない
      expect(
        await screen.findByText(/この条件で保存すると、週あたり見込みで約 0 件・約 0分/),
      ).toBeInTheDocument()
      expect(
        screen.queryByText(
          '検索すると、この条件で保存した場合の週あたりの見込み（件数・録画時間）が表示されます',
        ),
      ).not.toBeInTheDocument()
    })

    it('1 件マッチしたときは 7 日換算した件数・時間が出る（母数 = サンプルなので外挿の注記は出ない）', async () => {
      stubApi()
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      // 1 件 * 7/8 = 0.875 → 約 1 件。30 分（1_800_000ms）* 7/8 = 26.25 分 → 約 26分。
      const summary = await screen.findByText(
        /この条件で保存すると、週あたり見込みで約 1 件・約 26分/,
      )
      // 読み込み済み 1 件 = 母数 1 件なので、外挿であることの注記は不要
      expect(summary.textContent).not.toMatch(/先頭/)
    })

    it('読み込みが母数に追いついていない間は「先頭 N 件」からの外挿である旨を明記し、追いつくと消える（値札のために追加の HTTP リクエストは発生しない）', async () => {
      const { programDetailRequests } = stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      // 条件なしの検索で 37 件（pageSize=30 を超える）に当てる
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      // 37 件 * 7/8 = 32.375 → 約 32 件。全 37 件が一様に 30 分（1_800_000ms）なので、
      // 平均 30 分 * 37 件 * 7/8 = 971.25 分 = 16時間11分。最初の 30 件だけのサンプル
      // でも平均は同じ 30 分になるため、この値自体は「読み込みが全件に届いたとき」
      // と変わらない（下の「さらに表示」後の再検証で確認する）。
      await screen.findByText(/この条件で保存すると、週あたり見込みで約 32 件・約 16時間11分/)
      // まだ最初の 30 件しか durationMs を読み込んでいないので、外挿であることを明記する。
      // 「読み込み済み」ではなく「先頭」と言う --- サンプルは programId 昇順の
      // 先頭 N 件で無作為抽出ではない（`lib/rule-cost.ts` の `RuleCostSample` 参照）
      await waitFor(() => {
        expect(screen.getByText(/この条件で保存すると/).textContent).toContain(
          '（時間は先頭 30 件の平均から算出）',
        )
      })

      // 値札用に足した `useQueries`（`pages/search.tsx` の `costSampleIds`）が
      // `SearchResultList` と同じクエリキーを使うため、追加の HTTP リクエストが
      // 発生しないことをここで実測する（30 件・重複無し。60 件になっていないか）。
      await waitFor(() => expect(programDetailRequests.length).toBe(30))
      expect(new Set(programDetailRequests).size).toBe(30)

      await userEvent.click(screen.getByRole('button', { name: 'さらに表示' }))

      // 全件（37 件）の詳細が読み込み終わると、外挿の注記が消える
      // （値そのものは一様な 30 分番組なので変わらない）
      await waitFor(() => {
        expect(
          screen.getByText(/この条件で保存すると、週あたり見込みで約 32 件・約 16時間11分/)
            .textContent,
        ).not.toContain('先頭')
      })

      // 残り 7 件（37 - 30）がさらに読み込まれ、重複は無い（合計 37 件）
      await waitFor(() => expect(programDetailRequests.length).toBe(37))
      expect(new Set(programDetailRequests).size).toBe(37)
    })

    it('番組の詳細が 1 件も届いていない間は「0 件の平均から算出」という自己矛盾した文言を出さない', async () => {
      const { releaseProgramDetails } = stubApi({ holdProgramDetails: true })
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      // 検索（POST .../search）は解決したが、番組の詳細（GET /api/programs/{id}）は
      // まだ 1 件も返っていない瞬間を確実に再現する（`holdProgramDetails` で保留）。
      // 件数は totalCount だけで確定するので先に出るが、時間はサンプルが無いので
      // 「算出中…」になる。
      const summary = await screen.findByText(
        /この条件で保存すると、週あたり見込みで約 1 件・算出中…/,
      )
      // 「0 件の平均から算出」（sampleSize = 0 のまま外挿の注記だけが出る自己矛盾）
      // にならないことを確認する
      expect(summary.textContent).not.toContain('平均から算出')
      expect(summary.textContent).not.toContain('0 件')

      releaseProgramDetails()

      // 詳細が届くと通常の表示に戻る
      await waitFor(() => {
        expect(
          screen.getByText(/この条件で保存すると、週あたり見込みで約 1 件・約 26分/),
        ).toBeInTheDocument()
      })
    })

    it('期間条件で絞っている検索では 8 日換算の根拠を出さず、実際より小さく出ることを明記する（両方向）', async () => {
      stubApi()
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      // 期間を指定していない: 「8 日分を 7 日換算」という根拠が出る
      const withoutPeriod = await screen.findByText(
        /この条件で保存すると、週あたり見込みで約 1 件・約 26分/,
      )
      expect(withoutPeriod.textContent).toContain('8 日分を 7 日換算')
      expect(withoutPeriod.textContent).not.toContain('期間条件で絞っている')

      // 期間を指定する: 観測スパンが 8 日ではなくなるため、8 日を根拠にした文言を
      // 出さず、実際より小さく出ることを明記する
      fireEvent.change(screen.getByLabelText('開始日時'), {
        target: { value: '2020-01-01T00:00' },
      })
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      await waitFor(() => {
        const withPeriod = screen.getByText(
          /この条件で保存すると、週あたり見込みで約 1 件・約 26分/,
        )
        expect(withPeriod.textContent).not.toContain('8 日分を 7 日換算')
        expect(withPeriod.textContent).toContain(
          '期間条件で絞っているため、週あたりの見込みは実際より小さく出ます',
        )
      })
    })

    it('期間の根拠は実行した検索から導く: 下書きを触っても再検索するまで変わらない（両方向）', async () => {
      stubApi()
      renderPage()

      // 期間を入れて検索する
      await addKeyword('ニュース')
      fireEvent.change(screen.getByLabelText('開始日時'), {
        target: { value: '2020-01-01T00:00' },
      })
      await userEvent.click(screen.getByRole('button', { name: '検索' }))
      await waitFor(() => {
        expect(screen.getByText(/この条件で保存すると/).textContent).toContain(
          '期間条件で絞っている',
        )
      })

      // 再検索せずに期間欄だけを空にする。値札の数値（件数・時間）は「期間で
      // 絞った検索」の産物のままなので、その根拠の文言も変わってはならない
      // （変わると、期間で絞った数値に「8 日分を 7 日換算」という偽の根拠が付く）
      fireEvent.change(screen.getByLabelText('開始日時'), { target: { value: '' } })
      // 先に下書きが反映された（＝値札も再レンダーされた）ことを確かめる。これが
      // 無いと、再レンダー前にアサートして空虚に成功しうる
      await waitFor(() => {
        expect(screen.getByLabelText<HTMLInputElement>('開始日時').value).toBe('')
      })
      const afterClear = screen.getByText(/この条件で保存すると/)
      expect(afterClear.textContent).not.toContain('8 日分を 7 日換算')
      expect(afterClear.textContent).toContain('期間条件で絞っている')

      // 逆向き: 期間なしで検索し直すと根拠が戻る
      await userEvent.click(screen.getByRole('button', { name: '検索' }))
      await waitFor(() => {
        expect(screen.getByText(/この条件で保存すると/).textContent).toContain('8 日分を 7 日換算')
      })

      // 再検索せずにフォームへ期間を打ち込んでも、8 日基準の正しい見積もりに
      // 「実際より小さく出ます」という誤った但し書きは付かない
      fireEvent.change(screen.getByLabelText('開始日時'), {
        target: { value: '2020-01-02T00:00' },
      })
      await waitFor(() => {
        expect(screen.getByLabelText<HTMLInputElement>('開始日時').value).toBe('2020-01-02T00:00')
      })
      const afterType = screen.getByText(/この条件で保存すると/)
      expect(afterType.textContent).toContain('8 日分を 7 日換算')
      expect(afterType.textContent).not.toContain('期間条件で絞っている')
    })
  })

  describe('容量への影響（不足区間との交差、issue #475）', () => {
    /**
     * `news`（programId 100）は origin + 100h に開始、30 分番組
     * （`allPrograms` 生成規則、上部参照）。この区間と交差する不足区間を作る。
     */
    function overlappingOverage(): CapacityOverage {
      const startMs = origin + 100 * 3_600_000
      return {
        site: 'default',
        startAt: new Date(startMs + 5 * 60_000).toISOString(),
        endAt: new Date(startMs + 25 * 60_000).toISOString(),
        shortfall: 1,
        jammedTypes: ['GR'],
      }
    }

    it('交差する不足区間があれば件数を 1 行出す', async () => {
      stubApi({ overages: [overlappingOverage()] })
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      expect(
        await screen.findByText('既にチューナー不足の区間と重なる番組が 1 件あります'),
      ).toBeInTheDocument()
    })

    it('交差する不足区間が無ければ何も描画しない（「収まります」とは言わない）', async () => {
      // 不足区間はあるが、`news` の放送時間帯（origin + 100h 〜 100.5h）とは
      // 交差しない遠い時刻に置く。
      const farAway: CapacityOverage = {
        site: 'default',
        startAt: new Date(origin).toISOString(),
        endAt: new Date(origin + 60_000).toISOString(),
        shortfall: 1,
        jammedTypes: ['BS'],
      }
      stubApi({ overages: [farAway] })
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      // 値札の他の行が描画されるのを待ってから、この行だけが無いことを確かめる
      // （非同期の空虚な成功を避けるため、先に「他は描画済み」を確認する）
      await screen.findByText(/この条件で保存すると、週あたり見込みで約 1 件・約 26分/)
      expect(
        screen.queryByText(/既にチューナー不足の区間と重なる番組が/),
      ).not.toBeInTheDocument()
    })

    it('サンプルが上限で切れているときは「先頭 N 件のうち」と明記する', async () => {
      // programId 5（filler の 1 つ、programId 昇順の先頭 30 件に含まれる）の
      // 放送時間帯とだけ交差する不足区間。
      const startMs = origin + 5 * 3_600_000
      const overage: CapacityOverage = {
        site: 'default',
        startAt: new Date(startMs + 5 * 60_000).toISOString(),
        endAt: new Date(startMs + 25 * 60_000).toISOString(),
        shortfall: 1,
        jammedTypes: ['GR'],
      }
      stubApi({ overages: [overage] })
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      // 条件なしの検索で 37 件（pageSize=30 を超える）に当てる
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      expect(
        await screen.findByText(
          '先頭 30 件のうち、既にチューナー不足の区間と重なる番組が 1 件あります',
        ),
      ).toBeInTheDocument()
    })

    it('番組詳細が 1 件ずつ非同期に届いても、容量ノートの問い合わせは増え続けない', async () => {
      const { overagesRequests, releaseOneProgramDetail, programDetailRequests } = stubApi({
        holdProgramDetails: true,
      })
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      // 条件なしの検索で 37 件（pageSize=30 を超える）に当てる。詳細は保留
      // されるので、この時点ではまだ 1 件も届いていない。
      await userEvent.click(screen.getByRole('button', { name: '検索' }))
      await waitFor(() => expect(programDetailRequests.length).toBe(30))

      // 30 件の番組詳細を 1 件ずつ解決する。`filler` は programId 昇順
      // （1..30）でリクエストされ、`releaseOneProgramDetail` は先頭（＝最初に
      // リクエストされた番組）から解決するので、番組名の出現順で解決を追える。
      for (let id = 1; id <= 30; id++) {
        releaseOneProgramDetail()
        // eslint-disable-next-line no-await-in-loop
        await waitFor(() => expect(screen.getByText(`番組 ${id}`)).toBeInTheDocument())
      }

      // 窓が `Date.now()` だけに依存する固定窓であれば、30 回の個別解決を
      // 経ても `/api/capacity/overages` への要求は高々 1 回で足りる（React の
      // 再レンダーの割れ方に依存させないよう、上限には少し余裕を持たせる）。
      expect(overagesRequests.length).toBeGreaterThanOrEqual(1)
      expect(overagesRequests.length).toBeLessThanOrEqual(3)
    })

    /**
     * 終了未定番組（`durationMs = 0`）だけがサンプルのとき、窓をその時刻から
     * 作ると `start === end` に退化して 400 で沈黙する。ここで数えられるのは
     * 「不足区間が開始の瞬間を厳密にまたぐ」形だけ（`countProgramsInShortfall`
     * の doc。他の形を数えない旨の判定は `capacity.test.ts`）。
     */
    it('終了未定番組（durationMs = 0）だけがサンプルでも窓は退化せず、開始の瞬間をまたぐ不足区間なら数える', async () => {
      const undetermined: Program = {
        programId: 900,
        networkId: 32736,
        serviceId: 1024,
        eventId: 900,
        startAt: new Date(origin + 3 * 3_600_000).toISOString(),
        endAt: new Date(origin + 3 * 3_600_000).toISOString(),
        durationMs: 0,
        name: '終了未定番組',
        description: '',
        genres: [0],
        isFree: true,
      }
      // 放送開始の瞬間を厳密にまたぐ不足区間（幅 0 の区間が交差する唯一の形）。
      const overage: CapacityOverage = {
        site: 'default',
        startAt: new Date(origin + 3 * 3_600_000 - 5 * 60_000).toISOString(),
        endAt: new Date(origin + 3 * 3_600_000 + 5 * 60_000).toISOString(),
        shortfall: 1,
        jammedTypes: ['GR'],
      }
      const { overagesRequests } = stubApi({ extraPrograms: [undetermined], overages: [overage] })
      renderPage()

      await addKeyword('終了未定番組')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))

      expect(await screen.findByText('終了未定番組')).toBeInTheDocument()
      // 「不足区間があるのに沈黙」に落ちていないこと（ノートが実際に出る）。
      expect(
        await screen.findByText('既にチューナー不足の区間と重なる番組が 1 件あります'),
      ).toBeInTheDocument()

      // 窓が退化していれば `/api/capacity/overages` の `end` が `start` 以下に
      // なる（直す前の実装ではここが `start === end` になり 400 だった）。
      expect(overagesRequests.length).toBeGreaterThanOrEqual(1)
      for (const url of overagesRequests) {
        const params = new URL(url).searchParams
        const startMs = Date.parse(params.get('start') ?? '')
        const endMs = Date.parse(params.get('end') ?? '')
        expect(endMs).toBeGreaterThan(startMs)
      }
    })

    /**
     * 窓は時境界へ量子化してあるので、キーは毎時 0 分に 1 回進む。新しいキーには
     * データが無いため、素のままだとノートが 1 RTT 消える（`placeholderData:
     * keepPreviousData` がそれを止めていることの判定。`pages/home.test.tsx`
     * 「ホーム: 時境界を越えてキーが変わっても警告は消えない」と同じ形）。
     */
    it('時境界を越えてクエリキーが進み、新しいキーが未解決でもノートは消えない', async () => {
      // 時境界（`origin` は毎時 0 分）の 500ms 前に「今」を置く。
      vi.setSystemTime(origin - 500)
      const { overagesRequests, releaseOneProgramDetail, unresolvedOverages } = stubApi({
        overages: [overlappingOverage()],
        holdProgramDetails: true,
        holdOveragesAfterFirst: true,
      })
      renderPage()

      await addKeyword('ニュース')
      await userEvent.click(screen.getByRole('button', { name: '検索' }))
      // 詳細が届いて初めてノートの母集団ができる（1 件目 = `news`）。
      await waitFor(() => expect(overagesRequests.length).toBe(1))
      releaseOneProgramDetail()
      expect(
        await screen.findByText('既にチューナー不足の区間と重なる番組が 1 件あります'),
      ).toBeInTheDocument()

      // 時境界を越えたうえで再レンダーの引き金を引く（下書きを 1 文字足す）。
      vi.setSystemTime(origin + 500)
      await userEvent.type(screen.getByLabelText('テキスト条件 1 の値'), '7')

      // キーが実際に進んだこと（`start` の違う 2 回目の要求）を確かめる。これが
      // 無いと「キーが変わらなかったので消えなかった」でも通ってしまう。
      await waitFor(() => {
        const starts = overagesRequests.map((url) => new URL(url).searchParams.get('start'))
        expect(new Set(starts).size).toBe(2)
      })
      // 2 回目がまだ未解決であること自体を assert する（保留の仕掛けが静かに
      // 効かなくなると、`placeholderData` が無くてもノートは戻ってきてしまう）。
      expect(unresolvedOverages()).toBe(1)

      expect(
        screen.getByText('既にチューナー不足の区間と重なる番組が 1 件あります'),
      ).toBeInTheDocument()
    })
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

    /**
     * 期間指定の副作用の注意書きは警告なので琥珀（`--warning`）。jsdom は色を
     * 計算しないのでクラス名を見る（実画素の判定は web/e2e/design.mjs。
     * docs/frontend/design.md「色は信号のみ」）。
     */
    it('期間指定の注意書きは警告の信号色（琥珀）で出る', async () => {
      stubApi()
      renderPage()

      expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
      await addKeyword('ニュース')
      fireEvent.change(screen.getByLabelText('開始日時'), {
        target: { value: '2026-08-12T21:00' },
      })
      await userEvent.click(screen.getByRole('button', { name: 'この条件でルールを作成' }))

      const note = screen.getByText(/期間を指定したまま作成すると/)
      expect(note).toHaveClass('text-warning')
      expect(note.className).not.toMatch(/amber|yellow|orange/)
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
      // 捏造しない）。1 行目は「条件を追加」を押さなくても常に見かけ上出て
      // いるため（issue #305）、存在ではなく値が空であることで確かめる。
      expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('')
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

/**
 * 検索条件の置き場所は **URL > localStorage > 空**（docs/frontend/design.md §個人化）。
 *
 * URL は共有・ブックマークの宛先、localStorage は端末ごとの「前回の条件」。
 * どちらも下書きへ写す経路が同じ（`conditionsToDraft`）なので、取り違えると
 * 「共有リンクを開いたのに自分の前回の条件が出る」形で静かに壊れる。
 */
describe('SearchPage の条件の復元', () => {
  const lastKey = 'rokuban:search:last'
  const newsCondition = {
    textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
  }
  const condEntry = (cond: unknown) => [`/search?cond=${encodeURIComponent(JSON.stringify(cond))}`]

  it('?cond= で開くと条件が下書きに入り、そのまま検索される', async () => {
    const { searchBodies } = stubApi()
    renderPage(condEntry(newsCondition))

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('ニュース')
    // 開いた URL の条件がそのまま 1 回だけ送られる（同じ条件で二重に叩かない）
    expect(searchBodies).toEqual([newsCondition])
  })

  it('検索を押すと条件が URL と localStorage の両方に載る', async () => {
    const { searchBodies } = stubApi()
    const { router } = renderPage()

    await addKeyword('ニュース')
    await userEvent.click(await screen.findByRole('button', { name: '検索' }))
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()

    await waitFor(() => {
      expect((router.state.location.search as { cond?: unknown }).cond).toEqual(newsCondition)
    })
    expect(JSON.parse(localStorage.getItem(lastKey)!)).toEqual(newsCondition)
    // **押した回数だけ叩く。** `submit` が `appliedCondRef` に印を付け忘れると、
    // 自分で書き換えた URL をハイドレーション effect が拾って同じ条件を 2 回
    // 叩く（その変異でこの行が落ちることを確認済み）。
    //
    // **このハーネスで測れない部分がある。** `renderInRouter`（`test/router.tsx`）は
    // `validateSearch` を持たない最小ルートなので、`?cond=` が openapi のスキーマを
    // 通らない。「スキーマを通ると形が変わるせいで、生の JSON 比較では同じ条件を
    // 2 回叩く」という壊れ方は、ここではなく `routes.test.tsx`（validateSearch の
    // 出力の形）と `e2e/personalization.mjs` ③（実ブラウザで押下 1 回 = 1 回）が
    // 見ている。形が変わること自体は `lib/program-search.test.ts` が固定している。
    expect(searchBodies).toEqual([newsCondition])
  })

  it('URL に条件が無ければ前回の条件を下書きに戻すが、検索はしない', async () => {
    localStorage.setItem(lastKey, JSON.stringify(newsCondition))
    const { searchBodies } = stubApi()
    renderPage()

    expect(await screen.findByLabelText('テキスト条件 1 の値')).toHaveValue('ニュース')
    // 「まだ検索していない」を非同期の空虚な成功にしないため、実際に飛ぶ
    // 問い合わせ（サービス一覧）が解決するまで待ってから 0 件を主張する。
    expect(await screen.findByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
    expect(searchBodies).toEqual([])
    expect(screen.queryByText('ニュース7')).not.toBeInTheDocument()
  })

  it('URL の条件は localStorage の前回の条件より優先される', async () => {
    localStorage.setItem(
      lastKey,
      JSON.stringify({ textMatches: [{ target: 'name', mode: 'keyword', value: '深夜' }] }),
    )
    stubApi()
    renderPage(condEntry(newsCondition))

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByLabelText('テキスト条件 1 の値')).toHaveValue('ニュース')
  })

  it('壊れた前回の条件は無視して空の下書きで開く', async () => {
    localStorage.setItem(lastKey, '{textMatches:')
    stubApi()
    renderPage()

    expect(await screen.findByLabelText('テキスト条件 1 の値')).toHaveValue('')
  })

  /**
   * `?ruleId=` はルール編集として開く経路で、条件の正本はルールにある。ここで
   * `cond` も書くと、次に開いたときどちらを写したのかが読めなくなる。
   */
  it('?ruleId= で開いた画面の検索は URL に cond を載せない', async () => {
    stubApi({ rules: [ruleFixture] })
    const { router } = renderPage(['/search?ruleId=7'])

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '検索' }))

    await waitFor(() => {
      expect(JSON.parse(localStorage.getItem(lastKey)!)).toEqual({
        textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
      })
    })
    expect((router.state.location.search as { cond?: unknown }).cond).toBeUndefined()
  })
})
