// 番組表の site 和集合化を実ブラウザで判定する。
// jsdom はレイアウトを計算しないため、複数 site の列が実際に並び、各列の
// 番組セルが描画されることはここで測る（CLAUDE.md のテスト規律）。
import {
  ListProgramsResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const FIXED_NOW = new Date('2026-08-12T10:50:00+09:00')
const nowMs = FIXED_NOW.getTime()
const HOUR = 3_600_000
const iso = (ms) => new Date(ms).toISOString()
const sites = ['tokyo', 'osaka']

const services = {
  tokyo: {
    id: 1001024,
    networkId: 10,
    serviceId: 1024,
    name: '東京総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
  osaka: {
    id: 2001024,
    networkId: 20,
    serviceId: 1024,
    name: '大阪総合',
    channelType: 'GR',
    channel: '24',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
}

function programsFor(site) {
  const service = services[site]
  return Array.from({ length: 24 }, (_, index) => {
    const startAt = nowMs + index * HOUR
    return {
      programId: (site === 'tokyo' ? 1 : 2) * 1_000_000 + index,
      networkId: service.networkId,
      serviceId: service.serviceId,
      eventId: index + 1,
      startAt: iso(startAt),
      endAt: iso(startAt + HOUR),
      durationMs: HOUR,
      name: `${service.name} 番組 ${index + 1}`,
      description: '',
      genres: [3],
      isFree: true,
    }
  })
}

const ng = []

async function apiHandler({ path: pathname, json }) {
  if (pathname === '/api/sites') return json(sites)
  if (pathname === '/api/breakers') return json([])
  if (pathname === '/api/reservations') return json([])
  if (pathname === '/api/capacity/overages') return json([])
  if (pathname === '/api/capabilities') return json({ live: false })
  for (const site of sites) {
    if (pathname === `/api/sites/${site}/services`) return json([services[site]])
    if (pathname === `/api/sites/${site}/tuners`) return json([])
    if (pathname === `/api/sites/${site}/programs`) return json(programsFor(site))
  }
  if (/\/overlaps$/.test(pathname)) return json({ count: 0, reservations: [] })
  if (/\/reservation$/.test(pathname)) return json(null)
  return json([])
}

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()}`)

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ...sites.map((site) => [`${site} service`, ListServicesResponseItem, services[site]]),
    ...sites.flatMap((site) =>
      programsFor(site).map((program, index) => [
        `${site} programs[${index}]`,
        ListProgramsResponseItem,
        program,
      ]),
    ),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)

await page.goto(`${URL_BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

log('\n=== ① 複数 site の番組表列 ===')
const columns = page.getByTestId('program-grid-column')
await columns.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('① 番組表の列が 1 本も描画されない')
})

const columnCount = await columns.count()
log(`  描画された列: ${columnCount}`)
if (columnCount !== 2) ng.push(`① 番組表の列が 2 本ではない（${columnCount} 本）`)

if (columnCount >= 2) {
  const boxes = await columns.evaluateAll((nodes) =>
    nodes.map((node) => {
      const rect = node.getBoundingClientRect()
      return {
        site: node.getAttribute('data-site'),
        programCount: node.querySelectorAll('[data-testid="program-grid-cell"]').length,
        left: rect.left,
        width: rect.width,
      }
    }),
  )
  log(`  列の実測: ${JSON.stringify(boxes)}`)
  if (new Set(boxes.map((box) => box.site)).size !== 2) {
    ng.push('① 番組表の列が site ごとに識別されていない')
  }
  if (boxes.some((box) => box.programCount === 0 || box.width <= 0)) {
    ng.push('① site 列の一部に番組セルが無い、または幅が 0')
  }
  if (boxes[0].left >= boxes[1].left) ng.push('① site 列の順序または実レイアウトが不正')
}

await finish(ng, browser)
