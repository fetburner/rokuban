// H-6 / H-7 比較用スクリーンショット（使い捨て PoC）。
//   cd web && corepack pnpm build && corepack pnpm preview --port 4396 --strictPort &
//   OUT=./h67-mocks node e2e/h67-shots.mjs
import { mkdirSync } from 'node:fs'
import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4396'
const OUT = process.env.OUT ?? './h67-mocks'
mkdirSync(OUT, { recursive: true })

const recordings = Array.from({ length: 12 }, (_, i) => {
  const id = i + 1
  return {
    id, site: 'default', source: 'manual', serviceName: 'ＯＨＫ', channelType: 'GR', channel: '27',
    networkId: 32678, serviceId: 5168, eventId: id, title: `録画サンプル番組 第${id}回`,
    startAt: new Date(Date.parse('2026-01-01T12:00:00Z') + id * 3_600_000).toISOString(),
    durationMs: 1_800_000, status: 'finished', keepOriginal: 'always',
    cmDetection: { state: 'disabled' }, createdAt: '2026-01-02T12:30:00Z',
  }
})

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/events') return sseKeepAlive(route)
  if (/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') return json(recordings)
  return json([])
}

const browser = await launchBrowser()
async function open(mock) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2,
    locale: 'ja-JP', timezoneId: 'Asia/Tokyo', colorScheme: 'light',
  })
  const page = await context.newPage()
  await page.addInitScript((m) => { window.__mock = m }, mock)
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
  await page.getByText('録画サンプル番組 第1回').waitFor({ timeout: 15000 })
  return page
}
const shot = (page, name) => page.screenshot({ path: `${OUT}/${name}.png` })
const row = (page, n) => page.locator('[role=option]').nth(n - 1)
const enter = async (page) => {
  await page.getByRole('button', { name: '選択' }).click()
  await page.locator('[role=option]').first().waitFor()
}

// h6-current / h6-row-tab: row 1, 2 をクリック → Tab で 3 行目
for (const [h6, name] of [['current', 'h6-current'], ['tab', 'h6-row-tab']]) {
  const page = await open({ h6 })
  await enter(page)
  await row(page, 1).click()
  await (h6 === 'current' ? row(page, 2).getByRole('checkbox') : row(page, 2)).click()
  await page.keyboard.press('Tab')
  await shot(page, name)
  if (h6 === 'tab') {
    // Shift+click 範囲
    await page.close()
    const p2 = await open({ h6 })
    await enter(p2)
    await row(p2, 1).click()
    await row(p2, 4).click({ modifiers: ['Shift'] })
    await p2.mouse.move(0, 0)
    await shot(p2, 'h6-shift-range')
  }
  await page.close().catch(() => {})
}

// h6-roving
{
  const page = await open({ h6: 'roving' })
  await enter(page)
  await row(page, 1).focus()
  await page.keyboard.press('Space')
  await page.keyboard.press('ArrowDown')
  await page.keyboard.press('Space')
  await page.keyboard.press('ArrowDown')
  await shot(page, 'h6-roving')
}

// H-7
{
  const page = await open({ h7: true })
  await shot(page, 'h7-slash-hint')
  await page.keyboard.press('/')
  await shot(page, 'h7-slash-focused')
  await page.close()
}
{
  const page = await open({ h7: true })
  await page.keyboard.press('?')
  await page.getByText('キーボードショートカット').waitFor()
  await shot(page, 'h7-help')
  await page.close()
}
{
  const page = await open({ h7: true })
  await page.keyboard.press('g')
  await page.getByText('に続けて').waitFor()
  await shot(page, 'h7-g-pending')
}
await browser.close()
