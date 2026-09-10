// 番組表の表示形式を端末へ保存し、初回フレームのちらつきを起こさないことを
// 実ブラウザで判定する（issue #722）。
//
// jsdom では `matchMedia` の初期値と localStorage の読み込みは検証できても、
// リロード直後にリストが一度も描画されないことは測れない。ここでは DOM の
// 出現順を MutationObserver で記録する。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-view
import {
  ListProgramsResponseItem,
  ListServicesResponseItem,
} from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const FIXED_NOW = new Date('2026-08-14T12:00:00+09:00')
const HOUR = 3_600_000
const VIEW_KEY = 'rokuban:programs:view'
const ng = []

const service = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

const program = {
  programId: 72201,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 72201,
  startAt: new Date(FIXED_NOW.getTime() + HOUR).toISOString(),
  endAt: new Date(FIXED_NOW.getTime() + 2 * HOUR).toISOString(),
  durationMs: HOUR,
  name: 'ニュース7',
  description: '',
  genres: [0],
  isFree: true,
}

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/reservations') return json([])
  if (path === '/api/capacity/overages') return json([])
  if (path === '/api/encode-profiles') return json([])
  if (path === '/api/sites/default/services') return json([service])
  if (/^\/api\/sites\/default\/programs\/\d+\/overlaps$/.test(path)) {
    return json({ count: 0, reservations: [] })
  }
  if (path === '/api/sites/default/programs' && method === 'GET') {
    const start = Date.parse(url.searchParams.get('start') ?? '')
    const end = Date.parse(url.searchParams.get('end') ?? '')
    return json(
      Number.isFinite(start) &&
      Number.isFinite(end) &&
      Date.parse(program.endAt) > start &&
      Date.parse(program.startAt) < end
        ? [program]
        : [],
    )
  }
  return json([])
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ['program', ListProgramsResponseItem, program],
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

// `bounded-page-content` は ProgramsPage のリスト分岐、`program-grid` はグリッド分岐
// にだけ存在する。同期初期化前は最初の分岐が必ずリストになるので、
// 「最後にグリッドが出た」だけでなく「最初からグリッドだった」ことを確認できる。
await page.addInitScript(() => {
  window.__programViewHistory = []
  let lastView = null
  const recordView = () => {
    const main = document.querySelector('main')
    const view = main?.querySelector('[data-testid="bounded-page-content"]')
      ? 'list'
      : main?.querySelector('[data-testid="program-grid"]')
        ? 'grid'
        : null
    if (view !== null && view !== lastView) {
      window.__programViewHistory.push(view)
      lastView = view
    }
  }
  new MutationObserver(recordView).observe(document, {
    childList: true,
    subtree: true,
  })
  requestAnimationFrame(recordView)
})

await installApiStubs(page, apiHandler)

log('\n=== ① 番組表を選ぶと localStorage に保存される ===')
await page.clock.setFixedTime(FIXED_NOW)
await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
await page.getByText(program.name).waitFor({ timeout: 15000 })
await page.getByRole('button', { name: '番組表' }).click()
await page.getByTestId('program-grid').waitFor({ timeout: 15000 })

const savedView = await page.evaluate((key) => localStorage.getItem(key), VIEW_KEY)
if (savedView !== 'grid') {
  ng.push(`① 番組表の選択が localStorage に保存されない（${savedView}）`)
}
if (new URL(page.url()).search !== '?view=grid') {
  ng.push(`① 選択後の URL が想定外（${page.url()}）`)
}

// URL の view を外した素の `/programs` でも、保存値から番組表を復元する。
// ここを挟まないと、次の reload は URL の `view=grid` だけでも通ってしまう。
await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
await page.getByTestId('program-grid').waitFor({ timeout: 15000 })
if (new URL(page.url()).search !== '') {
  ng.push(`① 保存値を試す素の URL に query が残っている（${page.url()}）`)
}

log('\n=== ② リロード後の最初の表示が番組表で、リストへちらつかない ===')
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByTestId('program-grid').waitFor({ timeout: 15000 })
const viewHistory = await page.evaluate(() => window.__programViewHistory)
log(`  表示分岐の出現順: ${JSON.stringify(viewHistory)}`)
if (viewHistory[0] !== 'grid') {
  ng.push(`② リロード後の最初の表示が番組表ではない（${JSON.stringify(viewHistory)}）`)
}
if (viewHistory.includes('list')) {
  ng.push(`② リロード後にリストが一度描画されている（${JSON.stringify(viewHistory)}）`)
}

await context.close()
await finish(ng, browser)
