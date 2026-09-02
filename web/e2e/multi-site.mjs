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
const sites = ['tokyo', 'takamatsu']

const services = {
  tokyo: {
    id: 401024,
    networkId: 4,
    serviceId: 1024,
    name: '共有BS',
    channelType: 'BS',
    channel: 'BS15_0',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: true,
  },
  takamatsu: {
    id: 401024,
    networkId: 4,
    serviceId: 1024,
    name: '共有BS',
    channelType: 'BS',
    channel: 'BS15_0',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: true,
  },
}

function programsFor(site) {
  const service = services[site]
  return Array.from({ length: 24 }, (_, index) => {
    const startAt = nowMs + index * HOUR
    return {
      programId: 1_000_000 + index,
      networkId: service.networkId,
      serviceId: service.serviceId,
      eventId: index + 1,
      startAt: iso(startAt),
      endAt: iso(startAt + HOUR),
      durationMs: HOUR,
      name: `共有番組 ${index + 1}`,
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
  if (pathname === '/api/capacity/overages') {
    return json(
      sites.map((site) => ({
        site,
        startAt: iso(nowMs + HOUR),
        endAt: iso(nowMs + 2 * HOUR),
        shortfall: site === 'tokyo' ? 1 : 2,
        jammedTypes: ['BS'],
      })),
    )
  }
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
const duplicateKeyErrors = []
page.on('console', (message) => {
  if (message.type() === 'error' && /same key|unique "key"/i.test(message.text())) {
    duplicateKeyErrors.push(message.text())
  }
})
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

const headerSites = await page.getByTestId('program-grid-header-site').allTextContents()
log(`  可視の site 名: ${JSON.stringify(headerSites)}`)
if (headerSites.sort().join(',') !== [...sites].sort().join(',')) {
  ng.push('① 同名の共有 BS 列に可視の site 名が無い')
}

log('\n=== ② 容量帯は site の列内、ラベルは非衝突 ===')
const bands = page.getByTestId('capacity-band')
await bands.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('② 容量帯が描画されない')
})
const bandBoxes = await bands.evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { site: node.getAttribute('data-site'), left: rect.left, right: rect.right, width: rect.width }
  }),
)
const columnBoxes = await columns.evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { site: node.getAttribute('data-site'), left: rect.left, right: rect.right }
  }),
)
log(`  帯の実測: ${JSON.stringify(bandBoxes)}`)
for (const band of bandBoxes) {
  const matchingColumns = columnBoxes.filter((column) => column.site === band.site)
  const insideOwnSite = matchingColumns.some(
    (column) => band.left >= column.left - 0.5 && band.right <= column.right + 0.5,
  )
  const intersectsOtherSite = columnBoxes.some(
    (column) => column.site !== band.site && band.left < column.right && band.right > column.left,
  )
  if (!insideOwnSite || intersectsOtherSite || band.width <= 0) {
    ng.push(`② ${band.site ?? 'site 不明'} の容量帯が同じ site の列内に限定されていない`)
  }
}

const labelBoxes = await page.getByTestId('capacity-band-label').evaluateAll((nodes) =>
  nodes.map((node) => {
    const rect = node.getBoundingClientRect()
    return { top: rect.top, bottom: rect.bottom }
  }),
)
if (labelBoxes.length !== 2) ng.push(`② 容量帯ラベルが 2 件ではない（${labelBoxes.length} 件）`)
if (
  labelBoxes.length === 2 &&
  labelBoxes[0].top < labelBoxes[1].bottom &&
  labelBoxes[1].top < labelBoxes[0].bottom
) {
  ng.push('② 別 site の同時刻の容量帯ラベルが重なっている')
}

log('\n=== ③ 一覧行でも site を識別でき、React key が衝突しない ===')
await page.goto(`${URL_BASE}/programs`, { waitUntil: 'domcontentloaded' })
const listRows = page.locator('li[data-program-id="1000000"]')
await listRows.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ 共有番組の一覧行が描画されない')
})
if ((await listRows.count()) !== 2) ng.push(`③ 同一 programId の一覧行が 2 件ではない`)
for (const site of sites) {
  const row = listRows.filter({ has: page.getByText(site, { exact: true }) })
  if ((await row.count()) !== 1) ng.push(`③ ${site} の可視名を持つ一覧行が一意でない`)
}
if (duplicateKeyErrors.length > 0) {
  ng.push(`③ React key の重複警告: ${duplicateKeyErrors.join(' / ')}`)
}

await finish(ng, browser)
