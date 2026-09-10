// 番組表グリッドの空間的なキーボードナビゲーションの受け入れ判定。
//
// jsdom ではフォーカスとスクロール位置、仮想化後の DOM を同時に測れないため、
// 実ブラウザで次の 2 点を見る:
//   ① セルにフォーカスして ArrowRight を押すと、開始時刻を含む隣列のセルへ移る
//   ② 最初は仮想化で DOM に無い同列の遠い番組へ ArrowDown を押すと、スクロール後に
//      そのセルへフォーカスが移る
//
// 直す前は ArrowRight でフォーカスが移らず、ArrowDown でもネイティブの領域スクロール
// だけになるため、この判定は現行実装で失敗する。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:grid-navigation

import { ListProgramsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const FIXED_NOW = new Date('2026-08-12T09:00:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()
const ng = []

const serviceA = {
  id: 3273601024,
  networkId: 32736,
  serviceId: 1024,
  name: '左チャンネル',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

const serviceB = {
  id: 3273601032,
  networkId: 32736,
  serviceId: 1032,
  name: '右チャンネル',
  channelType: 'GR',
  channel: '26',
  remoteControlKeyId: 2,
  hasLogoData: false,
  hasPrograms: true,
}

const makeProgram = (programId, service, startMs, durationMs, name) => ({
  programId,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: programId,
  startAt: iso(startMs),
  endAt: iso(startMs + durationMs),
  durationMs,
  name,
  description: '',
  genres: [0],
  isFree: true,
})

const nearA = makeProgram(726001, serviceA, nowMs + 15 * 60_000, 30 * 60_000, '近くの左番組')
// nearA の開始時刻（09:15）を含むので、ArrowRight の優先規則でこれを選ぶ。
const nearB = makeProgram(726002, serviceB, nowMs, 60 * 60_000, '近くの右番組')
// 初期の可視窓から意図的に外す。ArrowDown はここまでスクロールしてからフォーカスする。
const farA = makeProgram(726003, serviceA, nowMs + 10 * 60 * 60_000, 30 * 60_000, '遠くの左番組')
const programs = [nearA, nearB, farA]

async function apiHandler({ path: p, json }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === '/api/reservations') return json([])
  if (p === '/api/capacity/overages') return json([])
  if (p === '/api/encode-profiles') return json([])
  if (p === `/api/sites/${SITE}/services`) return json([serviceA, serviceB])
  if (p === `/api/sites/${SITE}/programs`) return json(programs)
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
  return json([])
}

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['serviceA', ListServicesResponseItem, serviceA],
    ['serviceB', ListServicesResponseItem, serviceB],
    ...programs.map((program) => [`program ${program.programId}`, ListProgramsResponseItem, program]),
  ],
  ng,
)

await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)
await page.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

const grid = page.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 })

const cellFor = (program) =>
  page.locator(
    `[data-testid="program-grid-cell"][data-site="${SITE}"][data-program-id="${program.programId}"]`,
  )

const nearACell = cellFor(nearA)
const nearBCell = cellFor(nearB)
const farACell = cellFor(farA)
await nearACell.waitFor({ timeout: 10000 })
await nearBCell.waitFor({ timeout: 10000 })

// 遠い番組が最初から DOM にあると、②が仮想化境界を通らないので判定を止める。
if ((await farACell.count()) !== 0) {
  ng.push('② 遠い番組が初期表示から DOM にあり、仮想化後の移動を検証できない')
}

log('\n=== ① ArrowRight: 開始時刻を含む隣列へ移動 ===')
await nearACell.focus()
await nearACell.press('ArrowRight')
const rightFocus = await page.evaluate(() => {
  const active = document.activeElement
  return {
    programId: active?.getAttribute('data-program-id'),
    site: active?.getAttribute('data-site'),
  }
})
log(`  フォーカス先: ${JSON.stringify(rightFocus)}`)
if (rightFocus.programId !== String(nearB.programId) || rightFocus.site !== SITE) {
  ng.push(`① ArrowRight 後のフォーカスが隣列へ移らない（${JSON.stringify(rightFocus)}）`)
}

log('\n=== ② ArrowDown: 仮想化された遠い番組へ追従 ===')
const scrollBefore = await grid.evaluate((element) => element.scrollTop)
await nearACell.focus()
await nearACell.press('ArrowDown')
await page.waitForFunction(
  (programId) => document.activeElement?.getAttribute('data-program-id') === String(programId),
  farA.programId,
  { timeout: 10000 },
)
const scrollAfter = await grid.evaluate((element) => element.scrollTop)
const downFocus = await page.evaluate(() => ({
  programId: document.activeElement?.getAttribute('data-program-id'),
  site: document.activeElement?.getAttribute('data-site'),
}))
log(`  scrollTop: ${scrollBefore}px → ${scrollAfter}px / フォーカス先: ${JSON.stringify(downFocus)}`)
if (downFocus.programId !== String(farA.programId) || downFocus.site !== SITE) {
  ng.push(`② ArrowDown 後のフォーカスが遠い同列へ移らない（${JSON.stringify(downFocus)}）`)
}
if (scrollAfter <= scrollBefore) {
  ng.push(`② 目的セルへ追従してスクロールしない（${scrollBefore}px → ${scrollAfter}px）`)
}

await finish(ng, browser)
