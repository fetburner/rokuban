// PoC(H-16): 録画一覧の絞り込みパネルとジャンル選択の比較用スクリーンショットと実測。
// window.__mock.genre（recording-filters.tsx の PoC 分岐）で見た目を切り替える。
//
//   cd web && corepack pnpm build && corepack pnpm preview --port 4394 --strictPort &
//   OUT=./h16-mocks E2E_URL=http://localhost:4394 node e2e/h16-shots.mjs
import { mkdirSync } from 'node:fs'
import { installApiStubs, launchBrowser, log, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4394'
const OUT = process.env.OUT ?? './h16-mocks'
mkdirSync(OUT, { recursive: true })

const names = [
  ['GR', 'ＮＨＫ総合', 32736], ['GR', 'ＮＨＫＥテレ', 32737], ['GR', '日本テレビ', 32738], ['GR', 'テレビ朝日', 32739],
  ['GR', 'ＴＢＳ', 32740], ['GR', 'テレビ東京', 32741], ['GR', 'フジテレビ', 32742], ['GR', 'ＴＯＫＹＯ　ＭＸ', 32743],
  ['BS', 'ＮＨＫ　ＢＳ', 101], ['BS', 'ＢＳ日テレ', 141], ['BS', 'ＢＳ朝日', 151], ['BS', 'ＢＳ−ＴＢＳ', 161],
  ['CS', 'スカパー！', 55], ['CS', 'ＴＢＳチャンネル１', 296], ['CS', '日テレ　ＮＥＷＳ２４', 300], ['CS', 'ＷＯＷＯＷ', 200],
]
const netOf = { GR: 32736, BS: 4, CS: 7 }
const services = names.map(([channelType, name, serviceId], i) => ({
  id: netOf[channelType] * 100000 + serviceId, networkId: netOf[channelType], serviceId, name, channelType,
  channel: String(i), remoteControlKeyId: channelType === 'GR' ? i + 1 : 0, hasLogoData: false, hasPrograms: true,
}))
const rule = (id, name) => ({ id, name, enabled: true, priority: 10, keepOriginal: 'always', cmDetection: { state: 'disabled' }, createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z' })
const rules = [rule(8, '平日夜のニュースを録る'), rule(3, '朝の連続ドラマを録る'), rule(5, '週末アニメ'), rule(9, '映画 BS 録画')]
// 2 拠点: takamatsu は GR のみ。先頭 2 局は tokyo と networkId/serviceId が同じ（= 同じ Service.id）。
const takamatsu = [
  ...services.slice(0, 2),
  ...['ＮＨＫ高松', 'ＮＨＫ　Ｅ高松', 'ＲＮＣ西日本', 'ＫＳＢ瀬戸内海', 'ＥＢＣ愛媛', 'ＴＳＣ'].map((name, i) => ({
    id: 31921 * 100000 + 53248 + i, networkId: 31921, serviceId: 53248 + i, name, channelType: 'GR',
    channel: String(13 + i), remoteControlKeyId: i + 1, hasLogoData: false, hasPrograms: true,
  })),
]
let twoSites = false
const recording = {
  id: 1, site: 'default', source: 'rule', ruleId: 8, serviceName: 'ＯＨＫ', channelType: 'GR', channel: '27',
  networkId: 32678, serviceId: 5168, eventId: 1, title: 'ルール由来の録画', startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000, status: 'finished', keepOriginal: 'always', cmDetection: { state: 'disabled' }, createdAt: '2026-01-02T12:30:00Z',
}

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(twoSites ? ['tokyo', 'takamatsu'] : ['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/rules') return json(rules)
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) return json(twoSites && path.includes('takamatsu') ? takamatsu : services)
  if (/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') return json(twoSites ? [{ ...recording, site: 'tokyo' }, { ...recording, id: 2, site: 'takamatsu', title: '高松の録画' }] : [recording])
  if (['/api/breakers', '/api/encode-profiles'].includes(path)) return json([])
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  return json([])
}

const browser = await launchBrowser()

async function shot(file, genre, query = '', { openMenu = false, height = 800 } = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height }, deviceScaleFactor: 2, locale: 'ja-JP', timezoneId: 'Asia/Tokyo', colorScheme: 'light' })
  const page = await ctx.newPage()
  await page.addInitScript((g) => { window.__mock = { genre: g } }, genre)
  await installApiStubs(page, apiHandler)
  await page.goto(`${URL_BASE}/recordings${query}`, { waitUntil: 'domcontentloaded' })
  await page.getByText('ルール由来の録画').waitFor({ timeout: 15000 })
  await page.getByRole('button', { name: /絞り込み/ }).click()
  const panel = page.getByRole('dialog', { name: '絞り込み' })
  await panel.getByRole('group', { name: '種別' }).waitFor({ timeout: 15000 })
  await panel.getByRole('combobox', { name: 'ルール' }).waitFor({ timeout: 15000 })
  const m = await panel.evaluate((el) => {
    const r = el.getBoundingClientRect()
    let sc = el
    for (const d of [el, ...el.querySelectorAll('*')]) {
      if (d.clientHeight > 100 && d.scrollHeight > d.clientHeight + 1 && getComputedStyle(d).overflowY !== 'visible') { sc = d; break }
    }
    return {
      top: r.top, bottom: r.bottom, height: r.height, scrollHeight: sc.scrollHeight, clientHeight: sc.clientHeight, scrolls: sc.scrollHeight > sc.clientHeight + 1,
      sections: [...el.querySelectorAll('section')].map((s) => `${s.querySelector('h3')?.textContent}=${Math.round(s.getBoundingClientRect().height)}`),
    }
  })
  m.fitsViewport = m.bottom <= height && m.top >= 0
  log(file, JSON.stringify(m))
  if (openMenu) {
    await panel.getByRole('button', { name: /^ジャンル: / }).click()
    await page.getByRole('dialog', { name: 'ジャンル' }).waitFor({ timeout: 15000 })
  }
  await page.waitForTimeout(300)
  await page.screenshot({ path: `${OUT}/${file}` })
  await ctx.close()
}

if (process.env.TWO_SITES === '1') {
  twoSites = true
  await shot('two-sites-current-1280.png', 'current')
  await shot('two-sites-hide-shown-1280.png', 'hide-shown')
  await shot('two-sites-menu-1280.png', 'menu')
  await shot('two-sites-hide-shown-1280x720.png', 'hide-shown', '', { height: 720 })
  await shot('two-sites-menu-1280x720.png', 'menu', '', { height: 720 })
  await browser.close()
  process.exit(0)
}
await shot('current-1280.png', 'current')
await shot('hide-unnamed-1280.png', 'hide')
await shot('genre-menu-1280.png', 'menu', '?genre=3&genre=5')
await shot('genre-menu-open-1280.png', 'menu', '?genre=3&genre=5', { openMenu: true })
await shot('url-genre12-1280.png', 'hide', '?genre=12')
await shot('url-genre12-shown-1280.png', 'hide-shown', '?genre=12')
await browser.close()
