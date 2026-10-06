// 案 B（期間をツールバーへ）のラフのスクリーンショットを撮る。判定はしない（PoC 用）。
//
//   cd web && npx vite build && npx vite preview --port 4179 --strictPort &
//   node e2e/mock-period-shots.mjs <出力ディレクトリ>
import { chromium } from 'playwright'
import { installApiStubs, sseKeepAlive } from './lib.mjs'

const BASE = 'http://localhost:4179'
const OUT = process.argv[2]

const shows = [
  ['星屑のアトリエ', 'ＯＨＫ', '2026-07-03', 13],
  ['夜明けのレールウェイ', 'ＲＳＫ', '2026-07-05', 12],
  ['となりの魔導書店', 'ＫＳＢ', '2026-07-08', 12],
  ['銀河鉄塔の約束', 'ＴＳＣ', '2026-04-06', 24],
  ['ひだまり探偵団', 'ＲＮＣ', '2026-10-02', 1],
  ['蒼穹のリコーダー', 'ＮＨＫ総合', '2026-04-10', 12],
]
const recordings = []
let id = 1
for (const [title, serviceName, first, n] of shows) {
  for (let i = 0; i < n; i++) {
    const start = new Date(new Date(first + 'T15:00:00Z').getTime() + i * 7 * 86_400_000)
    if (start > new Date('2026-10-07T00:00:00Z')) break
    recordings.push({
      id: id++, site: 'default', source: 'rule', ruleId: 1, serviceName, channelType: 'GR', channel: '27',
      networkId: 32678, serviceId: 5168, eventId: id, title: `${title} 第${i + 1}話`, startAt: start.toISOString(),
      durationMs: 1_800_000, status: 'finished', keepOriginal: 'always', cmDetection: { state: 'disabled' },
      createdAt: start.toISOString(), genres: [7],
    })
  }
}
recordings.sort((a, b) => b.startAt.localeCompare(a.startAt))

async function api({ path, url, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/rules') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') {
    const from = url.searchParams.get('from'), to = url.searchParams.get('to')
    return json(recordings.filter((r) => (!from || r.startAt >= from) && (!to || r.startAt < to)).slice(0, 40))
  }
  return json([])
}

const browser = await chromium.launch()
async function shoot(name, viewport, query, act) {
  const ctx = await browser.newContext({ viewport, locale: 'ja-JP', timezoneId: 'Asia/Tokyo', deviceScaleFactor: 2 })
  const page = await ctx.newPage()
  await page.clock.setFixedTime(new Date('2026-10-07T12:00:00+09:00'))
  await installApiStubs(page, api)
  await page.goto(BASE + '/recordings' + query)
  await page.getByText(/第\d+話/).first().waitFor({ timeout: 15000 })
  const ys = []
  for (const loc of [page.getByRole('searchbox'), page.getByRole('button', { name: /2026 夏|期間|〜/ }).first(), page.getByRole('button', { name: /絞り込み/ }), page.getByRole('combobox', { name: '並び順' })]) {
    const r = await loc.boundingBox()
    ys.push(`${Math.round(r.y)}:${Math.round(r.width)}`)
  }
  console.log(name, '検索 / 期間 / 絞り込み / 並び順 の y:幅 =', ys.join('  '))
  if (act) await act(page)
  await page.waitForTimeout(300)
  await page.screenshot({ path: `${OUT}/${name}.png` })
  await ctx.close()
}
const summer = '?from=2026-06-30T15%3A00%3A00.000Z&to=2026-09-30T15%3A00%3A00.000Z&genre=7'
const openMenu = (p) => p.getByRole('button', { name: /2026 夏|期間|〜/ }).first().click()

const custom = '?from=2026-08-09T15%3A00%3A00.000Z&to=2026-08-16T15%3A00%3A00.000Z&genre=7'
const openPanel = (p) => p.getByRole('button', { name: /絞り込み/ }).click()
await shoot('1-desktop-menu', { width: 1280, height: 720 }, summer, openMenu)
await shoot('2-360-summer', { width: 360, height: 780 }, summer)
await shoot('3-390-summer', { width: 390, height: 844 }, summer)
await shoot('4-390-period-sheet', { width: 390, height: 844 }, summer, openMenu)
await shoot('5-390-filter-sheet', { width: 390, height: 844 }, summer, openPanel)
await shoot('6-360-custom-sheet', { width: 360, height: 780 }, custom, openMenu)
await browser.close()
