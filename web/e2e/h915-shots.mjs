// H-9 / H-15 比較用スクリーンショット（使い捨て PoC）。
//   cd web && corepack pnpm build && corepack pnpm preview --port 4397 --strictPort &
//   OUT=./h915-mocks node e2e/h915-shots.mjs
import { mkdirSync } from 'node:fs'
import { installApiStubs, launchBrowser, log, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4397'
const OUT = process.env.OUT ?? './h915-mocks'
mkdirSync(OUT, { recursive: true })

const mk = (id, title, serviceName, h) => ({
  id, site: 'default', source: 'manual', serviceName, channelType: 'GR', channel: '27',
  networkId: 32678, serviceId: 5168, eventId: id, title,
  startAt: `2026-10-0${h}T12:00:00Z`, durationMs: 1_800_000, status: 'finished',
  keepOriginal: 'always', cmDetection: { state: 'disabled' }, createdAt: '2026-10-01T12:30:00Z',
})
const recs = [mk(1, '朝のニュース', 'ＮＨＫ総合', 1), mk(2, '夜のドラマ 第3話', 'ＯＨＫ', 2), mk(3, '週末アニメ', 'ＫＳＳ', 3)]

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/events') return sseKeepAlive(route)
  if (/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') return json(recs)
  return json([])
}

const SEL = 'nav[aria-label="主ナビゲーション"]:not([data-testid]) a[aria-current=page]'
const CSS = {
  cur: '',
  bar: `${SEL}{background:transparent!important;box-shadow:inset 3px 0 0 var(--foreground);font-weight:700}`,
  invert: `${SEL}{background:var(--foreground)!important;color:var(--background)!important;font-weight:700}`,
  'strong-bg': `${SEL}{background:var(--scanline)!important;color:var(--paper)!important;font-weight:700}`,
}
const RING = {
  'ring-cur': '',
  'ring-solid': `:focus-visible{outline:2px solid var(--ring)!important;outline-offset:2px!important;box-shadow:none!important}`,
  'ring-dark': `:focus-visible{outline:2px solid var(--scanline)!important;outline-offset:2px!important;box-shadow:none!important}`,
}

const browser = await launchBrowser()
const probe = await (await browser.newContext()).newPage()
/** pixels は PNG を canvas に描いて論理座標 (x,y) の RGB を返す。origin は clip 左上。 */
async function pixels(buf, pts, origin = [0, 0], scale = 2) {
  return probe.evaluate(async ({ b64, pts, scale, origin }) => {
    const img = new Image()
    img.src = 'data:image/png;base64,' + b64
    await img.decode()
    const c = document.createElement('canvas')
    c.width = img.width
    c.height = img.height
    const g = c.getContext('2d')
    g.drawImage(img, 0, 0)
    return pts.map(([x, y]) => [...g.getImageData(Math.round((x - origin[0]) * scale), Math.round((y - origin[1]) * scale), 1, 1).data].slice(0, 3))
  }, { b64: buf.toString('base64'), pts, scale, origin })
}
const lum = (rgb) => {
  const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }
  return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2])
}
const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05) }
const results = []

async function open(theme, { collapsed = false, tooltip = false, css = '' } = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2, locale: 'ja-JP', timezoneId: 'Asia/Tokyo', colorScheme: theme })
  const page = await context.newPage()
  await page.addInitScript(({ theme, collapsed, tooltip }) => {
    window.__mock = { tooltip }
    try { localStorage.setItem('rokuban:sidebar:collapsed', collapsed ? '1' : '0') } catch {}
    if (theme === 'dark') document.documentElement.classList.add('dark')
  }, { theme, collapsed, tooltip })
  await page.clock.setFixedTime(new Date('2026-10-07T12:00:00+09:00'))
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
  await page.getByText('朝のニュース').first().waitFor({ timeout: 15000 })
  if (css) await page.addStyleTag({ content: css })
  await page.mouse.move(900, 700)
  await page.waitForTimeout(300)
  return { context, page }
}
const nav = (page) => page.locator('nav[aria-label="主ナビゲーション"]:not([data-testid])')

// ---- H-9 現在地 ----
for (const theme of ['light', 'dark']) {
  for (const v of Object.keys(CSS)) {
    const { context, page } = await open(theme, { css: CSS[v] })
    const cur = await nav(page).locator('a[aria-current=page]').boundingBox()
    const nb = await nav(page).boundingBox()
    const buf = await page.screenshot({ path: `${OUT}/h9-${v}-${theme}.png`, clip: { x: 0, y: 0, width: 560, height: 360 } })
    const y = cur.y + cur.height / 2
    const [bg, row, bar] = await pixels(buf, [[cur.x - 4, y], [cur.x + 10, y], [cur.x + 1.5, y]])
    const r = v === 'bar' ? ratio(bar, bg) : ratio(row, bg)
    results.push(`H-9 row ${v} ${theme}: ${r.toFixed(2)} (rgb(${v === 'bar' ? bar : row}) vs bg rgb(${bg}))`)
    await context.close()
  }
}

// ---- H-9 フォーカスリング ----
async function best(b, buf, origin) {
  const y = b.y + b.height / 2
  const pts = []
  for (let x = b.x - 8; x <= b.x + 3; x += 0.5) pts.push([x, y])
  const px = await pixels(buf, pts, origin)
  const bg = px[0]
  let m = 1
  let mp = bg
  px.forEach((p) => { const r = ratio(p, bg); if (r > m) { m = r; mp = p } })
  return [m, mp, bg]
}
for (const theme of ['light', 'dark']) {
  for (const v of Object.keys(RING)) {
    const { context, page } = await open(theme, { css: RING[v] })
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press('Tab')
      const ok = await page.evaluate(() => !!document.activeElement?.matches('nav:not([data-testid]) a:not([aria-current])'))
      if (ok) break
    }
    await page.waitForTimeout(500)
    const el = await page.evaluateHandle(() => document.activeElement)
    const b = await el.asElement().boundingBox()
    const buf = await page.screenshot({ path: `${OUT}/h9-focus-${v}-${theme}.png`, clip: { x: 0, y: 0, width: 560, height: 360 } })
    const [m, mp, bg] = await best(b, buf, [0, 0])
    results.push(`H-9 focus-link ${v} ${theme}: ${m.toFixed(2)} (ring rgb(${mp}) vs bg rgb(${bg}))`)
    for (const [kind, loc] of [
      ['button', page.getByRole('button', { name: '絞り込み', exact: true })],
      ['input', page.getByRole('searchbox', { name: '番組名・説明で検索' })],
    ]) {
      await page.keyboard.press('Tab')
      await loc.focus()
      await page.waitForTimeout(500)
      const cb = await loc.boundingBox()
      const clip = { x: Math.max(0, cb.x - 40), y: Math.max(0, cb.y - 30), width: 520, height: cb.height + 60 }
      const file = kind === 'button' ? `h9-focus-${v}-controls-${theme}.png` : `h9-focus-${v}-controls-input-${theme}.png`
      const cbuf = await page.screenshot({ path: `${OUT}/${file}`, clip })
      const [cm, cmp, cbg] = await best(cb, cbuf, [clip.x, clip.y])
      results.push(`H-9 focus-${kind} ${v} ${theme}: ${cm.toFixed(2)} (rgb(${cmp}) vs bg rgb(${cbg}))`)
    }
    await context.close()
  }
}

// ---- H-15 ----
{
  const clip = { x: 0, y: 0, width: 320, height: 460 }
  let { context, page } = await open('light', { collapsed: true, tooltip: true })
  await nav(page).getByRole('link', { name: '録画', exact: true }).hover()
  await page.locator('div.bg-popover[data-open]').waitFor({ timeout: 3000 })
  await page.waitForTimeout(400)
  await page.screenshot({ path: `${OUT}/h15-hover.png`, clip })
  const t0 = Date.now()
  await nav(page).getByRole('link', { name: '予約', exact: true }).hover()
  await page.locator('div.bg-popover[data-open]', { hasText: '予約' }).waitFor({ timeout: 3000 })
  results.push(`H-15 hover 予約 right after 録画 tooltip: visible after ${Date.now() - t0}ms`)
  await context.close()
  ;({ context, page } = await open('light', { collapsed: true, tooltip: true }))
  for (let i = 0; i < 12; i++) {
    await page.keyboard.press('Tab')
    if (await page.evaluate(() => document.activeElement?.textContent?.trim() === '予約')) break
  }
  await page.locator('div.bg-popover[data-open]').waitFor({ timeout: 3000 })
  await page.waitForTimeout(400)
  await page.screenshot({ path: `${OUT}/h15-focus.png`, clip })
  await context.close()
  ;({ context, page } = await open('light', { collapsed: false, tooltip: true }))
  await nav(page).getByRole('link', { name: '録画', exact: true }).hover()
  await page.waitForTimeout(1000)
  results.push(`H-15 expanded tooltip count: ${await page.locator('div.bg-popover[data-open]').count()}`)
  await page.screenshot({ path: `${OUT}/h15-expanded.png`, clip })
  await context.close()
}
results.forEach((r) => log(r))
await browser.close()
