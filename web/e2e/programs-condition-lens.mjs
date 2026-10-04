// 条件レンズの往復・API 一致判定・非一致セルの可読性を実ブラウザで確認する（issue #1098）。
//
// jsdom はレイアウトも色も測れない。ここでは Chromium の実描画から文字と背景色を
// canvas で画素化し、非一致セルのコントラストを測る。API が返さない同ジャンル番組を
// 混ぜ、ジャンルだけをクライアントで判定する実装も通らないようにする。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-condition-lens

import {
  ListProgramsResponseItem,
  ListReservationsResponseItem,
  ListServicesResponseItem,
  SearchProgramsResponseItem,
} from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const HOUR = 3_600_000
// CI runner と開発端末で Node の system timezone が違っても、ブラウザの Asia/Tokyo
// fixture が同じになるよう、ローカル壁時計は offset 付きで明示する。
const origin = new Date('2026-08-12T21:00:00+09:00')
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

// どちらもアニメ・特撮だが、検索 API は一方だけを返す。
const apiMatchProgram = {
  programId: 109801,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 1,
  startAt: iso(new Date('2026-08-12T23:40:00+09:00').getTime()),
  endAt: iso(new Date('2026-08-12T23:40:00+09:00').getTime() + HOUR),
  durationMs: HOUR,
  name: 'API が返すアニメ',
  description: '',
  genres: [7],
  isFree: true,
}
const omittedProgram = {
  ...apiMatchProgram,
  programId: 109802,
  eventId: 2,
  startAt: iso(origin.getTime() + 2 * HOUR),
  endAt: iso(origin.getTime() + 3 * HOUR),
  durationMs: HOUR,
  name: 'API が返さないアニメ',
}
const skippedOmittedProgram = {
  ...omittedProgram,
  programId: 109803,
  eventId: 3,
  startAt: iso(new Date('2026-08-13T01:30:00+09:00').getTime()),
  endAt: iso(new Date('2026-08-13T02:30:00+09:00').getTime()),
  name: 'API が返さないスキップ番組',
  intent: 'skip',
}

const searchMatch = {
  site: SITE,
  programId: apiMatchProgram.programId,
  networkId: service.networkId,
  serviceId: service.serviceId,
  startAt: apiMatchProgram.startAt,
  durationMs: apiMatchProgram.durationMs,
  name: apiMatchProgram.name,
  isFree: true,
}

const reservation = {
  site: SITE,
  programId: omittedProgram.programId,
  source: 'manual',
  state: 'active',
  title: omittedProgram.name,
  serviceName: service.name,
  channelType: service.channelType,
  startAt: omittedProgram.startAt,
  durationMs: omittedProgram.durationMs,
  createdAt: iso(origin.getTime() - HOUR),
  updatedAt: iso(origin.getTime() - HOUR),
  skip: false,
  series: null,
}

let searchRequests = []
let failNextSearch = false
let holdNextSearch = false
let releaseHeldSearch
let notifyPendingSearchStarted

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: false })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': keepalive\n\n' })
  if (path === '/api/reservations') return json([reservation])
  if (path === '/api/capacity/overages') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === `/api/sites/${SITE}/services`) return json([service])
  if (path === `/api/sites/${SITE}/programs` && method === 'GET') {
    const start = Date.parse(url.searchParams.get('start') ?? '')
    const end = Date.parse(url.searchParams.get('end') ?? '')
    return json([apiMatchProgram, omittedProgram, skippedOmittedProgram].filter(
      (program) => Date.parse(program.endAt) > start && Date.parse(program.startAt) < end,
    ))
  }
  if (path === '/api/programs/search' && method === 'POST') {
    const request = await route.request().postDataJSON()
    searchRequests.push(request)
    if (holdNextSearch) {
      holdNextSearch = false
      notifyPendingSearchStarted?.()
      await new Promise((resolve) => { releaseHeldSearch = resolve })
    }
    if (failNextSearch) {
      failNextSearch = false
      return route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({ error: '条件検索の一時エラー' }),
      })
    }
    // 意図的に genres を見て結果を作らない。検索 API 自体が返した集合が正本。
    return json([searchMatch])
  }
  if (/\/programs\/\d+\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(path)) return json({ extended: {}, audios: [] })
  return json([])
}

const conditionUrl = (url) => {
  const value = new URL(url).searchParams.get('cond')
  return value === null ? undefined : JSON.parse(value)
}
const stableStringify = (value) => JSON.stringify(value, (_key, current) => (
  current && typeof current === 'object' && !Array.isArray(current)
    ? Object.fromEntries(Object.entries(current).sort(([a], [b]) => a.localeCompare(b)))
    : current
))

const cellBackground = (cell) => cell.evaluate((el) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  ctx.fillStyle = getComputedStyle(el).backgroundColor
  ctx.fillRect(0, 0, 1, 1)
  return Array.from(ctx.getImageData(0, 0, 1, 1).data)
})

const leftBorderColor = (cell) => cell.evaluate((el) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  ctx.fillStyle = getComputedStyle(el).borderLeftColor
  ctx.fillRect(0, 0, 1, 1)
  return Array.from(ctx.getImageData(0, 0, 1, 1).data)
})

const textContrast = (span) => span.evaluate((el) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const rgba = (value) => {
    ctx.clearRect(0, 0, 1, 1)
    ctx.fillStyle = value
    ctx.fillRect(0, 0, 1, 1)
    return Array.from(ctx.getImageData(0, 0, 1, 1).data)
  }
  const cell = el.closest('[data-testid="program-grid-cell"]')
  let background = [0, 0, 0, 0]
  for (let node = el; node && node !== cell.parentElement; node = node.parentElement) {
    const candidate = rgba(getComputedStyle(node).backgroundColor)
    if (candidate[3] > 0) {
      background = candidate
      break
    }
  }
  const foreground = rgba(getComputedStyle(el).color)
  let opacity = 1
  for (let node = el; node && node !== cell.parentElement; node = node.parentElement) {
    opacity *= Number(getComputedStyle(node).opacity)
  }
  const paint = (color, under) => {
    const alpha = (color[3] / 255) * opacity
    return color.slice(0, 3).map((value, index) => value * alpha + under[index] * (1 - alpha))
  }
  const backdrop = paint(background, [255, 255, 255])
  const ink = paint(foreground, backdrop)
  const luminance = ([r, g, b]) => {
    const linear = (component) => {
      const value = component / 255
      return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
    }
    return 0.2126 * linear(r) + 0.7152 * linear(g) + 0.0722 * linear(b)
  }
  const [high, low] = [luminance(ink), luminance(backdrop)].sort((a, b) => b - a)
  return { ratio: (high + 0.05) / (low + 0.05), opacity, ink, backdrop }
})

log(`URL: ${BASE}`)
log('\n=== 契約検証: API フィクスチャ ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ['apiMatchProgram', ListProgramsResponseItem, apiMatchProgram],
    ['omittedProgram', ListProgramsResponseItem, omittedProgram],
    ['skippedOmittedProgram', ListProgramsResponseItem, skippedOmittedProgram],
    ['reservation', ListReservationsResponseItem, reservation],
    ['searchMatch', SearchProgramsResponseItem, searchMatch],
  ],
  ng,
)
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)

log('\n=== ① 空の cond は検索せず、凡例から条件を追加する ===')
await page.goto(`${BASE}/programs?view=grid&cond=${encodeURIComponent('{}')}`, { waitUntil: 'domcontentloaded' })
await page.getByTestId('program-grid').waitFor({ timeout: 15000 })
await page.getByTestId(`program-grid-cell`).first().waitFor()
if (searchRequests.length !== 0) ng.push(`① 空の cond で検索 API が ${searchRequests.length} 回呼ばれた`)
const initialOmittedCell = page.locator(
  `[data-testid="program-grid-cell"][data-program-id="${omittedProgram.programId}"]`,
)
const ordinaryBackground = await cellBackground(initialOmittedCell)

let pendingSearchStarted
const pendingSearch = new Promise((resolve) => { pendingSearchStarted = resolve })
notifyPendingSearchStarted = pendingSearchStarted
holdNextSearch = true
await page.getByTestId('genre-filter-7').click()
await page.waitForURL((url) => conditionUrl(url.href)?.genres?.includes(7) === true, { timeout: 10000 })
await page.getByTestId('condition-lens-status').getByText('読み込み中').waitFor({ timeout: 10000 })
await pendingSearch
notifyPendingSearchStarted = undefined
const cells = page.getByTestId('program-grid-cell')
const firstCell = cells.filter({ hasText: apiMatchProgram.name }).first()
const omittedCell = cells.filter({ hasText: omittedProgram.name }).first()
const skippedCell = cells.filter({ hasText: skippedOmittedProgram.name }).first()
const pendingTint = await cellBackground(omittedCell)
const pendingText = await textContrast(omittedCell.getByTestId('program-grid-cell-name'))
if ((await omittedCell.getAttribute('data-condition-match')) !== null) {
  ng.push('① 検索 API の応答前に非一致セルの判定が確定した')
}
if (JSON.stringify(pendingTint) !== JSON.stringify(ordinaryBackground) || pendingText.opacity < 0.99) {
  ng.push(`① 取得中に通常の地色・文字を保てない（background=${JSON.stringify(pendingTint)}, text=${JSON.stringify(pendingText)}）`)
}
const pendingCellStates = await cells.evaluateAll((nodes) => nodes.map((el) => ({
  hasMatch: el.hasAttribute('data-condition-match'),
  opacity: Number(getComputedStyle(el).opacity),
})))
if (pendingCellStates.some((cell) => cell.hasMatch || cell.opacity < 0.99)) {
  ng.push(`① 取得中にセルを一致・非一致扱い、または薄くした（${JSON.stringify(pendingCellStates)}）`)
}
releaseHeldSearch?.()

await firstCell.waitFor({ timeout: 10000 })
await page.getByText('1 件一致').waitFor({ timeout: 10000 })
if ((await firstCell.getAttribute('data-condition-match')) !== 'true') {
  ng.push('① API が返した番組が一致扱いにならない')
}
if ((await omittedCell.getAttribute('data-condition-match')) !== 'false') {
  ng.push('① API が返さない同ジャンル番組が非一致扱いにならない')
}
if ((await skippedCell.getAttribute('data-condition-match')) !== 'false') {
  ng.push('① API が返さないスキップ番組が非一致扱いにならない')
}
if (
  (await skippedCell.getAttribute('data-skip-intent')) !== 'true' ||
  !(await skippedCell.getByTestId('program-grid-cell-skip-intent-badge').isVisible())
) {
  ng.push('① 非一致セルから見えるスキップ印が消えた')
}
if (!(await firstCell.getAttribute('aria-label')).includes('条件に一致')) {
  ng.push('① 一致情報がセルの aria-label に含まれない')
}
const matchedBackground = await cellBackground(firstCell)
const omittedBackground = await cellBackground(omittedCell)
const matchedEdge = await leftBorderColor(firstCell)
const omittedEdge = await leftBorderColor(omittedCell)
const colorDistance = (a, b) => Math.sqrt(a.slice(0, 3).reduce((sum, channel, index) => sum + (channel - b[index]) ** 2, 0))
const fillDistance = colorDistance(matchedBackground, omittedBackground)
const edgeDistance = colorDistance(matchedEdge, omittedEdge)
log(`  一致 / 非一致の地色 RGB 距離=${fillDistance.toFixed(1)}、左罫 RGB 距離=${edgeDistance.toFixed(1)}`)
if (fillDistance < 2 && edgeDistance < 30) {
  ng.push(`① 一致セルと非一致セルの地・左罫に画素差が無い（pending=${pendingTint}）`)
}

log('\n=== ② 非一致セルの文字と予約印の実画素コントラスト ===')
const nonMatchName = omittedCell.getByTestId('program-grid-cell-name')
const nonMatchTime = omittedCell.getByTestId('program-grid-cell-time')
for (const [label, target] of [['番組名', nonMatchName], ['時刻', nonMatchTime]]) {
  const measured = await textContrast(target)
  log(`  ${label}: contrast=${measured.ratio.toFixed(2)} opacity=${measured.opacity.toFixed(2)}`)
  if (measured.ratio < 4.5 || measured.opacity < 0.99) {
    ng.push(`② 非一致セルの${label}が読めない（${JSON.stringify(measured)}）`)
  }
}
if ((await omittedCell.getAttribute('data-reserved')) !== 'true') {
  ng.push('② 非一致セルから予約状態が消えた')
}
const reservationBadge = omittedCell.getByTestId('program-grid-cell-reserved-label')
if (!(await reservationBadge.isVisible())) ng.push('② 非一致セルの見える予約印が消えた')
const reservationContrast = await textContrast(reservationBadge)
log(`  予約印: contrast=${reservationContrast.ratio.toFixed(2)} opacity=${reservationContrast.opacity.toFixed(2)}`)
if (reservationContrast.ratio < 4.5 || reservationContrast.opacity < 0.99) {
  ng.push(`② 予約印の文字が読めない（${JSON.stringify(reservationContrast)}）`)
}

// dark token でも非一致セルの文字を同じ基準で測る。
await page.evaluate(() => document.documentElement.classList.add('dark'))
await page.waitForTimeout(100)
const darkNameContrast = await textContrast(nonMatchName)
log(`  dark 番組名: contrast=${darkNameContrast.ratio.toFixed(2)} opacity=${darkNameContrast.opacity.toFixed(2)}`)
if (darkNameContrast.ratio < 4.5 || darkNameContrast.opacity < 0.99) {
  ng.push(`② dark の非一致セル文字が読めない（${JSON.stringify(darkNameContrast)}）`)
}
await page.evaluate(() => document.documentElement.classList.remove('dark'))

await skippedCell.click()
if (!(await skippedCell.getAttribute('class')).includes('ring-2')) {
  ng.push('② 条件レンズ中に選択の輪が消えた')
}
await page.keyboard.press('Escape')

log('\n=== ③ ローカル暦日件数と検索画面への往復 ===')
const dayCounts = await page.getByTestId('day-match-count').allTextContents()
if (dayCounts[0] !== '1件' || dayCounts[1] !== '0件') {
  ng.push(`③ 日付境界をまたぐ番組の件数が開始日のローカル暦日に付かない（${JSON.stringify(dayCounts.slice(0, 2))}）`)
}
await page.getByRole('link', { name: '検索で開く' }).click()
await page.waitForURL((url) => url.pathname === '/search' && conditionUrl(url.href)?.genres?.includes(7) === true)
await page.getByTestId('search-results').waitFor({ timeout: 15000 })
await page.getByRole('link', { name: '番組表で見る' }).click()
await page.waitForURL((url) => url.pathname === '/programs' && url.searchParams.get('view') === 'grid')
if (conditionUrl(page.url())?.genres?.[0] !== 7) ng.push('③ 番組表への遷移で cond が変わった')
await page.getByRole('link', { name: '検索で開く' }).click()
await page.waitForURL((url) => url.pathname === '/search' && conditionUrl(url.href)?.genres?.[0] === 7)
if (conditionUrl(page.url())?.genres?.[0] !== 7) ng.push('③ 検索画面への復帰で cond が変わった')

const sharedCondition = {
  genres: [7],
  textMatches: [{ target: 'name', mode: 'keyword', value: 'アニメ' }],
}
await page.goto(`${BASE}/search?cond=${encodeURIComponent(JSON.stringify(sharedCondition))}`, {
  waitUntil: 'domcontentloaded',
})
await page.getByTestId('search-results').waitFor({ timeout: 15000 })
await page.getByRole('link', { name: '番組表で見る' }).click()
await page.waitForURL((url) => url.pathname === '/programs' && url.searchParams.get('view') === 'grid')
if (stableStringify(conditionUrl(page.url())) !== stableStringify(sharedCondition)) {
  ng.push(`③ テキスト条件を含む cond が番組表へ同じ値で渡らない（${page.url()}）`)
}
if (!(await page.getByTestId('condition-lens').innerText()).includes('番組名に「アニメ」を含む')) {
  ng.push('③ 番組表の条件チップにテキスト条件の種別要約が無い')
}
await page.getByRole('link', { name: '検索で開く' }).click()
await page.waitForURL((url) => url.pathname === '/search' && conditionUrl(url.href)?.genres?.[0] === 7)
if (stableStringify(conditionUrl(page.url())) !== stableStringify(sharedCondition)) {
  ng.push(`③ テキスト条件を含む cond が検索画面へ同じ値で戻らない（${page.url()}）`)
}

log('\n=== ④ 検索失敗と再試行は通常表示を保つ ===')
await page.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })
await page.getByTestId('program-grid').waitFor({ timeout: 15000 })
failNextSearch = true
const errorBackground = await cellBackground(
  page.locator(`[data-testid="program-grid-cell"][data-program-id="${omittedProgram.programId}"]`),
)
await page.getByTestId('genre-filter-1').click()
await page.getByRole('alert').getByRole('button', { name: '再試行' }).waitFor({ timeout: 15000 })
const failedOmittedCell = page.getByTestId('program-grid-cell').filter({ hasText: omittedProgram.name }).first()
if ((await failedOmittedCell.getAttribute('data-condition-match')) !== null) {
  ng.push('④ 失敗時に全セルへ一致・非一致状態を付けた')
}
const failedText = await textContrast(failedOmittedCell.getByTestId('program-grid-cell-name'))
const failedBackground = await cellBackground(failedOmittedCell)
if (JSON.stringify(failedBackground) !== JSON.stringify(errorBackground) || failedText.opacity < 0.99) {
  ng.push(`④ 失敗時に通常表示を保てない（background=${JSON.stringify(failedBackground)}, text=${JSON.stringify(failedText)}）`)
}
const failedCellStates = await page.getByTestId('program-grid-cell').evaluateAll((nodes) => nodes.map((el) => ({
  hasMatch: el.hasAttribute('data-condition-match'),
  opacity: Number(getComputedStyle(el).opacity),
})))
if (failedCellStates.some((cell) => cell.hasMatch || cell.opacity < 0.99)) {
  ng.push(`④ 失敗時にセルを一致・非一致扱い、または薄くした（${JSON.stringify(failedCellStates)}）`)
}
await page.getByRole('alert').getByRole('button', { name: '再試行' }).click()
await page.getByText('1 件一致').waitFor({ timeout: 15000 })
if ((await failedOmittedCell.getAttribute('data-condition-match')) !== 'false') {
  ng.push('④ 再試行成功後に API 応答の一致集合が反映されない')
}

await context.close()
await finish(ng, browser)
