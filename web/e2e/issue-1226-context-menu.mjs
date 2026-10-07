// Issue #1226: デスクトップ一覧の行コンテキストメニューを実ブラウザで測る。
// menu の開閉・Escape 後のフォーカス復帰と、Shift / 選択中 / coarse pointer で
// browser の contextmenu を横取りしないことは jsdom のイベントだけでは保証できない。
import {
  ListRecordingsResponseItem,
  ListReservationsResponseItem,
  ListRulesResponseItem,
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

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []
const SITE = 'default'
const STAMP = '2026-10-07T03:00:00.000Z'

const recording = {
  id: 86,
  site: SITE,
  source: 'manual',
  serviceName: 'ＮＨＫ総合１・東京',
  channelType: 'GR',
  channel: '27',
  networkId: 32736,
  serviceId: 32736,
  eventId: 86,
  title: '右クリック用の録画',
  startAt: STAMP,
  durationMs: 30 * 60_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: STAMP,
}

const reservation = {
  site: SITE,
  programId: 9001,
  source: 'manual',
  state: 'active',
  title: '右クリック用の予約',
  serviceName: 'ＮＨＫ総合１・東京',
  channelType: 'GR',
  startAt: '2026-10-07T04:00:00.000Z',
  durationMs: 30 * 60_000,
  createdAt: STAMP,
  updatedAt: STAMP,
  series: null,
  skip: false,
}

const firstEpisode = {
  ...reservation,
  programId: 9002,
  title: '右クリックシリーズ 第一話',
  startAt: '2026-10-07T05:00:00.000Z',
  series: '右クリックシリーズ',
}

const secondEpisode = {
  ...reservation,
  programId: 9003,
  title: '右クリックシリーズ 第二話',
  startAt: '2026-10-07T06:00:00.000Z',
  series: '右クリックシリーズ',
}

const rule = {
  id: 18,
  name: '右クリック用のルール',
  enabled: true,
  priority: 10,
  keepOriginal: 'always',
  createdAt: STAMP,
  updatedAt: STAMP,
}

async function apiHandler({ path, json, route }) {
  const method = route.request().method()
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: false })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/storage') return json([])
  if (path === '/api/recording-shelves') return json([])
  if (path === '/api/rules') return json([rule])
  if (path === '/api/reservations') return json([reservation, firstEpisode, secondEpisode])
  if (path === '/api/capacity/overages') return json([])
  if (path === '/api/recordings' && method === 'GET') return json([recording])
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(path)) {
    return route.fulfill({ status: 404 })
  }
  return json([])
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['recording', ListRecordingsResponseItem, recording],
    ['reservation', ListReservationsResponseItem, reservation],
    ['rule', ListRulesResponseItem, rule],
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const FIXED_NOW = new Date('2026-10-07T12:00:00+09:00')

async function openPage(path, { coarse = false, reservationGrouping = 'time' } = {}) {
  const context = await browser.newContext({
    viewport: coarse ? { width: 390, height: 844 } : { width: 1280, height: 800 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    isMobile: coarse,
    hasTouch: coarse,
  })
  const page = await context.newPage()
  await page.clock.install({ time: FIXED_NOW })
  await installApiStubs(page, apiHandler)
  if (reservationGrouping !== null) {
    await page.addInitScript((grouping) => localStorage.setItem('rokuban:reservations:group', grouping), reservationGrouping)
  }
  await page.goto(URL_BASE + path, { waitUntil: 'domcontentloaded' })
  return { context, page }
}

async function installContextMenuProbe(page) {
  await page.evaluate(() => {
    window.__contextMenuProbe = null
    window.addEventListener(
      'contextmenu',
      (event) => {
        const shiftKey = event.shiftKey
        setTimeout(() => {
          window.__contextMenuProbe = {
            shiftKey,
            defaultPrevented: event.defaultPrevented,
          }
        }, 0)
      },
      true,
    )
  })
}

async function rightClick(page, target, modifiers = []) {
  await target.scrollIntoViewIfNeeded()
  const box = await target.boundingBox()
  if (!box) throw new Error('contextmenu target has no bounding box')
  await page.evaluate(() => {
    window.__contextMenuProbe = null
  })
  const shift = modifiers.includes('Shift')
  if (shift) await page.keyboard.down('Shift')
  await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2, { button: 'right' })
  if (shift) await page.keyboard.up('Shift')
  await page.waitForFunction(() => window.__contextMenuProbe !== null, undefined, { timeout: 3000 }).catch(() => {})
  return page.evaluate(() => window.__contextMenuProbe)
}

async function checkMenuAndEscape(page, row, label, expectedItems, expectedHref) {
  const probe = await rightClick(page, row)
  const menu = page.getByRole('menu')
  const opened = await menu
    .waitFor({ state: 'visible', timeout: 1800 })
    .then(() => true)
    .catch(() => false)
  log(`  ${label}: defaultPrevented=${probe?.defaultPrevented ?? 'event not observed'}, menu=${opened}`)
  if (!opened) {
    ng.push(`${label}: 行の右クリックでコンテキストメニューが開かない`)
    return
  }
  if (!probe?.defaultPrevented) ng.push(`${label}: 独自メニューを開いたのに browser の contextmenu を抑止しない`)
  const items = await menu.getByRole('menuitem').allTextContents()
  const normalized = items.map((item) => item.trim()).filter(Boolean)
  if (expectedItems.some((item, index) => normalized[index] !== item)) {
    ng.push(`${label}: menu item の先頭が想定と一致しない (${normalized.join(' / ')})`)
  }
  if (expectedHref) {
    const openHref = await menu
      .getByRole('menuitem', { name: '開く', exact: true })
      .getAttribute('href')
    if (openHref !== expectedHref) ng.push(`${label}: 「開く」の遷移先が一致しない (${openHref})`)
  }
  await page.keyboard.press('Escape')
  await menu.waitFor({ state: 'hidden', timeout: 3000 }).catch(() => {
    ng.push(`${label}: Escape でコンテキストメニューが閉じない`)
  })
  const rowHandle = await row.elementHandle()
  const returned = rowHandle
    ? await page
        .waitForFunction((element) => document.activeElement === element, rowHandle, { timeout: 1000 })
        .then(() => true)
        .catch(() => false)
    : false
  if (!returned) ng.push(`${label}: Escape 後にフォーカスが行へ戻らない`)
}

async function checkNativeContextMenu(page, row, label, modifiers = []) {
  const probe = await rightClick(page, row, modifiers)
  const menuOpen = (await page.getByRole('menu').count()) > 0
  if (probe === null) {
    ng.push(`${label}: browser contextmenu event が観測できない`)
  } else if (probe.defaultPrevented) {
    ng.push(`${label}: browser の contextmenu が preventDefault された`)
  }
  if (menuOpen) ng.push(`${label}: 独自メニューが開いた`)
  log(`  ${label}: defaultPrevented=${probe?.defaultPrevented ?? 'event not observed'}, menu=${menuOpen}`)
}

log('\n=== ① 1280px: 録画行の右クリックメニューと Escape ===')
{
  const { context, page } = await openPage('/recordings')
  await installContextMenuProbe(page)
  await page.getByText(recording.title, { exact: true }).waitFor({ timeout: 15_000 })
  const row = page.getByRole('link', { name: recording.title }).locator('xpath=..')
  await checkMenuAndEscape(page, row, '録画行', [
    '開く',
    '新しいタブで開く',
    'リンクをコピー',
  ])
  await page.getByRole('button', { name: '選択' }).click()
  const checkbox = page.getByRole('checkbox', { name: `${recording.title}を選択` })
  await checkbox.waitFor({ state: 'visible' })
  const selectedRow = checkbox.locator('xpath=..')
  await checkNativeContextMenu(page, selectedRow, '選択モード中の右クリック')
  await context.close()

  const { context: shiftContext, page: shiftPage } = await openPage('/recordings')
  await installContextMenuProbe(shiftPage)
  await shiftPage.getByText(recording.title, { exact: true }).waitFor({ timeout: 15_000 })
  const shiftRow = shiftPage.getByRole('link', { name: recording.title }).locator('xpath=..')
  const shifted = await rightClick(shiftPage, shiftRow, ['Shift'])
  log(
    `  Shift+右クリック: shiftKey=${shifted?.shiftKey ?? 'event not observed'}, defaultPrevented=${shifted?.defaultPrevented ?? 'event not observed'}`,
  )
  if (shifted === null) ng.push('Shift+右クリック: browser contextmenu event が観測できない')
  if (shifted?.defaultPrevented) ng.push('Shift+右クリック: browser contextmenu が抑止された')
  if ((await shiftPage.getByRole('menu').count()) > 0) ng.push('Shift+右クリック: 独自メニューが開いた')
  await shiftContext.close()
}

log('\n=== ② 1280px: 予約行の右クリックメニューと Escape ===')
{
  const { context, page } = await openPage('/reservations')
  await installContextMenuProbe(page)
  await page.getByText(reservation.title, { exact: true }).waitFor({ timeout: 15_000 })
  const row = page.getByText(reservation.title, { exact: true }).locator('xpath=../..')
  await checkMenuAndEscape(page, row, '予約行', [
    '開く',
    '新しいタブで開く',
    'リンクをコピー',
  ])
  const shifted = await rightClick(page, row, ['Shift'])
  if (shifted === null) ng.push('予約行 Shift+右クリック: browser contextmenu event が観測できない')
  if (shifted?.defaultPrevented) ng.push('予約行 Shift+右クリック: browser contextmenu が抑止された')
  if ((await page.getByRole('menu').count()) > 0) ng.push('予約行 Shift+右クリック: 独自メニューが開いた')
  await context.close()
}

log('\n=== ③ 1280px: 既定シリーズ表示の予約行・各話メニューと Escape ===')
{
  // 新しい browser context の localStorage は空なので、time の override を入れず
  // ReservationsPage の既定であるシリーズ表示を測る。
  const { context, page } = await openPage('/reservations', { reservationGrouping: null })
  await installContextMenuProbe(page)
  const individualHeader = page
    .getByTestId('reservation-series-header')
    .filter({ hasText: reservation.title })
  await individualHeader.waitFor({ state: 'visible', timeout: 15_000 })
  await checkMenuAndEscape(
    page,
    individualHeader,
    'シリーズ表示の単独予約行',
    ['開く', '新しいタブで開く', 'リンクをコピー'],
    '/reservations/default/9001',
  )

  const shifted = await rightClick(page, individualHeader, ['Shift'])
  if (shifted === null) ng.push('シリーズ表示 Shift+右クリック: browser contextmenu event が観測できない')
  if (shifted?.defaultPrevented) ng.push('シリーズ表示 Shift+右クリック: browser contextmenu が抑止された')
  if ((await page.getByRole('menu').count()) > 0) ng.push('シリーズ表示 Shift+右クリック: 独自メニューが開いた')

  const seriesHeader = page
    .getByTestId('reservation-series-header')
    .filter({ hasText: '右クリックシリーズ' })
  await checkMenuAndEscape(
    page,
    seriesHeader,
    'シリーズ見出し行',
    ['開く', '新しいタブで開く', 'リンクをコピー'],
    '/reservations/default/9002',
  )
  await page.getByRole('button', { name: '右クリックシリーズの予約を開く' }).click()
  const episodeRow = page.getByTestId('reservation-episode-row').filter({ hasText: '第二話' })
  await episodeRow.waitFor({ state: 'visible', timeout: 15_000 })
  await checkMenuAndEscape(
    page,
    episodeRow,
    '展開した各話行',
    ['開く', '新しいタブで開く', 'リンクをコピー'],
    '/reservations/default/9003',
  )
  await context.close()
}

log('\n=== ④ 1280px: ルール行の右クリックメニューと Escape ===')
{
  const { context, page } = await openPage('/rules')
  await installContextMenuProbe(page)
  const row = page.locator('li').filter({ has: page.getByRole('switch') }).locator('div.rounded-lg.border')
  await row.waitFor({ state: 'visible', timeout: 15_000 })
  await checkMenuAndEscape(page, row, 'ルール行', ['無効にする', '削除'])
  await context.close()
}

log('\n=== ⑤ 390px: coarse pointer では browser contextmenu を保つ ===')
{
  const { context, page } = await openPage('/recordings', { coarse: true })
  await installContextMenuProbe(page)
  await page.getByText(recording.title, { exact: true }).waitFor({ timeout: 15_000 })
  const coarse = await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches)
  if (!coarse) ng.push('④ coarse pointer context を再現できない')
  const row = page.getByRole('link', { name: recording.title }).locator('xpath=..')
  await checkNativeContextMenu(page, row, 'coarse pointer の右クリック')
  await context.close()

  const { context: reservationContext, page: reservationPage } = await openPage('/reservations', {
    coarse: true,
    reservationGrouping: null,
  })
  await installContextMenuProbe(reservationPage)
  const reservationHeader = reservationPage
    .getByTestId('reservation-series-header')
    .filter({ hasText: reservation.title })
  await reservationHeader.waitFor({ state: 'visible', timeout: 15_000 })
  await checkNativeContextMenu(reservationPage, reservationHeader, '粗いポインタの予約行')
  await reservationContext.close()
}

await finish(ng, browser)
