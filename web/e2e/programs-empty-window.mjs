// 番組リストの空の時間窓から後続窓へ進める導線の受け入れ判定。
//
// 見るのは:
//   ① 最初の空窓でも「次の時間帯を見る」で 6 時間ずつ進める
//   ② 連続する空窓のあとに後続取得が失敗しても、自動再試行せず同じボタンで
//      再試行でき、成功した窓の番組へ到達できる
//   ③ 番組表示後に空窓へ進んだ場合も、番兵による自動連鎖を止めて手動導線へ
//      切り替える
//   ④ 最終窓（hasNextPage=false）では空のままボタンを消し、追加取得しない
//
// 実 API / mirakc / DB は要らない。API は page.route で差し替える。
//
// フィクスチャは「何回目の要求か」ではなく「どの時間窓（start パラメータ）
// への要求か」で引く。本番の infinite query は既定のリトライ（1s + 2s + 4s の
// バックオフ）を持つため、同じ窓へ複数回リクエストが飛びうる --- 呼び出し回数で
// フィクスチャを進めると、自動リトライが「失敗窓の次の成功フィクスチャ」を
// 食べてしまい、エラー表示へ到達する前に黙って成功する。失敗させたい窓は、
// テスト本体が明示的に `api.repair(...)` で直すまで何度呼ばれても 500 を返す。
// 件数の判定も「要求した相異なる窓の数」（`distinctWindows`）で行い、
// 自動リトライで同じ窓が複数回来ても判定が揺れないようにする。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-empty-window
import { ListProgramsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'
const HOUR = 3_600_000
const WINDOW = 6 * HOUR
const QUARTER = 15 * 60_000
const FIXED_NOW = new Date('2026-08-14T12:00:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const service = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: 'テスト局',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

function program(programId, startAtMs, name) {
  return {
    programId,
    networkId: service.networkId,
    serviceId: service.serviceId,
    eventId: programId,
    startAt: iso(startAtMs),
    endAt: iso(startAtMs + QUARTER),
    durationMs: QUARTER,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

// 1 ページぶんをビューポートより長くする。復旧後に sentinel が直ちに可視になって
// 次の窓まで自動取得されると、空窓から「1 回の操作で 1 窓」の境界が測れないため。
const recoveryStartMs = nowMs + 2 * WINDOW
const recoveryPrograms = Array.from({ length: 24 }, (_, index) =>
  program(683001 + index, recoveryStartMs + index * QUARTER, `空窓から復旧した番組${index}`),
)
const populatedFirstWindow = Array.from({ length: 24 }, (_, index) =>
  program(683100 + index, nowMs + index * QUARTER, `空窓の前に表示された番組${index}`),
)

const ng = []

// createProgramApi は `baseMs` からの 6 時間刻みの窓インデックスでフィクスチャを
// 引く。`windowResponses[i]` が窓 i（[baseMs + i*6h, baseMs + (i+1)*6h)）への
// 応答。`'error'` は `repair` で直すまで何度要求されても 500 を返し続ける。
function createProgramApi(baseMs, windowResponses) {
  const windows = new Map(windowResponses.map((response, index) => [index, response]))
  const requests = []

  const windowIndexFor = (startMs) => Math.round((startMs - baseMs) / WINDOW)

  const handler = async ({ path: p, url, json, route }) => {
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/reservations') return json([])
    if (p === '/api/sites/default/services') return json([service])
    if (p === '/api/encode-profiles') return json([])
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (p === '/api/sites/default/programs') {
      const startMs = Date.parse(url.searchParams.get('start') ?? '')
      const endMs = Date.parse(url.searchParams.get('end') ?? '')
      requests.push({ startMs, endMs })
      const response = windows.get(windowIndexFor(startMs)) ?? []
      if (response === 'error') {
        return route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'program window unavailable' }),
        })
      }
      return json(response)
    }
    // `/api/events` などこの判定に関係しない API は、アプリが描画を続けられる
    // 空の成功応答にする。番組 API の経路だけは上で明示的に数える。
    return json([])
  }

  return {
    handler,
    requests,
    // repair はある窓（インデックス）の以後の応答を差し替える。エラー表示を
    // 確認したあとに呼び、その状態で手動再試行ボタンを押させる。
    repair(index, response) {
      windows.set(index, response)
    },
  }
}

// distinctWindows は要求のうち相異なる時間窓（start が同じものを 1 個に潰す）
// だけを初出順に返す。自動リトライで同じ窓へ複数回要求が飛んでも件数が
// ぶれないようにするため、件数判定・窓インデックス参照はすべてこれを介す。
function distinctWindows(requests) {
  const seen = new Set()
  const result = []
  for (const request of requests) {
    if (seen.has(request.startMs)) continue
    seen.add(request.startMs)
    result.push(request)
  }
  return result
}

async function waitForWindowCount(requests, expected, label) {
  const deadline = Date.now() + 15_000
  while (distinctWindows(requests).length < expected && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  const actual = distinctWindows(requests).length
  if (actual < expected) {
    ng.push(`${label}: 相異なる時間窓への要求が ${expected} 個に達しない（実際 ${actual} 個）`)
  }
}

function assertRequestWindow(request, expectedStartMs, label) {
  if (!request) {
    ng.push(`${label}: 番組 API の要求が記録されていない`)
    return
  }
  if (request.startMs !== expectedStartMs || request.endMs !== expectedStartMs + WINDOW) {
    ng.push(
      `${label}: 時間窓が不正（${iso(request.startMs)} - ${iso(request.endMs)}。期待 ${iso(expectedStartMs)} - ${iso(expectedStartMs + WINDOW)}）`,
    )
  }
}

async function newProgramPage(browser, api) {
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, api.handler)
  return { context, page }
}

log(`URL: ${URL_BASE}`)
log(`固定時刻: ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ...recoveryPrograms.map((item, index) => [
      `recoveryPrograms[${index}]`,
      ListProgramsResponseItem,
      item,
    ]),
    ...populatedFirstWindow.map((item, index) => [
      `populatedFirstWindow[${index}]`,
      ListProgramsResponseItem,
      item,
    ]),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

// --- ①② 最初の空窓 → 空窓 → 取得失敗 → 手動再試行 → 番組あり ---
// 窓 2（error）は `repair` で直すまで何度要求されても 500 を返し続ける ---
// 既定のリトライ（本番の挙動）が黙って成功フィクスチャを食べてしまうと、
// 「エラー表示 → 利用者が手動再試行」という導線そのものが測れなくなる。
const recoveryApi = createProgramApi(nowMs, [[], [], 'error'])
const recovery = await newProgramPage(browser, recoveryApi)
const recoveryPage = recovery.page
await recoveryPage.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })

const emptyState = recoveryPage.getByText('この時間帯の番組がありません')
const nextWindowButton = recoveryPage.getByRole('button', { name: '次の時間帯を見る' })

await emptyState.waitFor({ timeout: 15_000 })
await waitForWindowCount(recoveryApi.requests, 1, '① 初回窓')
log('\n=== ① 最初の空窓 ===')
if (distinctWindows(recoveryApi.requests).length !== 1) {
  ng.push(`① 初回窓の相異なる要求数が 1 ではない（${distinctWindows(recoveryApi.requests).length}）`)
}
assertRequestWindow(distinctWindows(recoveryApi.requests)[0], nowMs, '① 初回窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('① 最初の空窓に「次の時間帯を見る」が 1 個表示されない')
}
if ((await recoveryPage.getByTestId('program-list-sentinel').count()) !== 0) {
  ng.push('① 最初の空窓に自動読み込みの番兵が残っている')
}

await nextWindowButton.click()
await waitForWindowCount(recoveryApi.requests, 2, '① 2 番目の窓')
await emptyState.waitFor()
if (distinctWindows(recoveryApi.requests).length !== 2) {
  ng.push(`① 空窓 2 回目の要求後に窓が連鎖した（${distinctWindows(recoveryApi.requests).length}）`)
}
assertRequestWindow(distinctWindows(recoveryApi.requests)[1], nowMs + WINDOW, '① 2 番目の窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('① 連続する空窓に「次の時間帯を見る」が残らない')
}

await nextWindowButton.click()
await waitForWindowCount(recoveryApi.requests, 3, '② 失敗する窓')
// 既定のリトライのバックオフ（1s + 2s + 4s）を使い切ってから isFetchNextPageError
// が立つので、表示までに約 7 秒かかる。
await recoveryPage.getByText('続きの取得に失敗しました').waitFor({ timeout: 15_000 })
log('\n=== ② 後続窓の取得失敗 ===')
if (distinctWindows(recoveryApi.requests).length !== 3) {
  ng.push(`② 失敗時の相異なる要求数が 3 ではない（${distinctWindows(recoveryApi.requests).length}）`)
}
assertRequestWindow(distinctWindows(recoveryApi.requests)[2], nowMs + 2 * WINDOW, '② 失敗する窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('② 取得失敗後に同じ再試行ボタンが表示されない')
}
await recoveryPage.waitForTimeout(300)
if (distinctWindows(recoveryApi.requests).length !== 3) {
  ng.push('② 取得失敗後の待機中に自動再試行で新しい窓が要求された')
}

// ここで初めて窓 2 を直す。直す前にボタンを押しても 500 のままであることは
// 上の待機で確認済み。
recoveryApi.repair(2, recoveryPrograms)
await nextWindowButton.click()
await recoveryPage.getByText('空窓から復旧した番組0').waitFor({ timeout: 15_000 })
log('\n=== ② 手動再試行の成功 ===')
if (distinctWindows(recoveryApi.requests).length !== 3) {
  ng.push(`② 再試行後に新しい窓が増えた（${distinctWindows(recoveryApi.requests).length}。同じ窓の再試行のはず）`)
}
assertRequestWindow(distinctWindows(recoveryApi.requests)[2], nowMs + 2 * WINDOW, '② 再試行')
if ((await recoveryPage.getByText('この時間帯の番組がありません').count()) !== 0) {
  ng.push('② 番組取得成功後も空窓の表示が残っている')
}
if ((await recoveryPage.getByRole('button', { name: 'さらに読み込む' }).count()) !== 0) {
  ng.push('② 自動読み込み可能な通常窓に手動の「さらに読み込む」が表示された')
}
await recoveryPage.waitForTimeout(300)
if (distinctWindows(recoveryApi.requests).length !== 3) {
  ng.push('② 復旧した通常窓から予期しない新しい窓の取得が発生した')
}
await recovery.context.close()

// --- ③ 番組表示後に空窓へ進んだ場合も、自動連鎖を止める ---
const afterProgramApi = createProgramApi(nowMs, [populatedFirstWindow, []])
const afterProgram = await newProgramPage(browser, afterProgramApi)
const afterProgramPage = afterProgram.page
await afterProgramPage.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
await afterProgramPage.getByText('空窓の前に表示された番組0').waitFor({ timeout: 15_000 })
await waitForWindowCount(afterProgramApi.requests, 1, '③ 初回の番組あり窓')

// 初回窓は十分な行数があるので、末尾までスクロールすれば既存の自動読み込みを
// 発火できる。2 窓目は空で返し、その後に 3 窓目へ自動で進まないことを見る。
await afterProgramPage.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight))
await waitForWindowCount(afterProgramApi.requests, 2, '③ 空窓')
await afterProgramPage.getByRole('button', { name: '次の時間帯を見る' }).waitFor({ timeout: 15_000 })
log('\n=== ③ 番組表示後の空窓 ===')
if (distinctWindows(afterProgramApi.requests).length !== 2) {
  ng.push(`③ 空窓到達後に自動取得が連鎖した（相異なる要求数 ${distinctWindows(afterProgramApi.requests).length}）`)
}
assertRequestWindow(distinctWindows(afterProgramApi.requests)[1], nowMs + WINDOW, '③ 空窓')
if ((await afterProgramPage.getByTestId('program-list-sentinel').count()) !== 0) {
  ng.push('③ 番組表示後の空窓に自動読み込みの番兵が残っている')
}
await afterProgramPage.waitForTimeout(300)
if (distinctWindows(afterProgramApi.requests).length !== 2) {
  ng.push('③ 空窓の待機中に自動取得で新しい窓が要求された')
}
await afterProgram.context.close()

// --- ④ 最終窓 ---
// `?day=2026-08-21` は別 origin（day オフセット 7 の 0 時）になるため、
// この api インスタンスの窓インデックスはその基準時刻からの相対で数える。
const finalBaseMs = new Date('2026-08-21T00:00:00+09:00').getTime()
const finalApi = createProgramApi(finalBaseMs, [[], [], [], []])
const final = await newProgramPage(browser, finalApi)
const finalPage = final.page
await finalPage.goto(URL_BASE + '/programs?day=2026-08-21', { waitUntil: 'domcontentloaded' })
await finalPage.getByText('この時間帯の番組がありません').waitFor({ timeout: 15_000 })
await waitForWindowCount(finalApi.requests, 1, '④ 最終日の初回窓')

for (let expectedWindows = 2; expectedWindows <= 4; expectedWindows++) {
  // クリック直後は「読み込み中…」に変わるので、名前で取り直して count() を
  // 見ると反映待ちの一瞬を「ボタンが無い」と誤判定する（実際に ④ が落ちた）。
  // locator.click 側の actionability 待ちに任せ、窓が増えるまで待ってから次へ。
  await finalPage
    .getByRole('button', { name: '次の時間帯を見る' })
    .click({ timeout: 15_000 })
  await waitForWindowCount(finalApi.requests, expectedWindows, `④ ${expectedWindows} 窓目`)
}
await finalPage
  .getByRole('button', { name: '次の時間帯を見る' })
  .waitFor({ state: 'detached', timeout: 15_000 })

log('\n=== ④ 最終窓 ===')
if (distinctWindows(finalApi.requests).length !== 4) {
  ng.push(`④ 最終窓までの相異なる要求数が 4 ではない（${distinctWindows(finalApi.requests).length}）`)
}
if ((await finalPage.getByRole('button', { name: '次の時間帯を見る' }).count()) !== 0) {
  ng.push('④ 最終窓でも「次の時間帯を見る」が表示されている')
}
if ((await finalPage.getByRole('button', { name: 'さらに読み込む' }).count()) !== 0) {
  ng.push('④ 最終窓でも「さらに読み込む」が表示されている')
}
await finalPage.waitForTimeout(300)
if (distinctWindows(finalApi.requests).length !== 4) {
  ng.push('④ 最終窓のあとに新しい窓の取得が発生した')
}
await final.context.close()

await finish(ng, browser)
