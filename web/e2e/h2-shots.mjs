import { launchBrowser, installApiStubs } from './lib.mjs'
const BASE = 'http://localhost:4392'
const OUT = process.env.OUT_DIR ?? 'h2-mocks/'
const V = process.argv[2] // 'current' or variant
const now = new Date('2026-08-13T00:00:00.000Z')
const iso = (ms) => new Date(ms).toISOString()
const H = 3600e3
const rules = [
  { id: 1, name: '朝ドラ', enabled: true, priority: 10, keepOriginal: 'always', encodeProfiles: ['hevc-1080p'], textMatches: [{ target: 'name', mode: 'keyword', value: '連続テレビ小説' }], channelTypes: ['GR'], createdAt: iso(+now - 100 * H), updatedAt: iso(+now - 100 * H) },
  { id: 2, name: 'ニュース', enabled: true, priority: 5, keepOriginal: 'until_encoded', textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }], createdAt: iso(+now - 100 * H), updatedAt: iso(+now - 100 * H) },
  { id: 3, name: 'とても長いルール名のサンプルで折り返しや省略の挙動を確認する用のルール', enabled: false, priority: 1, keepOriginal: 'always', textMatches: [{ target: 'name', mode: 'keyword', value: '長い名前の番組' }], createdAt: iso(+now - 100 * H), updatedAt: iso(+now - 100 * H) },
]
const resv = [1, 2].map((i) => ({ id: i, site: 'default', programId: 9000 + i, source: 'rule', ruleId: 1, state: 'active', title: '連続テレビ小説', serviceName: 'NHK総合', channelType: 'GR', startAt: iso(+now + i * H), durationMs: 900000, createdAt: iso(+now), updatedAt: iso(+now), series: null, skip: false }))
const service = { id: 3273601024, networkId: 32736, serviceId: 1024, name: 'NHK総合', channelType: 'GR', channel: '27', remoteControlKeyId: 1, hasLogoData: false, hasPrograms: true }
const programs = [{ programId: 123501, networkId: 32736, serviceId: 1024, eventId: 1, startAt: '2026-08-13T01:00:00.000Z', endAt: '2026-08-13T02:00:00.000Z', durationMs: 3600000, name: 'ニュース速報と話題の特集', description: '', genres: [0], isFree: true }]
async function handler({ path, json }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: false })
  if (path === '/api/rules') return json(rules)
  if (path === '/api/reservations') return json(resv)
  if (path === '/api/encode-profiles') return json([{ name: 'hevc-1080p', container: 'mp4' }])
  if (path === '/api/sites/default/services') return json([service])
  if (path === '/api/sites/default/programs') return json(programs)
  if (/\/overlaps$/.test(path)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(path)) return json({ extended: {}, audios: [] })
  return json([])
}
const browser = await launchBrowser()
async function shot(name, w, scheme, fn) {
  const mobile = w === 390
  const ctx = await browser.newContext({ viewport: { width: w, height: mobile ? 844 : 800 }, deviceScaleFactor: mobile ? 2 : 1, isMobile: mobile, hasTouch: mobile, colorScheme: scheme, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
  const page = await ctx.newPage()
  await page.clock.setFixedTime(now)
  await page.addInitScript((v) => { window.__H2 = v }, V)
  await installApiStubs(page, handler)
  await fn(page)
  await page.waitForTimeout(400)
  await page.screenshot({ path: OUT + name })
  await ctx.close()
}
const rulesPage = async (page) => { await page.goto(BASE + '/rules'); await page.getByText('ニュース').first().waitFor() }
const progPage = async (page) => {
  await page.goto(BASE + '/programs')
  const row = page.locator('li[data-program-id="123501"]')
  await row.waitFor({ timeout: 15000 })
  await row.locator('button[aria-expanded]').click()
  await row.getByRole('link', { name: 'この番組名で検索' }).waitFor()
  await page.waitForTimeout(300)
  await row.scrollIntoViewIfNeeded()
}
if (V === 'current') {
  await shot('current-rules-390.png', 390, 'light', rulesPage)
  await shot('current-rules-1280.png', 1280, 'light', rulesPage)
  await shot('current-program-390.png', 390, 'light', progPage)
} else {
  await shot(`${V}-rules-390-light.png`, 390, 'light', rulesPage)
  await shot(`${V}-rules-390-dark.png`, 390, 'dark', rulesPage)
  await shot(`${V}-rules-1280-light.png`, 1280, 'light', rulesPage)
  await shot(`${V}-program-390-light.png`, 390, 'light', progPage)
}
await browser.close()
