// H-4（文字サイズ）の比較用スクリーンショットを撮る参照用スクリプト。判定はしない。
// 変種は実行時に CSS を足して作る（ソースは変えない）。
//   pnpm build && pnpm preview --port 4393 --strictPort &
//   E2E_URL=http://localhost:4393 OUT=/path/to/dir node e2e/h4-shots.mjs
import { mkdirSync } from 'node:fs'
import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4393'
const OUT = process.env.OUT ?? './h4-mocks'
mkdirSync(OUT, { recursive: true })
const SITE = 'default'
const MIN = 60_000
const NOW = new Date('2026-10-07T12:00:00+09:00')
const now = NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const CSS = {
  base: '',
  'grid-11': '[class*="text-[10px]"]{font-size:11px !important}',
  'xs-13': ':root{--text-xs:13px !important}',
  scale:
    '[class*="text-[10px]"],[class*="text-[11px]"],[class*="text-[9px]"]{font-size:12px !important}',
}

const names = ['NHK総合', 'NHKEテレ', '日テレ', 'テレビ朝日', 'TBS']
const services = names.map((name, i) => ({
  id: 3273601000 + i, networkId: 32736, serviceId: 1024 + i, name, channelType: 'GR',
  channel: String(27 + i), remoteControlKeyId: i + 1, hasLogoData: false, hasPrograms: true,
}))
let pid = 1000
const programs = []
const mk = (svc, startMin, dur, name, extra = {}) => {
  const p = {
    programId: ++pid, networkId: svc.networkId, serviceId: svc.serviceId, eventId: pid,
    startAt: iso(now + startMin * MIN), endAt: iso(now + (startMin + dur) * MIN),
    durationMs: dur * MIN, name, description: '', genres: [pid % 8], isFree: true, ...extra,
  }
  programs.push(p)
  return p
}
const reservedPrograms = []
services.forEach((svc, i) => {
  // 先頭の列に 5/15(予約)/30/60 分を隣接させる。ほかの列も尺を変えて並べる
  const seq = i === 0 ? [5, 15, 30, 60] : [[15, 30, 5, 60], [30, 5, 15, 60], [60, 15, 5, 30], [5, 5, 30, 60]][i - 1]
  let t = 10
  seq.forEach((d, j) => {
    const extra = i === 1 && j === 1 ? { intent: 'skip' } : {}
    const p = mk(svc, t, d, `${svc.name}の${d}分番組`, extra)
    if ((i === 0 && j === 1) || (i === 2 && j === 0) || (i === 3 && j === 3)) reservedPrograms.push(p)
    t += d
  })
})
const reservation = (p) => ({
  id: p.programId, site: SITE, programId: p.programId, source: 'manual', state: 'active',
  title: p.name, serviceName: names[p.serviceId - 1024], channelType: 'GR', startAt: p.startAt,
  durationMs: p.durationMs, createdAt: iso(now - 3_600_000), updatedAt: iso(now - 3_600_000),
  series: null, skip: false,
})
const reservations = reservedPrograms.map(reservation)

const recTitles = ['朝の連続ドラマ 第12話', 'ニュース7', '週刊ニュースダイジェスト', 'アニメ 春の嵐 #5', '紀行 日本の古道', 'バラエティ 笑いの殿堂', '映画 夜明けの街', 'ドキュメンタリー 海の記憶']
const recs = recTitles.map((title, i) => ({
  id: i + 1, site: SITE, source: i % 2 ? 'rule' : 'manual', serviceName: names[i % 5], channelType: 'GR',
  channel: String(27 + (i % 5)), networkId: 32736, serviceId: 1024 + (i % 5), eventId: i + 1, title,
  startAt: iso(now - (i + 2) * 3_600_000), durationMs: (30 + 15 * (i % 3)) * MIN,
  status: i === 3 ? 'failed' : 'finished', keepOriginal: i % 3 === 0 ? 'always' : 'auto',
  cmDetection: { state: 'disabled' }, createdAt: iso(now - (i + 2) * 3_600_000), sizeBytes: 1_500_000_000,
}))
const live = {
  ...recs[0], id: 99, title: '録画中のニュース', status: 'recording', startAt: iso(now - 20 * MIN),
  durationMs: 40 * MIN, startedAt: iso(now - 20 * MIN), createdAt: iso(now - 20 * MIN),
}
const thumb = (id) =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360"><rect width="640" height="360" fill="hsl(${(id * 47) % 360} 35% 55%)"/></svg>`

async function api({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/reservations') return json(reservations)
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/recordings') {
    return json(url.searchParams.get('status') === 'recording' ? [live] : recs)
  }
  if (path === '/api/recordings/continue-watching') return json([])
  if (/\/thumbnail$/.test(path)) {
    const id = Number(/recordings\/(\d+)\//.exec(path)?.[1] ?? 1)
    return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: thumb(id) })
  }
  if (path === `/api/sites/${SITE}/services`) return json(services)
  if (path === `/api/sites/${SITE}/programs` && method === 'GET') return json(programs)
  if (path === '/api/programs/search' && method === 'POST') {
    return json(programs.map((p) => ({ site: SITE, programId: p.programId, networkId: p.networkId, serviceId: p.serviceId, startAt: p.startAt, name: p.name })))
  }
  if (/\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(path)) return json({ extended: {}, audios: [] })
  return json([])
}

const browser = await launchBrowser()
const PHONE = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }
// 番組表は lg（64rem）未満では出ない。phone では撮れないので 1024px で代用する
const LG = { viewport: { width: 1024, height: 844 }, deviceScaleFactor: 2 }
const DESK = { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2 }

/** shot は 1 枚撮る。`ready` で待つ要素、`clip` は page を受けて矩形を返す。 */
async function shot(file, { dev = DESK, css = 'base', url, scale, ready, clip, el }) {
  const context = await browser.newContext({ ...dev, locale: 'ja-JP', timezoneId: 'Asia/Tokyo', colorScheme: 'light' })
  if (scale) await context.addInitScript((s) => localStorage.setItem('rokuban:programs:grid-scale', s), String(scale))
  const page = await context.newPage()
  await page.clock.setFixedTime(NOW)
  await installApiStubs(page, api)
  await page.goto(BASE + url, { waitUntil: 'domcontentloaded' })
  if (CSS[css]) await page.addStyleTag({ content: CSS[css] })
  await ready(page)
  await page.waitForTimeout(500)
  const target = `${OUT}/${file}.png`
  if (el) await el(page).first().screenshot({ path: target })
  else if (clip) await page.screenshot({ path: target, clip: await clip(page) })
  else await page.screenshot({ path: target })
  console.log(file)
  await context.close()
}

const gridReady = async (p) => {
  await p.getByTestId('program-grid').waitFor()
  await p.locator('[data-testid="program-grid-cell"]').first().waitFor()
}
const recReady = (p) => p.getByText(recTitles[0]).first().waitFor()
const GRID = '/programs?view=grid'

// (a)
for (const v of ['10', '11']) {
  const css = v === '11' ? 'grid-11' : 'base'
  for (const [label, dev] of [['lg1024', LG], ['desktop', DESK]]) {
    await shot(`a-grid-${v}-${label}-120`, { dev, css, url: GRID, ready: gridReady })
  }
  await shot(`a-grid-${v}-lg1024-240`, { dev: LG, css, url: GRID, scale: 240, ready: gridReady })
  await shot(`a-crop-${v}`, {
    dev: { ...DESK, deviceScaleFactor: 3 }, css, url: GRID, ready: gridReady,
    clip: async (p) => {
      const ids = [programs[0], programs[1], programs[2]].map((x) => x.programId)
      const boxes = await Promise.all(ids.map((id) => p.locator(`[data-program-id="${id}"]`).first().boundingBox()))
      const x = Math.min(...boxes.map((b) => b.x))
      const y = Math.min(...boxes.map((b) => b.y))
      const bottom = Math.max(...boxes.map((c) => c.y + c.height))
      return { x: x - 8, y: y - 8, width: 260, height: bottom - y + 16 }
    },
  })
}

// (b)
for (const v of ['12', '13']) {
  const css = v === '13' ? 'xs-13' : 'base'
  await shot(`b-xs-${v}-recordings`, { css, url: '/recordings', ready: recReady })
  await shot(`b-xs-${v}-programs`, { css, url: '/programs?view=list', ready: (p) => p.getByText(programs[0].name).first().waitFor() })
  await shot(`b-crop-${v}`, {
    css, url: '/recordings', ready: recReady,
    el: (p) => p.getByText(recTitles[3]).first().locator('xpath=ancestor::*[self::li or self::article][1]'),
  })
}

// (c)
const places = {
  'home-manage-phone': { dev: PHONE, url: '/?mode=ops', ready: (p) => p.getByTestId('home-timeline-block').first().waitFor() },
  'home-watch-phone': { dev: PHONE, url: '/?mode=watch', ready: (p) => p.getByTestId('home-next-watch-thumbnail').waitFor() },
  'daystrip-phone': { dev: PHONE, url: `/programs?view=list&cond=${encodeURIComponent('{"genres":[7]}')}`, ready: (p) => p.getByTestId('day-match-count').first().waitFor() },
  'thumb-phone': { dev: PHONE, url: '/?mode=watch', ready: (p) => p.getByTestId('home-hero-station').waitFor(), el: (p) => p.getByTestId('home-next-watch-thumbnail') },
  'home-manage-desktop': { dev: DESK, url: '/?mode=ops', ready: (p) => p.getByTestId('home-timeline-block').first().waitFor() },
  'legend-desktop': { dev: DESK, url: GRID, ready: gridReady },
}
for (const [place, o] of Object.entries(places)) {
  for (const [tag, css] of [['before', 'base'], ['after', 'scale']]) await shot(`c-${place}-${tag}`, { ...o, css })
}
await browser.close()
