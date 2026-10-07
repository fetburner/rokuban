// H-12 / H-13 比較用スクリーンショット（PoC。判定ではなく見た目の比較用）。
//   cd web && corepack pnpm build && corepack pnpm preview --port 4399 --strictPort &
//   OUT=./h1213-mocks E2E_URL=http://localhost:4399 node e2e/h1213-shots.mjs
// 変種は window.__mock（{ h12: 'body', h13: 'toast' | 'hybrid' }）で切り替える。
import { mkdirSync } from 'node:fs'
import { installApiStubs, launchBrowser, log, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4399'
const OUT = process.env.OUT ?? './h1213-mocks'
mkdirSync(OUT, { recursive: true })

const iso = (ms) => new Date(ms).toISOString()
const now = Date.now()
const MIN = 60_000
const HOUR = 60 * MIN
const base = { createdAt: iso(now - 100 * HOUR), updatedAt: iso(now - 100 * HOUR) }
const rule = (over) => ({
  id: 1, name: '朝ドラ', enabled: true, priority: 10, keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  textMatches: [{ target: 'name', mode: 'keyword', value: '連続テレビ小説' }], ...base, ...over,
})
const resv = (id, startAt, durationMs) => ({
  id, site: 'default', programId: 9000 + id, source: 'rule', ruleId: 1, state: 'active',
  title: '連続テレビ小説', serviceName: 'NHK総合', channelType: 'GR', startAt: iso(startAt),
  durationMs, createdAt: iso(now - HOUR), updatedAt: iso(now - HOUR), series: null, skip: false,
})
const future = [resv(1, now + 2 * HOUR, 15 * MIN), resv(2, now + 26 * HOUR, 15 * MIN), resv(3, now + 50 * HOUR, 15 * MIN)]
const withRecording = [resv(1, now - 5 * MIN, 15 * MIN), resv(2, now + 24 * HOUR, 15 * MIN), resv(3, now + 48 * HOUR, 15 * MIN)]

async function open(browser, mock, reservations) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2, colorScheme: 'light' })
  const page = await context.newPage()
  await page.addInitScript((m) => { window.__mock = m }, mock)
  let enabled = true
  await installApiStubs(page, async ({ path, json, route }) => {
    if (path === '/api/sites') return json(['default'])
    if (path === '/api/capabilities') return json({ live: true })
    if (path === '/api/encode-profiles') return json([{ name: 'hevc-1080p', container: 'mp4' }])
    if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
    if (path === '/api/events') return sseKeepAlive(route)
    if (path === '/api/rules') return json([rule({ enabled })])
    if (/^\/api\/rules\/1$/.test(path) && route.request().method() === 'PATCH') {
      enabled = route.request().postDataJSON().enabled
      return json(rule({ enabled }))
    }
    if (path === '/api/reservations') return json(reservations)
    return json([])
  })
  page.on('response', (r) => { if (r.request().method() !== 'GET') log('  ', r.request().method(), r.url(), r.status()) })
  await page.goto(URL_BASE + '/rules')
  await page.waitForSelector('text=朝ドラ')
  return { context, page }
}

/** Tab を押して aria-label が re に合う要素へフォーカスを運ぶ。 */
async function tabTo(page, re) {
  for (let i = 0; i < 60; i++) {
    await page.keyboard.press('Tab')
    const label = await page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? '')
    if (re.test(label)) return
  }
  throw new Error('tab target not found: ' + re)
}

async function openDelete(page) {
  await tabTo(page, /その他の操作/)
  await page.keyboard.press('Enter')
  await page.waitForSelector('[role=menu]')
  await page.waitForTimeout(300)
  const focused = await page.evaluate(() => document.activeElement?.textContent ?? '')
  if (!focused.includes('削除')) await page.keyboard.press('ArrowDown')
  await page.keyboard.press('Enter')
  await page.waitForSelector('[role=alertdialog]')
  await page.waitForTimeout(400)
}

const browser = await launchBrowser()
const shot = (page, name) => page.screenshot({ path: `${OUT}/${name}.png` })
const active = (page) => page.evaluate(() => {
  const a = document.activeElement
  return `${a?.tagName} ${a?.getAttribute('role') ?? ''} "${(a?.textContent ?? '').trim().slice(0, 20)}" data-slot=${a?.getAttribute('data-slot') ?? ''}`
})

// H-12
{
  const { context, page } = await open(browser, {}, future)
  await openDelete(page)
  log('h12-cancel active:', await active(page))
  await shot(page, 'h12-cancel')
  await context.close()
}
{
  const { context, page } = await open(browser, { h12: 'body' }, future)
  await openDelete(page)
  log('h12-body active:', await active(page))
  await shot(page, 'h12-body')
  await page.keyboard.press('Enter')
  await page.waitForTimeout(500)
  log('h12 after Return: dialog still open =', (await page.locator('[role=alertdialog]').count()) === 1, '| active:', await active(page))
  await page.keyboard.press('Tab')
  await page.waitForTimeout(200)
  log('h12-body-tab active:', await active(page))
  await shot(page, 'h12-body-tab')
  await context.close()
}

// H-13
async function toggleOff(page) {
  await tabTo(page, /を有効にする/)
  await page.keyboard.press('Space')
  await page.waitForTimeout(800)
}
for (const [name, mock, resvs] of [
  ['h13-dialog', {}, future],
  ['h13-toast', { h13: 'toast' }, future],
  ['h13-hybrid-recording', { h13: 'hybrid' }, withRecording],
  ['h13-hybrid-plain', { h13: 'hybrid' }, future],
]) {
  const { context, page } = await open(browser, mock, resvs)
  await toggleOff(page)
  await shot(page, name)
  await context.close()
}
await browser.close()
