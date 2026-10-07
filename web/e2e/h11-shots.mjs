// H-11 検索画面の主操作配置・空状態の比較用スクリーンショット（throwaway PoC）。
//   corepack pnpm build && corepack pnpm preview --port 4398 --strictPort &
//   OUT=./h11-mocks E2E_URL=http://localhost:4398 node e2e/h11-shots.mjs
import fs from 'node:fs'
import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4398'
const OUT = process.env.OUT ?? './h11-mocks'
fs.mkdirSync(OUT, { recursive: true })
const SITE = 'default'

const services = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'ＮＨＫＢＳプレミアム４Ｋ',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3273701032,
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
    hasPrograms: true,
  },
]

const restoredCondition = {
  genres: [0],
  services: [{ networkId: 32736, serviceId: 1024 }],
}

/**
 * matchedProgramIds は検索スタブが返す programId の集合（④で使う）。
 *
 * 検索 API（`POST /api/programs/search`）は表示に要るメタデータ込みの行
 * （`networkId` / `serviceId` / `startAt` / `durationMs` / `name` / `isFree`）を
 * 返す。詳細 GET を 1 件ずつ叩く N+1 は無い（実物と同じ形）。
 *
 * **件数を 20 件にしているのは、結果がスクロールの余地を作るため。** 数件だと
 * 結果の先頭へ寄せる操作がドキュメント末尾で頭打ちになり、`scroll-margin-top`
 * （`sticky` なページヘッダの下に潜らせないための余白）を落としても④が通って
 * しまう。20 件なら頭打ちにならないので、その分の判定が生きる。
 */
const matchedProgramIds = Array.from({ length: 20 }, (_, i) => 3273610240001 + i)

/** programDetail は `GET /api/sites/{site}/programs/{id}` の応答。 */
function programDetail(id, index) {
  const startAt = new Date(Date.UTC(2026, 7, 20, 12 + index, 0, 0)).toISOString()
  const endAt = new Date(Date.UTC(2026, 7, 20, 12 + index, 30, 0)).toISOString()
  return {
    programId: id,
    networkId: 32736,
    serviceId: 1024,
    eventId: 1 + index,
    startAt,
    endAt,
    durationMs: 30 * 60 * 1000,
    name: `ニュース ${index + 1}`,
    description: '',
    genres: [0],
    isFree: false,
  }
}

/** apiHandler は /search の描画に要る `/api/**` の応答を作る。 */
async function apiHandler({ path: p, json, route }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false, cmDetect: false })
  if (p === `/api/sites/${SITE}/services`) return json(services)
  if (p === '/api/reservations') return json([])
  const intent = /^\/api\/sites\/([^/]+)\/programs\/(\d+)\/intent$/.exec(p)
  if (intent !== null && route.request().method() === 'PUT') {
    return route.fulfill({ status: 204 })
  }
  if (p === '/api/programs/search')
    return json(
      matchedProgramIds.map((programId, index) => {
        const detail = programDetail(programId, index)
        return {
          site: SITE,
          programId,
          networkId: detail.networkId,
          serviceId: detail.serviceId,
          startAt: detail.startAt,
          durationMs: detail.durationMs,
          name: detail.name,
          isFree: detail.isFree,
        }
      }),
    )
  const detail = /^\/api\/sites\/[^/]+\/programs\/(\d+)$/.exec(p)
  if (detail !== null) {
    const id = Number(detail[1])
    const index = matchedProgramIds.indexOf(id)
    if (index < 0) {
      return route.fulfill({ status: 404, contentType: 'application/json', body: '{}' })
    }
    return json(programDetail(id, index))
  }
  return json([])
}


const browser = await launchBrowser()

async function open(ctxOpts, mock, { openDetails = true } = {}) {
  const context = await browser.newContext({ colorScheme: 'light', ...ctxOpts })
  const page = await context.newPage()
  await page.addInitScript((m) => { window.__mock = m }, mock)
  await page.route('**/api/events**', sseKeepAlive)
  await installApiStubs(page, async (a) => {
    if (a.path === '/api/events') return sseKeepAlive(a.route)
    if (a.path === '/api/programs/search' && mock.delay) await new Promise((r) => setTimeout(r, mock.delay))
    return apiHandler(a)
  })
  await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: '詳細条件を表示', exact: true }).waitFor({ timeout: 15000 })
  await page.getByText('サイト一覧を取得中…').waitFor({ state: 'hidden', timeout: 15000 })
  if (openDetails) {
    await page.getByRole('button', { name: '詳細条件を表示', exact: true }).click()
    await page.waitForTimeout(300)
  }
  return page
}

const phone = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }
const desktop = { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 2 }

for (const v of ['cur', 'after', 'sticky']) {
  const page = await open(phone, { buttons: v })
  await page.screenshot({ path: `${OUT}/${v}-390.png` })
  await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))
  await page.waitForTimeout(300)
  await page.screenshot({ path: `${OUT}/${v}-390-scrolled.png` })
  if (v === 'sticky') {
    const m = await page.evaluate(() => {
      const r = (s) => document.querySelector(s)?.getBoundingClientRect()
      const bar = r('[data-testid="actions"]')
      const nav = r('nav[aria-label="主ナビゲーション"]')
      return { barHeight: bar?.height, barTop: bar?.top, navHeight: nav?.height, navTop: nav?.top, vh: innerHeight }
    })
    console.log('sticky bar @390x844', JSON.stringify(m))
  }
  await page.context().close()
}

{
  const page = await open({ ...phone, viewport: { width: 390, height: 500 } }, { buttons: 'sticky' }, { openDetails: false })
  await page.getByLabel('テキスト条件 1 の値').focus()
  await page.waitForTimeout(300)
  await page.screenshot({ path: `${OUT}/sticky-390-keyboard.png` })
  const m = await page.evaluate(() => {
    const bar = document.querySelector('[data-testid="actions"]').getBoundingClientRect()
    const inp = document.activeElement.getBoundingClientRect()
    return { barTop: bar.top, barH: bar.height, inputTop: inp.top, inputBottom: inp.bottom, vh: innerHeight }
  })
  console.log('sticky @390x500', JSON.stringify(m))
  await page.context().close()
}

for (const [name, mock] of [['empty-cur', {}], ['empty-neutral', { empty: 'neutral' }]]) {
  const page = await open(desktop, mock, { openDetails: false })
  await page.screenshot({ path: `${OUT}/${name}-1280.png` })
  await page.context().close()
}

{
  const page = await open(desktop, { delay: 60000 }, { openDetails: false })
  await page.getByRole('button', { name: '検索', exact: true }).click()
  await page.waitForTimeout(600)
  await page.screenshot({ path: `${OUT}/loading-1280.png` })
  await page.context().close()
}
await browser.close()
process.exit(0)
