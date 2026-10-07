// PoC(H-8): 「どれか 1 つを選ぶ」切替の現状と統一案の比較用スクリーンショット。
// window.__mock.uni で programs の ViewChips（V→segmented）と home-mode-toggle（N→tabs）を差し替える。
//   cd web && pnpm build && pnpm preview --port 4400 --strictPort &
//   OUT=./h8-mocks E2E_URL=http://localhost:4400 node e2e/h8-shots.mjs
import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { installApiStubs, launchBrowser, log, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4400'
const OUT = process.env.OUT ?? './h8-mocks'
mkdirSync(OUT, { recursive: true })
const NOW = new Date('2026-10-02T12:00:00+09:00')
const STAMP = NOW.toISOString()
const iso = (day, hour) => new Date(`2026-10-${String(day).padStart(2, '0')}T${String(hour).padStart(2, '0')}:00:00+09:00`).toISOString()

const reservation = ({ id, day, hour, title, series, state = 'active', source = 'rule', ruleId = 8 }) => ({
  site: 'default',
  programId: 9000 + id,
  source,
  ...(ruleId === undefined ? {} : { ruleId }),
  state,
  title,
  serviceName: 'ＮＨＫ総合１・東京',
  channelType: 'GR',
  startAt: iso(day, hour),
  durationMs: 45 * 60_000,
  createdAt: STAMP,
  updatedAt: STAMP,
  series,
  skip: false,
})
const reservations = [
  reservation({ id: 2, day: 2, hour: 20, title: '金曜アニメ 第2話', series: '金曜アニメ' }),
  reservation({ id: 3, day: 3, hour: 20, title: '金曜アニメ 第3話', series: '金曜アニメ', state: 'detached' }),
  reservation({ id: 4, day: 4, hour: 21, title: 'ひとり予約', series: '録画の無いシリーズ', source: 'manual', ruleId: undefined }),
  reservation({ id: 6, day: 6, hour: 23, title: '別の番組', series: '別のシリーズ', source: 'manual', ruleId: undefined }),
]
const rules = [{ id: 8, name: '金曜アニメ', enabled: true, priority: 10, keepOriginal: 'always', createdAt: STAMP, updatedAt: STAMP }]
const shelves = [{ value: '金曜アニメ', title: '金曜アニメ 第3話', count: 12, playableCount: 10, unwatchedCount: 4, latestStartAt: iso(1, 20), representativeId: 701 }]

async function apiHandler({ path: p, json, route }) {
  if (p === '/api/events') return sseKeepAlive(route)
  if (p === '/api/sites') return json(['default'])
  if (p === '/api/capabilities') return json({ live: true })
  if (p === '/api/reservations') return json(reservations)
  if (p === '/api/rules') return json(rules)
  if (p === '/api/recording-shelves') return json(shelves)
  if (p === '/api/encode-queue') return json({ queued: 2, running: 1 })
  return json([])
}

const targets = {
  'rec-series': { url: '/recordings', loc: (pg) => pg.getByRole('group', { name: '録画とシリーズの表示切替' }) },
  'library-trash': { url: '/recordings', loc: (pg) => pg.getByRole('button', { name: 'ごみ箱', exact: true }).first() },
  'list-grid': { url: '/programs', loc: (pg) => pg.getByRole('group', { name: '表示形式' }) },
  daystrip: { url: '/programs', loc: (pg) => pg.getByRole('group', { name: '日付' }), curOnly: true },
  'all-attention': { url: '/reservations', loc: (pg) => pg.getByRole('group', { name: '予約の絞り込み' }) },
  'series-time': { url: '/reservations', loc: (pg) => pg.getByRole('group', { name: '予約のまとめ方' }) },
  'watch-manage': { url: '/', loc: (pg) => pg.getByRole('group', { name: 'ホームの表示切替' }) },
}
const sizes = {
  1280: { viewport: { width: 1280, height: 800 }, dsf: 2 },
  390: { viewport: { width: 390, height: 844 }, dsf: 2, mobile: true },
}

const browser = await launchBrowser()
async function open(w, uni, url) {
  const s = sizes[w]
  const ctx = await browser.newContext({
    viewport: s.viewport,
    deviceScaleFactor: s.dsf,
    isMobile: !!s.mobile,
    hasTouch: !!s.mobile,
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
  })
  const page = await ctx.newPage()
  await page.addInitScript((u) => { window.__mock = { uni: u } }, uni)
  await page.clock.install({ time: NOW })
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + url, { waitUntil: 'domcontentloaded' })
  return { ctx, page }
}

for (const prefix of ['cur', 'uni']) {
  for (const w of [1280, 390]) {
    for (const [id, t] of Object.entries(targets)) {
      if (prefix === 'uni' && t.curOnly) continue
      const { ctx, page } = await open(w, prefix === 'uni', t.url)
      const loc = t.loc(page)
      await loc.waitFor({ timeout: 8000 }).catch(() => {})
      await page.waitForTimeout(800)
      const file = path.join(OUT, `${prefix}-${id}-${w}.png`)
      const box = await loc.boundingBox().catch(() => null)
      if (box) {
        const bottom = Math.min(box.y + box.height + 24, sizes[w].viewport.height)
        await page.screenshot({ path: file, clip: { x: 0, y: 0, width: sizes[w].viewport.width, height: bottom } })
      } else {
        log(`  (absent at ${w}px: ${id}) -> top 160px`)
        await page.screenshot({ path: file, clip: { x: 0, y: 0, width: sizes[w].viewport.width, height: 160 } })
      }
      await ctx.close()
    }
  }
}
for (const [name, url, w] of [['uni-recordings-1280', '/recordings', 1280], ['uni-reservations-1280', '/reservations', 1280], ['uni-programs-390', '/programs', 390]]) {
  const { ctx, page } = await open(w, true, url)
  await page.waitForTimeout(1000)
  await page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: true })
  await ctx.close()
}
await browser.close()
log('done')
