import { ListRecordingsResponseItem } from '../src/api/zod.ts'

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
const ng = []
const fixedNow = new Date('2026-08-12T21:34:00+09:00')
const recording = {
  id: 11,
  site: 'default',
  source: 'manual',
  serviceName: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  networkId: 32736,
  serviceId: 1024,
  eventId: 11,
  title: 'キーボードスクロール確認',
  startAt: '2026-08-12T12:24:00.000Z',
  durationMs: 1_800_000,
  status: 'recording',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: '2026-08-12T12:24:00.000Z',
  startedAt: '2026-08-12T12:24:00.000Z',
}

log('\n=== ホーム管理モード: 時間軸の左右キー ===')
await validateFixturesOrExit([['timeline recording', ListRecordingsResponseItem, recording]], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await page.clock.setFixedTime(fixedNow)
await installApiStubs(page, async ({ path, url, json, route }) => {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/recordings/continue-watching') return json([])
  if (path === '/api/recordings') {
    return json(url.searchParams.get('status') === 'recording' ? [recording] : [])
  }
  if (path === '/api/reservations' || path === '/api/breakers' || path === '/api/storage') {
    return json([])
  }
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/capacity/overages') return json([])
  return json([])
})

await page.goto(`${URL_BASE}/?mode=ops`, { waitUntil: 'domcontentloaded' })
const timeline = page.getByRole('region', { name: '時間軸。横にスクロールできます' })
await timeline.waitFor({ timeout: 10_000 })
await page.getByTestId('home-timeline-block').waitFor({ timeout: 10_000 })
await page.waitForFunction(() => {
  const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
  return frame !== null && frame.scrollWidth > frame.clientWidth && frame.scrollLeft > 0
}, undefined, { timeout: 5_000 })

let tabFocused = false
for (let tabCount = 0; tabCount < 40; tabCount += 1) {
  await page.keyboard.press('Tab')
  tabFocused = await timeline.evaluate((frame) => document.activeElement === frame)
  if (tabFocused) break
}

const readStableScrollLeft = () => page.evaluate(() => new Promise((resolve, reject) => {
  const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
  if (frame === null) return reject(new Error('timeline frame disappeared'))
  let previous = frame.scrollLeft
  let stableFrames = 0
  const deadline = performance.now() + 2_000
  const sample = () => {
    const current = frame.scrollLeft
    stableFrames = current === previous ? stableFrames + 1 : 0
    previous = current
    if (stableFrames >= 5) return resolve(current)
    if (performance.now() >= deadline) return reject(new Error('timeline scroll did not settle'))
    requestAnimationFrame(sample)
  }
  requestAnimationFrame(sample)
}))

const before = await readStableScrollLeft()
await page.keyboard.press('ArrowLeft')
const afterLeft = await readStableScrollLeft()
await page.keyboard.press('ArrowRight')
const afterRight = await readStableScrollLeft()

log(`  1280×800 / Tab で tabindex region に到達=${tabFocused}`)
log(`  scrollLeft: initial=${before}, ArrowLeft=${afterLeft}, ArrowRight=${afterRight}`)
if (!tabFocused) ng.push('Tab で時間軸の枠に到達しない')
if (afterLeft >= before) ng.push(`← で左へスクロールしない（${before} → ${afterLeft}）`)
if (afterRight <= afterLeft) ng.push(`→ で右へスクロールしない（${afterLeft} → ${afterRight}）`)

await context.close()
await finish(ng, browser)
