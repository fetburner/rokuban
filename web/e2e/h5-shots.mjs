// PoC(H-5): 右クリックメニュー案の比較用スクリーンショット（使い捨て）。
// src/components/mock-context-menu.tsx（モック。ContextMenu は @base-ui/react のもの）と
// window.__mock で案を切り替える。判定ではなく画像を撮るだけ。
//
//   cd web && corepack pnpm build && corepack pnpm preview --port 4395 --strictPort &
//   OUT=./h5-mocks E2E_URL=http://localhost:4395 node e2e/h5-shots.mjs
import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4395'
const OUT = process.env.OUT ?? './h5-mocks'
mkdirSync(OUT, { recursive: true })

const STAMP = '2026-10-02T03:00:00Z'
const iso = (day, hour) =>
  new Date(`2026-10-${String(day).padStart(2, '0')}T${String(hour).padStart(2, '0')}:00:00+09:00`).toISOString()

const titles = ['金曜アニメ 第3話', 'ブラタモリ', '大相撲中継', '朝ドラ 第12話', 'ニュースウォッチ']
const recordings = titles.map((title, i) => ({
  id: i + 1,
  site: 'default',
  source: 'rule',
  serviceName: 'ＮＨＫ総合１・東京',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: i + 1,
  title,
  startAt: iso(1 + i, 20),
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: STAMP,
}))

const reservation = (id, day, hour, title, ruleId) => ({
  site: 'default',
  programId: 9000 + id,
  source: 'rule',
  ruleId,
  state: 'active',
  title,
  serviceName: 'ＮＨＫ総合１・東京',
  channelType: 'GR',
  startAt: iso(day, hour),
  durationMs: 45 * 60_000,
  createdAt: STAMP,
  updatedAt: STAMP,
  series: null,
  skip: false,
})
const reservations = [
  reservation(1, 9, 20, '金曜アニメ 第4話', 8),
  reservation(2, 9, 21, 'ブラタモリ', 8),
  reservation(3, 10, 20, '朝ドラ 第13話', 8),
]
const rules = [
  { id: 8, name: '金曜アニメ', enabled: true, priority: 10, keepOriginal: 'always', cmDetection: { state: 'disabled' }, textMatches: [{ target: 'name', mode: 'keyword', value: '金曜アニメ' }], createdAt: STAMP, updatedAt: STAMP },
  { id: 9, name: '朝ドラ', enabled: true, priority: 20, keepOriginal: 'always', cmDetection: { state: 'disabled' }, textMatches: [{ target: 'name', mode: 'keyword', value: '連続テレビ小説' }], createdAt: STAMP, updatedAt: STAMP },
]

async function apiHandler({ path: p, json, route }) {
  if (p === '/api/events') return sseKeepAlive(route)
  if (p === '/api/sites') return json(['default'])
  if (p === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (p === '/api/breakers') return json([])
  if (p === '/api/recordings') return json(recordings)
  if (p === '/api/reservations') return json(reservations)
  if (p === '/api/rules') return json(rules)
  if (/\/thumbnail$/.test(p)) return route.fulfill({ status: 404 })
  if (p === '/api/encode-queue') return json({ queued: 0, running: 0 })
  return json([])
}

const browser = await launchBrowser()

async function open(mock, route, wait) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 2,
    colorScheme: 'light',
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await page.addInitScript((m) => {
    window.__mock = m
    localStorage.setItem('rokuban:reservations:group', 'time')
  }, mock)
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + route, { waitUntil: 'domcontentloaded' })
  await page.getByText(wait).first().waitFor({ timeout: 15000 })
  return { context, page }
}
const shot = (page, name) => page.screenshot({ path: path.join(OUT, name), animations: 'disabled' })
const menuUp = (page) => page.getByRole('menu').waitFor({ timeout: 5000 })
const thumb = (page, n = 0) => page.locator('li:has(div.aspect-video)').nth(n).locator('div.aspect-video')

// 1, 4: 案 A 録画
{
  const { context, page } = await open({ variant: 'A' }, '/recordings', '金曜アニメ 第3話')
  await thumb(page).click({ button: 'right', force: true })
  await menuUp(page)
  await shot(page, 'A-recording.png')
  await page.getByRole('menuitem', { name: 'ごみ箱へ' }).click()
  await page.getByText('1 件をごみ箱へ移動').waitFor()
  await shot(page, 'A-recording-after-trash.png')
  await context.close()
}
// 2: 案 A 予約
{
  const { context, page } = await open({ variant: 'A' }, '/reservations', '金曜アニメ 第4話')
  await page.getByText('金曜アニメ 第4話').first().click({ button: 'right', force: true })
  await menuUp(page)
  await shot(page, 'A-reservation.png')
  await context.close()
}
// 3: 案 A ルール
{
  const { context, page } = await open({ variant: 'A' }, '/rules', '金曜アニメ')
  await page.locator('li').first().click({ button: 'right', force: true, position: { x: 600, y: 30 } })
  await menuUp(page)
  await shot(page, 'A-rule.png')
  await context.close()
}
// 5: 案 C
{
  const { context, page } = await open({ variant: 'C' }, '/recordings', '金曜アニメ 第3話')
  const link = page.locator('[data-mock=title-link]').first()
  await link.hover()
  await link.evaluate((el) => {
    el.style.outline = '2px dashed #2563eb'
    el.style.outlineOffset = '3px'
  })
  await shot(page, 'C-title-only-hover.png')
  await link.evaluate((el) => (el.style.outline = ''))
  await thumb(page).click({ button: 'right', force: true })
  await menuUp(page)
  await shot(page, 'C-title-only-menu.png')
  await context.close()
}
// 6: 選択モード
for (const withMenu of [true, false]) {
  const { context, page } = await open({ variant: 'A', menu: withMenu }, '/recordings', '金曜アニメ 第3話')
  await page.getByRole('button', { name: '選択' }).click()
  for (const t of titles.slice(0, 3)) await page.getByRole('checkbox', { name: `${t}を選択` }).click()
  if (withMenu) {
    await thumb(page, 1).click({ button: 'right', force: true })
    await menuUp(page)
  }
  await shot(page, withMenu ? 'S-selection-menu.png' : 'S-selection-none.png')
  await context.close()
}
await browser.close()
