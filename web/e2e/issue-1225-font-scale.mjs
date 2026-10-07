// Typography scale regression gate for issue #1225.
//
// jsdom does not resolve Tailwind utilities or model browser input rendering. This gate reads
// computed sizes from the live browser for the compact labels in both time-by-width displays
// and for mobile form controls at a 390px viewport.
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:issue-1225-font-scale
//
// The iPhone Safari focus-zoom check remains a physical-device follow-up.
import {
  ListCapacityOveragesResponseItem,
  ListProgramsResponseItem,
  ListRecordingsResponseItem,
  ListReservationsResponseItem,
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
const SECOND_SITE = 'secondary'
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const nowMs = FIXED_NOW.getTime()
const HOUR = 3_600_000
const iso = (ms) => new Date(ms).toISOString()
const ng = []

const services = {
  [SITE]: {
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
  [SECOND_SITE]: {
    id: 3273701032,
    networkId: 32737,
    serviceId: 1032,
    name: 'NHK Eテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
    hasPrograms: true,
  },
}

function program(programId, service, startOffsetMinutes, durationMinutes, name, intent) {
  const startMs = nowMs + startOffsetMinutes * 60_000
  return {
    programId,
    networkId: service.networkId,
    serviceId: service.serviceId,
    eventId: programId,
    startAt: iso(startMs),
    endAt: iso(startMs + durationMinutes * 60_000),
    durationMs: durationMinutes * 60_000,
    name,
    description: '番組表セル内の説明文。',
    genres: [0],
    isFree: true,
    ...(intent === undefined ? {} : { intent }),
  }
}

const reservedProgram = program(1_225_001, services[SITE], 30, 30, '予約済み番組')
const skippedProgram = program(1_225_002, services[SITE], 60, 30, 'スキップ中の番組', 'skip')
const secondaryProgram = program(1_225_003, services[SECOND_SITE], 30, 30, '別サイトの番組')
const programs = [reservedProgram, skippedProgram]

const reservation = {
  id: reservedProgram.programId,
  site: SITE,
  programId: reservedProgram.programId,
  source: 'manual',
  state: 'active',
  title: reservedProgram.name,
  serviceName: services[SITE].name,
  channelType: services[SITE].channelType,
  startAt: reservedProgram.startAt,
  durationMs: reservedProgram.durationMs,
  createdAt: iso(nowMs - HOUR),
  updatedAt: iso(nowMs - HOUR),
  series: null,
  skip: false,
}

const recording = {
  id: 1_225_004,
  site: SITE,
  source: 'manual',
  serviceName: services[SITE].name,
  channelType: services[SITE].channelType,
  channel: services[SITE].channel,
  networkId: services[SITE].networkId,
  serviceId: services[SITE].serviceId,
  eventId: 1_225_004,
  title: '時間軸の録画',
  startAt: iso(nowMs - 30 * 60_000),
  durationMs: HOUR,
  status: 'recording',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: iso(nowMs - 30 * 60_000),
  startedAt: iso(nowMs - 30 * 60_000),
}

const overage = {
  site: SITE,
  startAt: iso(nowMs + 30 * 60_000),
  endAt: iso(nowMs + 90 * 60_000),
  shortfall: 1,
  jammedTypes: ['GR'],
}

await validateFixturesOrExit(
  [
    ...Object.entries(services).map(([site, item]) => [`${site} service`, ListServicesResponseItem, item]),
    ...[...programs, secondaryProgram].map((item) => [`program ${item.programId}`, ListProgramsResponseItem, item]),
    ['reservation', ListReservationsResponseItem, reservation],
    ['recording', ListRecordingsResponseItem, recording],
    ['overage', ListCapacityOveragesResponseItem, overage],
  ],
  ng,
)
await verifyBundleMatchesOrExit(BASE, ng)

const apiHandler = async ({ path, url, json, route }) => {
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/sites') return json([SITE, SECOND_SITE])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/breakers') return json([])
  if (path === '/api/reservations') return json([reservation])
  if (path === '/api/capacity/overages') return json([overage])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/recordings/continue-watching') return json([])
  if (path === '/api/recordings') {
    return json(url.searchParams.get('status') === 'recording' ? [recording] : [])
  }
  const servicesMatch = /^\/api\/sites\/([^/]+)\/services$/.exec(path)
  if (servicesMatch !== null) return json([services[servicesMatch[1]]].filter(Boolean))
  const programsMatch = /^\/api\/sites\/([^/]+)\/programs$/.exec(path)
  if (programsMatch !== null) {
    return json(programsMatch[1] === SITE ? programs : [secondaryProgram])
  }
  if (/\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(path)) return json({ extended: {}, audios: [] })
  if (path === '/api/storage') return json([])
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  return json([])
}

const browser = await launchBrowser()

log('\n=== 390px fine-pointer: input font size ===')
const narrowContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 3,
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const narrowPage = await narrowContext.newPage()
await narrowPage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(narrowPage, apiHandler)
await narrowPage.goto(`${BASE}/search`, { waitUntil: 'domcontentloaded' })
const narrowInput = narrowPage.getByRole('textbox', { name: 'テキスト条件 1 の値' })
await narrowInput.waitFor({ timeout: 10_000 })
const pointerTypes = await narrowPage.evaluate(() => ({
  fine: matchMedia('(pointer: fine)').matches,
  coarse: matchMedia('(pointer: coarse)').matches,
}))
log(`  pointer: fine=${pointerTypes.fine}, coarse=${pointerTypes.coarse}`)
if (!pointerTypes.fine || pointerTypes.coarse) ng.push('390px: fine-pointer 条件ではない')
const inputSizes = await narrowPage.locator('input, textarea, select').evaluateAll((elements) =>
  elements
    .filter((element) => {
      const rect = element.getBoundingClientRect()
      return rect.width > 0 && rect.height > 0
    })
    .map((element) => ({
      label: element.getAttribute('aria-label') ?? element.getAttribute('type') ?? element.tagName,
      fontSize: Number.parseFloat(getComputedStyle(element).fontSize),
    })),
)
const minInputSize = Math.min(...inputSizes.map(({ fontSize }) => fontSize))
log(`  390×844 / 入力欄 ${inputSizes.length} 件 / 最小 ${minInputSize}px`)
if (inputSizes.length === 0) ng.push('390px: 表示された入力欄がない')
for (const input of inputSizes) {
  if (input.fontSize < 16) ng.push(`390px: ${input.label} が ${input.fontSize}px（16px 未満）`)
}
await narrowContext.close()

log('\n=== 1280px desktop: form controls keep their base size ===')
const desktopContext = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const desktopPage = await desktopContext.newPage()
await desktopPage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(desktopPage, apiHandler)
await desktopPage.goto(`${BASE}/search`, { waitUntil: 'domcontentloaded' })
await desktopPage.getByRole('textbox', { name: 'テキスト条件 1 の値' }).waitFor({ timeout: 10_000 })
const desktopInputSizes = await desktopPage.locator('input, textarea, select').evaluateAll((elements) =>
  elements
    .filter((element) => {
      const rect = element.getBoundingClientRect()
      return rect.width > 0 && rect.height > 0
    })
    .map((element) => ({
      label: element.getAttribute('aria-label') ?? element.getAttribute('type') ?? element.tagName,
      fontSize: Number.parseFloat(getComputedStyle(element).fontSize),
    })),
)
const minDesktopInputSize = Math.min(...desktopInputSizes.map(({ fontSize }) => fontSize))
log(`  1280×900 / 入力欄 ${desktopInputSizes.length} 件 / 最小 ${minDesktopInputSize}px`)
if (desktopInputSizes.length === 0) ng.push('1280px: 表示された入力欄がない')
for (const input of desktopInputSizes) {
  if (input.fontSize !== 14) {
    ng.push(`1280px: ${input.label} が ${input.fontSize}px（既定の 14px でない）`)
  }
}
await desktopContext.close()

async function checkFormControlBreakpoint(width, expectation) {
  const context = await browser.newContext({
    viewport: { width, height: 844 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler)
  await page.goto(`${BASE}/search`, { waitUntil: 'domcontentloaded' })
  await page.getByRole('textbox', { name: 'テキスト条件 1 の値' }).waitFor({ timeout: 10_000 })
  const pointerTypes = await page.evaluate(() => ({
    fine: matchMedia('(pointer: fine)').matches,
    coarse: matchMedia('(pointer: coarse)').matches,
  }))
  if (!pointerTypes.fine || pointerTypes.coarse) {
    ng.push(`${width}px: fine-pointer 条件ではない`)
  }
  const sizes = await page.locator('input, textarea, select').evaluateAll((elements) =>
    elements
      .filter((element) => {
        const rect = element.getBoundingClientRect()
        return rect.width > 0 && rect.height > 0
      })
      .map((element) => ({
        label: element.getAttribute('aria-label') ?? element.getAttribute('type') ?? element.tagName,
        fontSize: Number.parseFloat(getComputedStyle(element).fontSize),
      })),
  )
  const minSize = Math.min(...sizes.map(({ fontSize }) => fontSize))
  log(`  ${width}×844 fine-pointer / 入力欄 ${sizes.length} 件 / 最小 ${minSize}px`)
  if (sizes.length === 0) ng.push(`${width}px: 表示された入力欄がない`)
  for (const input of sizes) {
    if ('minimum' in expectation && input.fontSize < expectation.minimum) {
      ng.push(`${width}px: ${input.label} が ${input.fontSize}px（${expectation.minimum}px 未満）`)
    }
    if ('exact' in expectation && input.fontSize !== expectation.exact) {
      ng.push(`${width}px: ${input.label} が ${input.fontSize}px（${expectation.exact}px でない）`)
    }
  }
  await context.close()
}

log('\n=== md breakpoint: 767px is mobile, 768px is desktop ===')
await checkFormControlBreakpoint(767, { minimum: 16 })
await checkFormControlBreakpoint(768, { exact: 14 })

log('\n=== program grid: compact labels ===')
const gridContext = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const gridPage = await gridContext.newPage()
await gridPage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(gridPage, apiHandler)
await gridPage.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })
await gridPage.getByTestId('program-grid').waitFor({ timeout: 15_000 })

async function checkMinimumFont(locator, label, minimumPx) {
  try {
    const target = locator.first()
    await target.waitFor({ timeout: 10_000 })
    const fontSize = await target.evaluate((element) =>
      Number.parseFloat(getComputedStyle(element).fontSize),
    )
    log(`  ${label}: ${fontSize}px`)
    if (fontSize < minimumPx) ng.push(`${label} が ${fontSize}px（${minimumPx}px 未満）`)
  } catch {
    ng.push(`${label}: 対象が描画されない`)
  }
}

async function checkExactFont(locator, label, expectedPx) {
  try {
    const target = locator.first()
    await target.waitFor({ timeout: 10_000 })
    const fontSize = await target.evaluate((element) =>
      Number.parseFloat(getComputedStyle(element).fontSize),
    )
    log(`  ${label}: ${fontSize}px`)
    if (fontSize !== expectedPx) {
      ng.push(`${label} が ${fontSize}px（${expectedPx}px でない）`)
    }
  } catch {
    ng.push(`${label}: 対象が描画されない`)
  }
}

await checkExactFont(
  gridPage.getByTestId('genre-filter-0'),
  'ジャンル凡例（移行後の text-xs）',
  12,
)
await checkMinimumFont(
  gridPage.getByTestId('program-grid-header-remote-control-key'),
  '番組表のリモコン番号',
  11,
)
await checkMinimumFont(gridPage.getByTestId('program-grid-header-site'), '番組表の site 名', 11)
await checkMinimumFont(gridPage.getByTestId('program-grid-tick'), '番組表の時刻目盛り', 11)
await checkMinimumFont(gridPage.getByTestId('program-grid-cell-reserved-label'), '番組表の予約印', 11)
await checkMinimumFont(gridPage.getByTestId('program-grid-cell-skip-intent-badge'), '番組表のスキップ印', 11)
await checkMinimumFont(gridPage.getByTestId('program-grid-cell-time'), '番組表セルの開始時刻', 11)
await checkMinimumFont(
  gridPage.getByTestId('program-grid-now-label').locator('span'),
  '番組表の「いま」',
  11,
)
await gridContext.close()

log('\n=== home operations timeline: time-by-width labels ===')
const homeContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  deviceScaleFactor: 3,
  isMobile: true,
  hasTouch: true,
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const homePage = await homeContext.newPage()
await homePage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(homePage, apiHandler)
await homePage.goto(`${BASE}/?mode=ops`, { waitUntil: 'domcontentloaded' })
await homePage.getByTestId('home-ops-timeline').waitFor({ timeout: 15_000 })
await homePage.getByTestId('home-timeline-block').first().waitFor({ timeout: 10_000 })
await checkMinimumFont(homePage.getByTestId('home-timeline-tick'), 'ホーム時間軸の目盛り', 11)
await checkMinimumFont(homePage.getByTestId('home-timeline-now-label'), 'ホーム時間軸の「いま」', 11)
await checkMinimumFont(homePage.getByTestId('home-timeline-block').first(), 'ホーム時間軸の録画ブロック', 11)
await checkMinimumFont(homePage.getByTestId('home-overage-label'), 'ホーム時間軸の不足札', 11)
await homeContext.close()

await finish(ng, browser)
