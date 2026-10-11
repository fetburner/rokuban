// 正時に grid queryKey が変わっても、表示中の番組表とスクロール位置を保つ判定。
//
// API 応答を保留したまま実時刻だけ正時の先へ進め、その後の操作で ProgramsPage
// を再レンダーさせる。時計だけ進めてもキーは変わらないため、操作を挟む。
// jsdom は scrollTop と実レイアウトを測れないので、CI ではなく実 Chromium で見る。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-grid-hour-boundary
import {
  ListCapacityOveragesResponseItem,
  ListProgramsResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const HOUR = 3_600_000
const FIXED_NOW = new Date('2026-08-14T12:59:35+09:00')
const initialOrigin = new Date(FIXED_NOW)
initialOrigin.setMinutes(0, 0, 0)
const nextOrigin = initialOrigin.getTime() + HOUR
const nextOriginISO = new Date(nextOrigin).toISOString()
const nowAfterBoundary = new Date(nextOrigin + 20 * 60_000)
const midnightOrigin = new Date(nextOrigin)
midnightOrigin.setDate(midnightOrigin.getDate() + 1)
midnightOrigin.setHours(0, 0, 0, 0)
const midnightOriginISO = midnightOrigin.toISOString()
const nowAfterMidnight = new Date(midnightOrigin.getTime() + 20 * 60_000)
const iso = (ms) => new Date(ms).toISOString()
const ng = []

const service = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

const program = (programId, startMs, durationMs, name) => ({
  programId,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: programId,
  startAt: iso(startMs),
  endAt: iso(startMs + durationMs),
  durationMs,
  name,
  description: '',
  genres: [7],
  isFree: true,
})

const programs = Array.from({ length: 24 }, (_, index) =>
  program(133900 + index, initialOrigin.getTime() + index * HOUR, HOUR, `番組 ${index}`),
)
const endedAfterBoundary = program(133999, nextOrigin - 15 * 60_000, 30 * 60_000, '正時後に終了した番組')
programs.push(endedAfterBoundary)

const overage = {
  site: SITE,
  startAt: iso(nextOrigin + 2.25 * HOUR),
  endAt: iso(nextOrigin + 3.25 * HOUR),
  shortfall: 1,
  jammedTypes: ['BS'],
}

await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ...programs.map((item) => [`program ${item.programId}`, ListProgramsResponseItem, item]),
    ['overage', ListCapacityOveragesResponseItem, overage],
  ],
  ng,
)
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const page = await browser.newPage({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
let holdBoundaryPrograms = false
let holdBoundaryOverages = false
let releasePrograms
let releaseOverages
let holdMidnightPrograms = false
let holdMidnightOverages = false
let releaseMidnightPrograms
let releaseMidnightOverages

async function apiHandler({ path, url, json, route }) {
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: false })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/reservations') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/capacity/overages') {
    if (holdMidnightOverages && url.searchParams.get('start') === midnightOriginISO) {
      holdMidnightOverages = false
      await new Promise((resolve) => { releaseMidnightOverages = resolve })
    }
    if (holdBoundaryOverages && url.searchParams.get('start') === nextOriginISO) {
      holdBoundaryOverages = false
      await new Promise((resolve) => { releaseOverages = resolve })
    }
    return json([overage])
  }
  if (path === `/api/sites/${SITE}/services`) return json([service])
  if (path === `/api/sites/${SITE}/programs`) {
    if (holdMidnightPrograms && url.searchParams.get('start') === midnightOriginISO) {
      holdMidnightPrograms = false
      await new Promise((resolve) => { releaseMidnightPrograms = resolve })
    }
    if (holdBoundaryPrograms && url.searchParams.get('start') === nextOriginISO) {
      holdBoundaryPrograms = false
      await new Promise((resolve) => { releasePrograms = resolve })
    }
    const start = Date.parse(url.searchParams.get('start') ?? '')
    const end = Date.parse(url.searchParams.get('end') ?? '')
    return json(programs.filter((item) => Date.parse(item.endAt) > start && Date.parse(item.startAt) < end))
  }
  if (path === '/api/programs/search') return json([])
  if (/\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  return json([])
}

await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)
await page.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

const grid = page.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 }).catch(() => ng.push('初期グリッドが描画されない'))
await page.locator(`[data-testid="program-grid-cell"][data-program-id="${endedAfterBoundary.programId}"]`).waitFor({ timeout: 10000 }).catch(() => {
  ng.push('正時後の番組セルが初期表示されない')
})
await page.getByTestId('capacity-band').waitFor({ timeout: 10000 }).catch(() => {
  ng.push('初期のチューナー不足帯が描画されない')
})

if ((await grid.count()) > 0) {
  await grid.evaluate((element) => { element.scrollTop = 300 })
  const before = await grid.evaluate((element) => element.scrollTop)
  const anchor = page.locator('[data-testid="program-grid-cell"][data-program-id="133903"]')
  await anchor.waitFor({ timeout: 10000 }).catch(() => ng.push('スクロール位置の番組アンカーが描画されない'))
  const anchorTopBefore = await page.evaluate(() => {
    const gridElement = document.querySelector('[data-testid="program-grid"]')
    const cell = document.querySelector('[data-testid="program-grid-cell"][data-program-id="133903"]')
    if (!gridElement || !cell) return null
    return cell.getBoundingClientRect().top - gridElement.getBoundingClientRect().top
  })
  log(`正時前: scrollTop=${before}px / 番組アンカーの画面内位置=${anchorTopBefore}px`)
  if (before < 250) ng.push(`判定位置までスクロールできない（${before}px）`)

  const isBoundaryPrograms = (request) => {
    const url = new URL(request.url())
    return url.pathname === `/api/sites/${SITE}/programs` && url.searchParams.get('start') === nextOriginISO
  }
  const isBoundaryOverages = (request) => {
    const url = new URL(request.url())
    return url.pathname === '/api/capacity/overages' && url.searchParams.get('start') === nextOriginISO
  }
  const programsRequest = page.waitForRequest(isBoundaryPrograms, { timeout: 10000 })
  const overagesRequest = page.waitForRequest(isBoundaryOverages, { timeout: 10000 })
  holdBoundaryPrograms = true
  holdBoundaryOverages = true
  const clockBefore = await page.evaluate(() => Date.now())
  await page.clock.setFixedTime(nowAfterBoundary)
  const clockAfter = await page.evaluate(() => Date.now())
  log(`操作前のブラウザ時計: ${iso(clockBefore)} → ${iso(clockAfter)}`)
  // genre 操作が ProgramsPage を再レンダーさせ、Date.now() 由来の queryKey を更新する。
  await page.getByTestId('genre-filter-7').click()
  await Promise.all([programsRequest, overagesRequest]).catch(() => {
    ng.push('正時後の番組・超過帯の再取得が始まらない')
  })

  const during = await page.evaluate(() => {
    const gridElement = document.querySelector('[data-testid="program-grid"]')
    const band = document.querySelector('[data-testid="capacity-band"]')
    const currentTime = document.querySelector('[data-testid="program-grid-now-time"]')
    const firstTick = document.querySelector('[data-testid="program-grid-tick"]')
    const endedCell = document.querySelector('[data-program-id="133999"]')
    return {
      gridPresent: gridElement !== null,
      scrollTop: gridElement?.scrollTop ?? null,
      anchorTop: (() => {
        const cell = document.querySelector('[data-testid="program-grid-cell"][data-program-id="133903"]')
        return gridElement && cell
          ? cell.getBoundingClientRect().top - gridElement.getBoundingClientRect().top
          : null
      })(),
      bandPresent: band !== null,
      bandVisible: (() => {
        if (!band || !gridElement) return false
        const bandRect = band.getBoundingClientRect()
        const gridRect = gridElement.getBoundingClientRect()
        const bandStyle = getComputedStyle(band)
        const visibleTop = gridRect.top + 40 // sticky header + border
        return (
          bandRect.height > 0 &&
          bandStyle.display !== 'none' &&
          bandStyle.visibility !== 'hidden' &&
          Number(bandStyle.opacity) > 0 &&
          bandRect.bottom > visibleTop &&
          bandRect.top < gridRect.bottom
        )
      })(),
      currentTime: currentTime?.textContent?.trim() ?? null,
      currentLineTop: Number.parseFloat(document.querySelector('[data-testid="program-grid-now-line"]')?.style.top ?? ''),
      firstTick: firstTick?.textContent?.trim() ?? null,
      ended: endedCell?.getAttribute('data-ended') ?? null,
    }
  })
  log(`正時後、応答保留中: ${JSON.stringify(during)} / アンカー前=${anchorTopBefore}px`)
  if (!during.gridPresent) ng.push('再取得中にグリッドが外れた（ListSkeleton へ戻った）')
  if (
    anchorTopBefore === null ||
    during.anchorTop === null ||
    Math.abs(during.anchorTop - anchorTopBefore) > 1
  ) {
    ng.push(`再取得中に同じ番組の画面位置が ${anchorTopBefore}px から ${during.anchorTop}px へ変わった`)
  }
  if (!during.bandPresent || !during.bandVisible) ng.push('再取得中にチューナー不足帯が消えた')
  if (during.currentTime !== '13:20') ng.push(`現在時刻が新しい時刻へ進まない（${during.currentTime}）`)
  if (
    !Number.isFinite(during.currentLineTop) ||
    Math.abs(during.currentLineTop - 40) > 0.1
  ) {
    ng.push(`現在時刻線の位置が新しい時刻へ進まない（${during.currentLineTop}）`)
  }
  if (during.firstTick !== '13:00') ng.push(`時間軸の起点が新しい正時へ進まない（${during.firstTick}）`)
  if (during.ended !== 'true') ng.push(`正時後の番組が新しい時刻で放送終了扱いにならない（${during.ended}）`)

  const programsResponse = page.waitForResponse((response) => isBoundaryPrograms(response.request()), { timeout: 10000 })
  const overagesResponse = page.waitForResponse((response) => isBoundaryOverages(response.request()), { timeout: 10000 })
  releasePrograms?.()
  releaseOverages?.()
  await Promise.all([programsResponse, overagesResponse]).catch(() => {})

  const midnightDate = page.locator('[role="group"][aria-label="日付"] button[aria-current="date"]')
  const dateBeforeMidnight = await midnightDate.getAttribute('aria-label')
  const isMidnightPrograms = (request) => {
    const url = new URL(request.url())
    return url.pathname === `/api/sites/${SITE}/programs` && url.searchParams.get('start') === midnightOriginISO
  }
  const isMidnightOverages = (request) => {
    const url = new URL(request.url())
    return url.pathname === '/api/capacity/overages' && url.searchParams.get('start') === midnightOriginISO
  }
  const midnightProgramsRequest = page.waitForRequest(isMidnightPrograms, { timeout: 10000 })
  const midnightOveragesRequest = page.waitForRequest(isMidnightOverages, { timeout: 10000 })
  holdMidnightPrograms = true
  holdMidnightOverages = true
  await page.clock.setFixedTime(nowAfterMidnight)
  await page.getByTestId('genre-filter-7').click()
  await Promise.all([midnightProgramsRequest, midnightOveragesRequest]).catch(() => {
    ng.push('日付変更後の番組・超過帯の再取得が始まらない')
  })

  const afterMidnight = await page.evaluate(() => ({
    gridPresent: document.querySelector('[data-testid="program-grid"]') !== null,
    firstTick: document.querySelector('[data-testid="program-grid-tick"]')?.textContent?.trim() ?? null,
    currentTime: document.querySelector('[data-testid="program-grid-now-time"]')?.textContent?.trim() ?? null,
    currentLineTop: Number.parseFloat(document.querySelector('[data-testid="program-grid-now-line"]')?.style.top ?? ''),
    currentDate: document.querySelector('[role="group"][aria-label="日付"] button[aria-current="date"]')?.getAttribute('aria-label') ?? null,
  }))
  log(`日付変更後、応答保留中: ${JSON.stringify(afterMidnight)} / 前=${dateBeforeMidnight}`)
  if (!afterMidnight.gridPresent) ng.push('日付変更後の再取得中にグリッドが外れた')
  if (afterMidnight.firstTick !== '00:00') ng.push(`日付変更後の時間軸起点が進まない（${afterMidnight.firstTick}）`)
  if (afterMidnight.currentTime !== '00:20') ng.push(`日付変更後の現在時刻が進まない（${afterMidnight.currentTime}）`)
  if (
    !Number.isFinite(afterMidnight.currentLineTop) ||
    Math.abs(afterMidnight.currentLineTop - 40) > 0.1
  ) {
    ng.push(`日付変更後の現在時刻線位置が進まない（${afterMidnight.currentLineTop}）`)
  }
  if (
    !dateBeforeMidnight?.startsWith('8月14日(金)') ||
    !afterMidnight.currentDate?.startsWith('8月15日(土)')
  ) {
    ng.push(`日付見出しが暦日変更に追従しない（${dateBeforeMidnight} → ${afterMidnight.currentDate}）`)
  }

  const midnightProgramsResponse = page.waitForResponse((response) => isMidnightPrograms(response.request()), { timeout: 10000 })
  const midnightOveragesResponse = page.waitForResponse((response) => isMidnightOverages(response.request()), { timeout: 10000 })
  releaseMidnightPrograms?.()
  releaseMidnightOverages?.()
  await Promise.all([midnightProgramsResponse, midnightOveragesResponse]).catch(() => {})
}

await finish(ng, browser)
