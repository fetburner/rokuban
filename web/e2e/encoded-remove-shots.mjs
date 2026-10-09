// PoC（参照用・マージしない）: エンコード版の削除 UI の画面案を撮る。
//   cd web && corepack pnpm build && corepack pnpm preview --port 4392 --strictPort &
//   OUT_DIR=/tmp/encoded-remove-shots node e2e/encoded-remove-shots.mjs
import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4392'
const OUT = process.env.OUT_DIR
mkdirSync(OUT, { recursive: true })
const GB = 1_000_000_000

const base = {
  site: 'default', ruleId: 5, source: 'rule', serviceName: 'ＮＨＫ総合１・岡山', channelType: 'GR', channel: '27',
  networkId: 32678, serviceId: 5168, eventId: 1, description: '国内外の主要なニュースを、現場からの中継や解説を交えて詳しくお伝えします。',
  series: null, durationMs: 1_800_000, status: 'finished', cmDetection: { state: 'detected', ranges: [] },
  startAt: '2026-09-30T10:00:00.000Z', startedAt: '2026-09-30T10:00:00.000Z', endedAt: '2026-09-30T10:30:00.000Z', createdAt: '2026-09-30T10:35:00.000Z',
}
const withOriginal = {
  ...base, id: 1, title: '映像の世紀 バタフライエフェクト', keepOriginal: 'always', sizeBytes: 6.4 * GB,
  encodedAssets: [
    { profile: 'h265-1080p', sizeBytes: 2.1 * GB },
    { profile: 'h264-720p', sizeBytes: 0.9 * GB },
    { profile: 'h264-cut', cut: true, sizeBytes: 0.7 * GB },
  ],
  encodeProfiles: ['h265-1080p', 'h264-720p', 'h264-cut'],
}
const noOriginal = {
  ...base, id: 2, title: 'ドキュメント７２時間', keepOriginal: 'until_encoded',
  encodedAssets: [
    { profile: 'h265-1080p', sizeBytes: 2.3 * GB },
    { profile: 'h264-cut', cut: true, sizeBytes: 0.8 * GB },
  ],
  encodeProfiles: ['h265-1080p', 'h264-cut'],
}
const lastCopy = {
  ...base, id: 3, title: 'ブラタモリ', keepOriginal: 'until_encoded',
  encodedAssets: [{ profile: 'h264-720p', sizeBytes: 0.9 * GB }],
  encodeProfiles: ['h264-720p'],
}
const extra = [4, 5, 6, 7].map((id, i) => ({
  ...(i % 2 ? noOriginal : withOriginal), id, title: ['ダーウィンが来た！', 'NHKスペシャル', '鶴瓶の家族に乾杯', 'プロフェッショナル'][i],
  startAt: `2026-09-2${i}T10:00:00.000Z`,
}))
const all = [withOriginal, noOriginal, lastCopy, ...extra]

async function api({ path: p, url, json, route }) {
  if (p === '/api/sites') return json(['default'])
  if (p === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (p === '/api/encode-profiles') return json([{ name: 'h265-1080p' }, { name: 'h264-720p' }, { name: 'h264-cut', cut: true }])
  if (p === '/api/rules') return json([{ id: 5, name: 'ドキュメンタリー', keepOriginal: 'always' }])
  if (p === '/api/events') return sseKeepAlive(route)
  if (p === '/api/recordings' && route.request().method() === 'GET') return json(url.searchParams.get('trash') === 'true' ? [] : all)
  const m = /^\/api\/recordings\/(\d+)$/.exec(p)
  if (m) return json(all.find((r) => r.id === Number(m[1])))
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(p)) {
    return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: '<svg xmlns="http://www.w3.org/2000/svg" width="160" height="90"><rect width="160" height="90" fill="#263650"/><path d="M0 70 60 25l36 32 22-20 42 34v19H0Z" fill="#485d7c"/></svg>' })
  }
  if (/^\/api\/media\//.test(p)) return route.fulfill({ status: 404 })
  return json([])
}

const browser = await launchBrowser()
async function open(mock, id, viewport = { width: 1280, height: 800 }) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 2, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
  await ctx.addInitScript((m) => { window.__mock = m }, mock)
  const page = await ctx.newPage()
  await installApiStubs(page, api)
  await page.goto(`${URL_BASE}/recordings/${id}`)
  if (viewport.width < 600) await page.getByRole('tab', { name: '版' }).click()
  await page.locator('[data-testid="recording-assets-group"]').waitFor()
  await page.locator('[data-testid="recording-assets-group"]').scrollIntoViewIfNeeded()
  return page
}
async function section(page, name, extraBottom = 0) {
  const box = await page.locator('[data-testid="recording-assets-group"]').boundingBox()
  box.y += await page.evaluate(() => window.scrollY)
  await page.screenshot({ path: path.join(OUT, `${name}.png`), clip: { x: box.x - 12, y: box.y - 12, width: box.width + 24, height: box.height + 24 + extraBottom }, fullPage: true, animations: 'disabled' })
}
const shot = (page, name) => page.screenshot({ path: path.join(OUT, `${name}.png`), animations: 'disabled' })

let page
if (!process.env.ONLY_BULK) {
// 案 A
page = await open('A', 1)
await section(page, 'a-rows')
await page.getByRole('button', { name: 'h265-1080pを削除' }).click()
await page.getByRole('alertdialog').waitFor()
await shot(page, 'dialog-reversible')
await page.context().close()

page = await open('A', 2)
await page.getByRole('button', { name: 'h265-1080pを削除' }).click()
await page.getByRole('alertdialog').waitFor()
await shot(page, 'dialog-irreversible')
await page.context().close()

page = await open('A', 3)
await section(page, 'a-last-copy')
await page.context().close()

// 案 B
page = await open('B', 1)
await page.getByRole('button', { name: 'h265-1080pのその他の操作' }).click()
await page.getByRole('menu').waitFor()
await section(page, 'b-menu', 90)
await page.context().close()

// 案 C
page = await open('C', 1)
await section(page, 'c-idle')
await page.getByRole('button', { name: '容量を空ける…' }).click()
await page.getByLabel('h265-1080pを選ぶ').check()
await page.getByLabel('h264-720pを選ぶ').check()
await section(page, 'c-picked')
await page.context().close()

page = await open('C', 2)
await page.getByRole('button', { name: '容量を空ける…' }).click()
await page.getByLabel('h265-1080pを選ぶ').check()
await section(page, 'c-picked-irreversible')
await page.context().close()

// スマホ
page = await open('A', 2, { width: 390, height: 844 })
await section(page, 'phone-a-rows')
await page.getByRole('button', { name: 'h265-1080pを削除' }).click()
await page.getByRole('alertdialog').waitFor()
await shot(page, 'phone-dialog-irreversible')
await page.context().close()
}

// 一括
{
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
  page = await ctx.newPage()
  await installApiStubs(page, api)
  await page.goto(`${URL_BASE}/recordings`)
  await page.getByText('ブラタモリ').first().waitFor()
  await page.getByRole('button', { name: '選択', exact: true }).click()
  await page.keyboard.press('ControlOrMeta+a')
  await shot(page, 'bulk-bar')
  await page.getByRole('button', { name: '版を削除…' }).click()
  await page.getByRole('alertdialog').waitFor()
  await shot(page, 'bulk-dialog')
  await ctx.close()
}
await browser.close()
console.log('ok')
