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
const recoveryStartMs = nowMs + 2 * 6 * HOUR
const recoveryPrograms = Array.from({ length: 24 }, (_, index) =>
  program(683001 + index, recoveryStartMs + index * QUARTER, `空窓から復旧した番組${index}`),
)
const populatedFirstWindow = Array.from({ length: 24 }, (_, index) =>
  program(683100 + index, nowMs + index * QUARTER, `空窓の前に表示された番組${index}`),
)

const ng = []

function createProgramApi(sequence) {
  const requests = []

  const handler = async ({ path: p, url, json, route }) => {
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/reservations') return json([])
    if (p === '/api/sites/default/services') return json([service])
    if (p === '/api/encode-profiles') return json([])
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (p === '/api/sites/default/programs') {
      requests.push({
        startMs: Date.parse(url.searchParams.get('start') ?? ''),
        endMs: Date.parse(url.searchParams.get('end') ?? ''),
      })
      const response = sequence[requests.length - 1] ?? []
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

  return { handler, requests }
}

async function waitForRequestCount(requests, expected, label) {
  const deadline = Date.now() + 15_000
  while (requests.length < expected && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  if (requests.length < expected) {
    ng.push(`${label}: 番組 API の要求が ${expected} 回に達しない（実際 ${requests.length} 回）`)
  }
}

function assertRequestWindow(request, expectedStartMs, label) {
  if (!request) {
    ng.push(`${label}: 番組 API の要求が記録されていない`)
    return
  }
  if (request.startMs !== expectedStartMs || request.endMs !== expectedStartMs + 6 * HOUR) {
    ng.push(
      `${label}: 時間窓が不正（${iso(request.startMs)} - ${iso(request.endMs)}。期待 ${iso(expectedStartMs)} - ${iso(expectedStartMs + 6 * HOUR)}）`,
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
const recoveryApi = createProgramApi([[], [], 'error', recoveryPrograms])
const recovery = await newProgramPage(browser, recoveryApi)
const recoveryPage = recovery.page
await recoveryPage.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })

const emptyState = recoveryPage.getByText('この時間帯の番組がありません')
const nextWindowButton = recoveryPage.getByRole('button', { name: '次の時間帯を見る' })

await emptyState.waitFor({ timeout: 15_000 })
await waitForRequestCount(recoveryApi.requests, 1, '① 初回窓')
log('\n=== ① 最初の空窓 ===')
if (recoveryApi.requests.length !== 1) ng.push(`① 初回窓の要求数が 1 ではない（${recoveryApi.requests.length}）`)
assertRequestWindow(recoveryApi.requests[0], nowMs, '① 初回窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('① 最初の空窓に「次の時間帯を見る」が 1 個表示されない')
}
if ((await recoveryPage.getByTestId('program-list-sentinel').count()) !== 0) {
  ng.push('① 最初の空窓に自動読み込みの番兵が残っている')
}

await nextWindowButton.click()
await waitForRequestCount(recoveryApi.requests, 2, '① 2 番目の窓')
await emptyState.waitFor()
if (recoveryApi.requests.length !== 2) ng.push(`① 空窓 2 回目の要求後に要求が連鎖した（${recoveryApi.requests.length}）`)
assertRequestWindow(recoveryApi.requests[1], nowMs + 6 * HOUR, '① 2 番目の窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('① 連続する空窓に「次の時間帯を見る」が残らない')
}

await nextWindowButton.click()
await waitForRequestCount(recoveryApi.requests, 3, '② 失敗する窓')
await recoveryPage.getByText('続きの取得に失敗しました').waitFor({ timeout: 15_000 })
log('\n=== ② 後続窓の取得失敗 ===')
if (recoveryApi.requests.length !== 3) ng.push(`② 失敗時の要求数が 3 ではない（${recoveryApi.requests.length}）`)
assertRequestWindow(recoveryApi.requests[2], nowMs + 2 * 6 * HOUR, '② 失敗する窓')
if ((await nextWindowButton.count()) !== 1) {
  ng.push('② 取得失敗後に同じ再試行ボタンが表示されない')
}
await recoveryPage.waitForTimeout(300)
if (recoveryApi.requests.length !== 3) ng.push('② 取得失敗後に自動再試行が発生した')

await nextWindowButton.click()
await waitForRequestCount(recoveryApi.requests, 4, '② 再試行')
await recoveryPage.getByText('空窓から復旧した番組0').waitFor({ timeout: 15_000 })
log('\n=== ② 手動再試行の成功 ===')
if (recoveryApi.requests.length !== 4) ng.push(`② 再試行後の要求数が 4 ではない（${recoveryApi.requests.length}）`)
assertRequestWindow(recoveryApi.requests[3], nowMs + 2 * 6 * HOUR, '② 再試行')
if ((await recoveryPage.getByText('この時間帯の番組がありません').count()) !== 0) {
  ng.push('② 番組取得成功後も空窓の表示が残っている')
}
if ((await recoveryPage.getByRole('button', { name: 'さらに読み込む' }).count()) !== 0) {
  ng.push('② 自動読み込み可能な通常窓に手動の「さらに読み込む」が表示された')
}
await recoveryPage.waitForTimeout(300)
if (recoveryApi.requests.length !== 4) ng.push('② 復旧した通常窓から予期しない追加取得が発生した')
await recovery.context.close()

// --- ③ 番組表示後に空窓へ進んだ場合も、自動連鎖を止める ---
const afterProgramApi = createProgramApi([populatedFirstWindow, []])
const afterProgram = await newProgramPage(browser, afterProgramApi)
const afterProgramPage = afterProgram.page
await afterProgramPage.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
await afterProgramPage.getByText('空窓の前に表示された番組0').waitFor({ timeout: 15_000 })
await waitForRequestCount(afterProgramApi.requests, 1, '③ 初回の番組あり窓')

// 初回窓は十分な行数があるので、末尾までスクロールすれば既存の自動読み込みを
// 発火できる。2 窓目は空で返し、その後に 3 窓目へ自動で進まないことを見る。
await afterProgramPage.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight))
await waitForRequestCount(afterProgramApi.requests, 2, '③ 空窓')
await afterProgramPage.getByRole('button', { name: '次の時間帯を見る' }).waitFor({ timeout: 15_000 })
log('\n=== ③ 番組表示後の空窓 ===')
if (afterProgramApi.requests.length !== 2) {
  ng.push(`③ 空窓到達後に自動取得が連鎖した（要求数 ${afterProgramApi.requests.length}）`)
}
assertRequestWindow(afterProgramApi.requests[1], nowMs + 6 * HOUR, '③ 空窓')
if ((await afterProgramPage.getByTestId('program-list-sentinel').count()) !== 0) {
  ng.push('③ 番組表示後の空窓に自動読み込みの番兵が残っている')
}
await afterProgramPage.waitForTimeout(300)
if (afterProgramApi.requests.length !== 2) ng.push('③ 空窓の待機中に自動取得が発生した')
await afterProgram.context.close()

// --- ④ 最終窓 ---
const finalApi = createProgramApi([[], [], [], []])
const final = await newProgramPage(browser, finalApi)
const finalPage = final.page
await finalPage.goto(URL_BASE + '/programs?day=2026-08-21', { waitUntil: 'domcontentloaded' })
await finalPage.getByText('この時間帯の番組がありません').waitFor({ timeout: 15_000 })
await waitForRequestCount(finalApi.requests, 1, '④ 最終日の初回窓')

for (let expectedRequests = 2; expectedRequests <= 4; expectedRequests++) {
  const button = finalPage.getByRole('button', { name: '次の時間帯を見る' })
  if ((await button.count()) !== 1) {
    ng.push(`④ 最終日の ${expectedRequests - 1} 窓目の後に次の時間帯ボタンが無い`)
    break
  }
  await button.click()
  await waitForRequestCount(finalApi.requests, expectedRequests, `④ ${expectedRequests} 窓目`)
}
await finalPage
  .getByRole('button', { name: '次の時間帯を見る' })
  .waitFor({ state: 'detached', timeout: 15_000 })

log('\n=== ④ 最終窓 ===')
if (finalApi.requests.length !== 4) ng.push(`④ 最終窓までの要求数が 4 ではない（${finalApi.requests.length}）`)
if ((await finalPage.getByRole('button', { name: '次の時間帯を見る' }).count()) !== 0) {
  ng.push('④ 最終窓でも「次の時間帯を見る」が表示されている')
}
if ((await finalPage.getByRole('button', { name: 'さらに読み込む' }).count()) !== 0) {
  ng.push('④ 最終窓でも「さらに読み込む」が表示されている')
}
await finalPage.waitForTimeout(300)
if (finalApi.requests.length !== 4) ng.push('④ 最終窓のあとに追加取得が発生した')
await final.context.close()

await finish(ng, browser)
