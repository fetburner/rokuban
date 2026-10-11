// ライブ画面の番組境界（issue #1348）の受け入れ判定。jsdom の fake timer は実ブラウザの
// タイマー・描画・fetch の順序を再現しないので、境界をまたぐ挙動は実 Chromium の
// `page.clock` で見る（e2e/README.md）。API は page.route で差し替えるので、DB・mirakc・
// チューナーは要らない。
//
// 見るのは 4 ケース。どれも 2 局構成で、局 A は長時間番組が境界をまたいで続き、局 B は
// 境界（06:10:30。分の途中に置く。分ちょうどだと窓の期限と境界が重なり、境界の窓を
// 取れていなくても毎分の窓が代わりに通ってしまう）で番組が替わる。
//   a. 通常の境界: 境界の前に境界窓が 1 回だけ取得され、境界をまたぐ 3 秒間に番組の行が
//      消えず、境界後に次の番組が出る。境界窓への要求は合計 1 回（修正前は境界後 0.9 秒に
//      約 60 回の再取得が出た）
//   b. 遅い回線: 境界窓の応答を保留したまま境界を越える。続いている局 A の行は残り、
//      局 B の終了済み番組は出ない。応答が届くと局 B の次の番組が出る
//   c. タイマー遅延: 時計を境界の 3 分後へ一度に飛ばしても、その時点の放送中番組が出る
//   d. 表示 tick が境界ちょうどに来る配置: 境界の前後 3 秒を 1ms 刻みで見ても、
//      終わった番組の行が消える／残る瞬間がない
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:live-boundary
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
const MIN = 60_000
const boundary = Date.parse('2026-10-11T06:10:30.000Z')
const iso = (ms) => new Date(ms).toISOString()
const NETWORK_ID = 32736

const services = [
  ['チャンネル A', 1024],
  ['チャンネル B', 1025],
].map(([name, serviceId]) => ({
  id: NETWORK_ID * 100_000 + serviceId,
  networkId: NETWORK_ID,
  serviceId,
  name,
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: serviceId - 1023,
  hasLogoData: false,
  hasPrograms: true,
}))

let nextProgramId = 1
function program(serviceId, name, startMs, endMs) {
  const programId = nextProgramId++
  return {
    programId,
    networkId: NETWORK_ID,
    serviceId,
    eventId: programId,
    startAt: iso(startMs),
    endAt: iso(endMs),
    durationMs: endMs - startMs,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

const A = program(1024, 'A の長時間番組', boundary - 30 * MIN, boundary + 30 * MIN)
const B_PREV = program(1025, 'B の前の番組', boundary - 30 * MIN, boundary)
const B_NEXT = program(1025, 'B の次の番組', boundary, boundary + 30 * MIN)
const B_MID = program(1025, 'B の途中の番組', boundary, boundary + 2 * MIN)
const B_LATE = program(1025, 'B の遅延後の番組', boundary + 2 * MIN, boundary + 30 * MIN)
const NORMAL = [A, B_PREV, B_NEXT]
const LATE = [A, B_PREV, B_MID, B_LATE]

const ng = []
await validateFixturesOrExit(
  [
    ...services.map((item, index) => [`services[${index}]`, ListServicesResponseItem, item]),
    ...[A, B_PREV, B_NEXT, B_MID, B_LATE].map((item) => [
      `program ${item.name}`,
      ListProgramsResponseItem,
      item,
    ]),
  ],
  ng,
)

log(`URL: ${URL_BASE}`)
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

/**
 * openScenario は fixtures で番組 API を返すページを開き、時計を止めて `boundary - leadMs`
 * まで進める。`leadMs` の端数が、1 秒刻みの表示 tick の位相を決める（境界の 0.5 秒後に
 * tick が来る配置なら 10_500、境界ちょうどなら 10_000）。
 * 窓の取得は `windowRequests`（全局一覧窓の開始時刻を要求順に積む）に記録する。
 * `hold` を true にすると、境界窓（start が boundary の全局一覧）の応答を `release()` まで保留する。
 */
async function openScenario(fixtures, { leadMs, hold = false }) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  page.on('pageerror', (error) => ng.push(`ページ例外: ${error.message}`))
  const windowRequests = []
  const holds = []
  await page.clock.install({ time: boundary - 90_500 })
  await installApiStubs(page, async ({ path: p, url, json }) => {
    if (p === '/api/sites') return json(['default'])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/sites/default/services') return json(services)
    if (p === '/api/sites/default/programs') {
      const start = Date.parse(url.searchParams.get('start') ?? '')
      const end = Date.parse(url.searchParams.get('end') ?? '')
      const serviceFilter = url.searchParams.getAll('service').map(Number)
      if (serviceFilter.length === 0) windowRequests.push(start)
      if (hold && serviceFilter.length === 0 && start === boundary) {
        await new Promise((resolve) => holds.push(resolve))
      }
      return json(
        fixtures.filter(
          (item) =>
            Date.parse(item.startAt) < end &&
            Date.parse(item.endAt) > start &&
            (serviceFilter.length === 0 || serviceFilter.includes(NETWORK_ID * 100_000 + item.serviceId)),
        ),
      )
    }
    return json([])
  })
  await page.goto(URL_BASE + '/live', { waitUntil: 'domcontentloaded' })
  await rows(page, 2, '初回表示')
  // 描画後の時計を止め、leadMs の位置へ一度に飛ぶ。飛んだ先で期限の来ていた tick は 1 回だけ
  // その時刻に走るので、以後の tick は飛んだ先から 1 秒刻みになる（位相が leadMs で決まる）。
  const now = await page.evaluate(() => Date.now())
  await page.clock.pauseAt(now + 1_000)
  await page.clock.fastForward(boundary - leadMs - (now + 1_000))
  return {
    context,
    page,
    windowRequests,
    release: () => holds.forEach((resolve) => resolve()),
  }
}

/** rowNames は一覧の各局に出ている「番組表の予定: <番組名>」の番組名を返す。 */
function rowNames(page) {
  return page.evaluate(() =>
    [...document.querySelectorAll('[aria-label^="番組表の予定: "]')]
      .map((el) => el.getAttribute('aria-label').replace('番組表の予定: ', ''))
      .sort(),
  )
}

/** rows は予定の行が `count` 個になるまで実時間で待つ。 */
async function rows(page, count, label) {
  const deadline = Date.now() + 15_000
  let names = []
  while (Date.now() < deadline) {
    names = await rowNames(page)
    if (names.length === count) return names
    await page.waitForTimeout(50)
  }
  ng.push(`${label}: 予定の行が ${count} 個にならない（${JSON.stringify(names)}）`)
  return names
}

/** settle は fake 時計を小刻みに進めつつ実時間を流し、保留中の fetch と描画を片付ける。 */
async function settle(page, steps = 6) {
  for (let i = 0; i < steps; i++) {
    await page.clock.runFor(50)
    await page.waitForTimeout(50)
  }
}

const sameNames = (actual, expected) => JSON.stringify(actual) === JSON.stringify([...expected].sort())

/**
 * crossBoundary は境界の 1.5 秒前から 1.5 秒後までを `stepMs` 刻みで進め、刻みごとに
 * 予定の行を `check(names, nowMs)` へ渡す。
 */
async function crossBoundary(page, stepMs, check) {
  const start = await page.evaluate(() => Date.now())
  for (let at = start; at < boundary + 1_500; at += stepMs) {
    await page.clock.runFor(stepMs)
    check(await rowNames(page), at + stepMs)
  }
}

/** boundaryRequests は境界時刻の全局一覧窓への要求数。 */
const boundaryRequests = (windowRequests) => windowRequests.filter((start) => start === boundary).length

// --- a. 通常の境界 ---
{
  log('\n=== a. 通常の境界 ===')
  const { context, page, windowRequests } = await openScenario(NORMAL, { leadMs: 10_500 })
  await page.clock.runFor(10_500 - 1_500)
  await settle(page)
  if (boundaryRequests(windowRequests) !== 1) {
    ng.push(`a. 境界の前に境界窓が 1 回取得されていない（${boundaryRequests(windowRequests)} 回）`)
  }
  const before = await rowNames(page)
  if (!sameNames(before, [A.name, B_PREV.name])) {
    ng.push(`a. 境界の前の表示が不正（${JSON.stringify(before)}）`)
  }
  let vanished = 0
  await crossBoundary(page, 20, (names, at) => {
    if (names.length !== 2) {
      vanished += 1
      if (vanished === 1) ng.push(`a. 境界をまたぐ間に行が ${names.length} 個になった（${iso(at)}）`)
    }
  })
  await settle(page)
  const after = await rowNames(page)
  if (!sameNames(after, [A.name, B_NEXT.name])) {
    ng.push(`a. 境界後に次の番組が出ていない（${JSON.stringify(after)}）`)
  }
  if (boundaryRequests(windowRequests) !== 1) {
    ng.push(`a. 境界窓への要求が合計 1 回でない（${boundaryRequests(windowRequests)} 回）`)
  }
  log(`  行が欠けた刻み: ${vanished}、境界窓への要求: ${boundaryRequests(windowRequests)} 回`)
  await context.close()
}

// --- b. 遅い回線（境界窓の応答が間に合わない） ---
{
  log('\n=== b. 遅い回線 ===')
  const { context, page, windowRequests, release } = await openScenario(NORMAL, {
    leadMs: 10_500,
    hold: true,
  })
  await page.clock.runFor(10_500 - 1_500)
  await settle(page)
  if (boundaryRequests(windowRequests) !== 1) {
    ng.push(`b. 境界窓の取得が始まっていない（${boundaryRequests(windowRequests)} 回）`)
  }
  let wrong = 0
  await crossBoundary(page, 20, (names, at) => {
    // 境界の後は、局 A の番組は残り、局 B の終了済み番組と次の番組（まだ届いていない）は出ない。
    const expected = at < boundary ? [A.name, B_PREV.name] : [A.name]
    if (!sameNames(names, expected)) {
      wrong += 1
      if (wrong === 1) ng.push(`b. 応答待ちの間の表示が不正（${JSON.stringify(names)}、${iso(at)}）`)
    }
  })
  if (boundaryRequests(windowRequests) !== 1) {
    ng.push(`b. 応答待ちの間に境界窓を取り直し続けた（${boundaryRequests(windowRequests)} 回）`)
  }
  release()
  await settle(page)
  const after = await rowNames(page)
  if (!sameNames(after, [A.name, B_NEXT.name])) {
    ng.push(`b. 応答後に次の番組が出ていない（${JSON.stringify(after)}）`)
  }
  log(`  応答待ちで表示が不正だった刻み: ${wrong}`)
  await context.close()
}

// --- c. タイマー遅延（時計を境界の 3 分後へ飛ばす） ---
{
  log('\n=== c. タイマー遅延 ===')
  const { context, page, windowRequests } = await openScenario(LATE, { leadMs: 10_500 })
  await page.clock.runFor(10_500 - 1_500)
  await settle(page)
  const now = await page.evaluate(() => Date.now())
  await page.clock.fastForward(boundary + 3 * MIN - now)
  await settle(page, 20)
  const after = await rowNames(page)
  if (!sameNames(after, [A.name, B_LATE.name])) {
    ng.push(`c. 3 分遅れた後の放送中番組が出ていない（${JSON.stringify(after)}）`)
  }
  log(`  全局一覧窓の要求数: ${windowRequests.length}`)
  await context.close()
}

// --- d. 表示 tick が境界ちょうどに来る配置 ---
{
  log('\n=== d. 表示 tick が境界ちょうど ===')
  const { context, page, windowRequests } = await openScenario(NORMAL, { leadMs: 10_000 })
  await page.clock.runFor(10_000 - 1_500)
  await settle(page)
  let bad = 0
  await crossBoundary(page, 1, (names, at) => {
    // 境界より前は前の番組、境界以後は次の番組。どちらでも 2 行で、終わった番組は出ない。
    const expected = at < boundary ? [A.name, B_PREV.name] : [A.name, B_NEXT.name]
    if (!sameNames(names, expected)) {
      bad += 1
      if (bad === 1) ng.push(`d. 境界ちょうど付近の表示が不正（${JSON.stringify(names)}、${iso(at)}）`)
    }
  })
  if (boundaryRequests(windowRequests) !== 1) {
    ng.push(`d. 境界窓への要求が合計 1 回でない（${boundaryRequests(windowRequests)} 回）`)
  }
  log(`  表示が不正だった刻み: ${bad}`)
  await context.close()
}

await finish(ng, browser)
