// Display switches are classified by what the user is choosing: presentation,
// filters, or distinct screen content. This gate checks the two controls changed
// for issue #1229 in a rendered browser and captures both themes and widths.
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:issue-1229-switches
import { mkdirSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
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

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const OUT_DIR = process.env.E2E_SHOT_DIR ?? path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  'screenshots',
  'issue-1229',
)
const FIXED_NOW = new Date('2026-08-14T12:00:00+09:00')
const HOUR = 3_600_000
const PROGRAM_VIEW_KEY = 'rokuban:programs:view'
const HOME_MODE_KEY = 'rokuban:home:mode'
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
const program = {
  programId: 1229,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 1229,
  startAt: new Date(FIXED_NOW.getTime() + HOUR).toISOString(),
  endAt: new Date(FIXED_NOW.getTime() + 2 * HOUR).toISOString(),
  durationMs: HOUR,
  name: '表示切替の確認番組',
  description: '',
  genres: [0],
  isFree: true,
}

async function apiHandler({ path: requestPath, url, json, route }) {
  if (requestPath === '/api/sites') return json(['default'])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/sites/default/services') return json([service])
  if (requestPath === '/api/sites/default/programs') {
    const start = Date.parse(url.searchParams.get('start') ?? '')
    const end = Date.parse(url.searchParams.get('end') ?? '')
    return json(Number.isFinite(start) && Number.isFinite(end) && start < Date.parse(program.endAt) && end > Date.parse(program.startAt) ? [program] : [])
  }
  if (/^\/api\/sites\/default\/programs\/\d+\/overlaps$/.test(requestPath)) {
    return json({ count: 0, reservations: [] })
  }
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/storage') return json([])
  return json([])
}

function check(condition, message) {
  if (!condition) ng.push(message)
}

// Segmented controls must look identical: same frame, same selected fill.
async function measureSegment(group) {
  return group.evaluate((element) => {
    const frame = getComputedStyle(element)
    const on = getComputedStyle(element.querySelector('[aria-pressed="true"],[aria-current="page"]'))
    const off = getComputedStyle(element.querySelector('[aria-pressed="false"],a:not([aria-current])'))
    return {
      border: `${frame.borderTopWidth} ${frame.borderTopStyle} ${frame.borderTopColor}`,
      groupBg: frame.backgroundColor,
      selectedBg: on.backgroundColor,
      selectedShadow: on.boxShadow,
      unselectedBg: off.backgroundColor,
    }
  })
}

function shotName(page, mode, theme, width) {
  return path.join(OUT_DIR, `${page}-${mode}-${theme}-${width}.png`)
}

await validateFixturesOrExit([
  ['service', ListServicesResponseItem, service],
  ['program', ListProgramsResponseItem, program],
], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)
mkdirSync(OUT_DIR, { recursive: true })

const browser = await launchBrowser()
for (const theme of ['light', 'dark']) {
  for (const width of [1280, 390]) {
    const context = await browser.newContext({
      viewport: { width, height: 900 },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      isMobile: width < 768,
      hasTouch: width < 768,
    })
    const page = await context.newPage()
    const segments = []
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler)

    log(`\n=== Programs: ${theme}, ${width}px ===`)
    await page.goto(`${URL_BASE}/programs`, { waitUntil: 'domcontentloaded' })
    await page.getByText(program.name).waitFor({ timeout: 15_000 })
    await page.waitForFunction((isDark) => document.documentElement.classList.contains('dark') === isDark, theme === 'dark')
    const programsToggle = page.getByRole('group', { name: '表示形式' })
    if (width < 1024) {
      check(await programsToggle.count() === 0, `Programs ${theme}/${width}: list/grid toggle should be hidden below lg`)
      await page.screenshot({ path: shotName('programs', 'list', theme, width), animations: 'disabled' })
    } else {
      await programsToggle.waitFor({ timeout: 10_000 })
      segments.push(['programs', await measureSegment(programsToggle)])
      const listButton = programsToggle.getByRole('button', { name: 'リスト' })
      const gridButton = programsToggle.getByRole('button', { name: '番組表' })
      check(await listButton.getAttribute('aria-pressed') === 'true', `Programs ${theme}/${width}: list should be selected initially`)
      check(await gridButton.getAttribute('aria-pressed') === 'false', `Programs ${theme}/${width}: grid should be unselected initially`)
      await page.screenshot({ path: shotName('programs', 'list', theme, width), animations: 'disabled' })

      await gridButton.click()
      await page.getByTestId('program-grid').waitFor({ timeout: 15_000 })
      check(new URL(page.url()).searchParams.get('view') === 'grid', `Programs ${theme}/${width}: grid selection should update URL`)
      check(await page.evaluate((key) => localStorage.getItem(key), PROGRAM_VIEW_KEY) === 'grid', `Programs ${theme}/${width}: grid selection should save to localStorage`)
      check(await gridButton.getAttribute('aria-pressed') === 'true', `Programs ${theme}/${width}: grid should be selected after activation`)
      await page.screenshot({ path: shotName('programs', 'grid', theme, width), animations: 'disabled' })

      await listButton.click()
      await page.getByTestId('program-grid').waitFor({ state: 'detached', timeout: 15_000 })
      check(new URL(page.url()).searchParams.get('view') === 'list', `Programs ${theme}/${width}: list selection should update URL`)
      check(await page.evaluate((key) => localStorage.getItem(key), PROGRAM_VIEW_KEY) === 'list', `Programs ${theme}/${width}: list selection should save to localStorage`)
    }

    log(`\n=== Home: ${theme}, ${width}px ===`)
    await page.goto(`${URL_BASE}/`, { waitUntil: 'domcontentloaded' })
    const homeToggle = page.getByTestId('home-mode-toggle')
    await homeToggle.waitFor({ timeout: 15_000 })
    await page.waitForFunction((isDark) => document.documentElement.classList.contains('dark') === isDark, theme === 'dark')
    const watchLink = homeToggle.getByRole('link', { name: '見る' })
    const manageLink = homeToggle.getByRole('link', { name: /管理/ })

    const checkActiveTab = async (link, label) => {
      const result = await link.evaluate((element) => {
        const style = getComputedStyle(element)
        return {
          borderBottomWidth: style.borderBottomWidth,
          borderBottomStyle: style.borderBottomStyle,
          className: element.className,
          ariaCurrent: element.getAttribute('aria-current'),
        }
      })
      check(result.borderBottomWidth === '2px' && result.borderBottomStyle === 'solid', `Home ${theme}/${width}: ${label} selected tab needs a 2px underline (${JSON.stringify(result)})`)
      check(result.className.split(/\s+/).includes('border-foreground'), `Home ${theme}/${width}: ${label} underline should use the neutral foreground token (${JSON.stringify(result)})`)
    }

    check(await watchLink.getAttribute('aria-current') === 'page', `Home ${theme}/${width}: Watch should be selected without a mode URL`)
    await checkActiveTab(watchLink, 'Watch')
    await page.screenshot({ path: shotName('home', 'watch', theme, width), animations: 'disabled' })

    await manageLink.click()
    await page.waitForURL((current) => current.searchParams.get('mode') === 'ops')
    check(await manageLink.getAttribute('aria-current') === 'page', `Home ${theme}/${width}: Manage should be selected after activation`)
    check(await page.evaluate((key) => localStorage.getItem(key), HOME_MODE_KEY) === 'ops', `Home ${theme}/${width}: Manage selection should save to localStorage`)
    await checkActiveTab(manageLink, 'Manage')
    await page.screenshot({ path: shotName('home', 'manage', theme, width), animations: 'disabled' })

    await page.goto(`${URL_BASE}/`, { waitUntil: 'domcontentloaded' })
    check(await page.getByTestId('home-mode-toggle').getByRole('link', { name: /管理/ }).getAttribute('aria-current') === 'page', `Home ${theme}/${width}: saved Manage preference should restore without a mode URL`)
    await page.goto(`${URL_BASE}/?mode=watch`, { waitUntil: 'domcontentloaded' })
    check(await page.getByTestId('home-mode-toggle').getByRole('link', { name: '見る' }).getAttribute('aria-current') === 'page', `Home ${theme}/${width}: URL should take precedence over saved Manage preference`)

    log(`\n=== Recordings / Reservations: ${theme}, ${width}px ===`)
    await page.goto(`${URL_BASE}/recordings`, { waitUntil: 'domcontentloaded' })
    const recTabs = page.getByTestId('recordings-view-tabs')
    await recTabs.waitFor({ timeout: 15_000 })
    await page.waitForFunction((isDark) => document.documentElement.classList.contains('dark') === isDark, theme === 'dark')
    const libTab = recTabs.getByRole('button', { name: 'ライブラリ' })
    const trashTab = recTabs.getByRole('button', { name: 'ごみ箱' })
    const tabStyle = (button) => button.evaluate((element) => {
      const style = getComputedStyle(element)
      return { w: style.borderBottomWidth, st: style.borderBottomStyle, bg: style.backgroundColor, cls: element.className }
    })
    const checkRecTab = async (button, label) => {
      const r = await tabStyle(button)
      check(r.w === '2px' && r.st === 'solid', `Recordings ${theme}/${width}: ${label} selected tab needs a 2px underline (${JSON.stringify(r)})`)
      check(r.cls.split(/\s+/).includes('border-foreground'), `Recordings ${theme}/${width}: ${label} underline should use the foreground token`)
      check(r.bg === 'rgba(0, 0, 0, 0)', `Recordings ${theme}/${width}: ${label} selected tab must not be filled (${r.bg})`)
    }
    await checkRecTab(libTab, 'Library')
    const idle = await tabStyle(trashTab)
    check(!idle.cls.split(/\s+/).includes('border-foreground'), `Recordings ${theme}/${width}: unselected tab must have no foreground underline`)
    await page.screenshot({ path: shotName('recordings', 'library', theme, width), animations: 'disabled' })
    await trashTab.click()
    await page.waitForURL((current) => current.searchParams.get('tab') === 'trash')
    await checkRecTab(trashTab, 'Trash')
    await page.goto(`${URL_BASE}/recordings`, { waitUntil: 'domcontentloaded' })
    const recSeries = page.getByRole('group', { name: '録画とシリーズの表示切替' })
    await recSeries.waitFor({ timeout: 15_000 })
    segments.push(['recording-series', await measureSegment(recSeries)])

    await page.goto(`${URL_BASE}/reservations`, { waitUntil: 'domcontentloaded' })
    const resGroup = page.getByRole('group', { name: '予約のまとめ方' })
    await resGroup.waitFor({ timeout: 15_000 })
    segments.push(['reservation-group', await measureSegment(resGroup)])
    await page.screenshot({ path: shotName('reservations', 'list', theme, width), animations: 'disabled' })

    const [ref, ...rest] = segments
    for (const [name, m] of rest) {
      check(JSON.stringify(m) === JSON.stringify(ref[1]), `Segmented ${theme}/${width}: ${name} differs from ${ref[0]} (${JSON.stringify(m)} vs ${JSON.stringify(ref[1])})`)
    }
    check(ref[1].border.startsWith('1px solid') && ref[1].selectedBg !== ref[1].unselectedBg, `Segmented ${theme}/${width}: frame and selected fill must exist (${JSON.stringify(ref[1])})`)

    await context.close()
  }
}

await finish(ng, browser)
