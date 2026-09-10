// 番組表の短い番組を時間軸のズームで選べることを実ブラウザで判定する（issue #724）。
//
// jsdom は scrollTop・セルの実矩形・隣接セルへの実ポインタ座標を測れないため、
// ここで 5 分 / 10 分 / 30 分セルの高さと、隣接境界付近の選択先を測る。実装前は
// 5 分セルが 10px のままでズーム段階も無いため、縮尺の判定と選択判定が落ちる。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-grid-zoom
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { ListProgramsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'
const MINUTE = 60_000
const FIXED_NOW = new Date('2026-08-14T12:00:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()
const GRID_SCALE_KEY = 'rokuban:programs:grid-scale'
const HEADER_HEIGHT_PX = 36

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

function program(programId, startOffsetMinutes, durationMinutes, name) {
  const startMs = nowMs + startOffsetMinutes * MINUTE
  return {
    programId,
    networkId: service.networkId,
    serviceId: service.serviceId,
    eventId: programId,
    startAt: iso(startMs),
    endAt: iso(startMs + durationMinutes * MINUTE),
    durationMs: durationMinutes * MINUTE,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

// 同じ列で隣接させる。初期の 120px/時では 10px / 20px / 60px だが、
// 480px/時なら 40px / 80px / 240px になり、見た目の時間比例を保ったまま押せる。
const shortProgram = program(724001, 30, 5, '5分番組')
const tenMinuteProgram = program(724002, 35, 10, '10分番組')
const longProgram = program(724003, 45, 30, '30分番組')
const programs = [shortProgram, tenMinuteProgram, longProgram]

async function apiHandler({ path: p, json }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === '/api/breakers') return json([])
  if (p === '/api/reservations') return json([])
  if (p === '/api/capacity/overages') return json([])
  if (p === '/api/encode-profiles') return json([])
  if (p === `/api/sites/${SITE}/services`) return json([service])
  if (p === `/api/sites/${SITE}/programs`) return json(programs)
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
  return json([])
}

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()}`)

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ...programs.map((item, index) => [`programs[${index}]`, ListProgramsResponseItem, item]),
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
// 実行環境に前回の縮尺が残っていても、既定値から 480px/時への遷移を測れるようにする。
await context.addInitScript((key) => localStorage.removeItem(key), GRID_SCALE_KEY)
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)
await page.goto(`${URL_BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })

const grid = page.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 })
const scaleGroup = page.getByRole('group', { name: '時間軸の縮尺' })
await scaleGroup.waitFor({ timeout: 10000 })

const cellFor = (programId) =>
  page.locator(`[data-testid="program-grid-cell"][data-program-id="${programId}"]`)

const shortCell = cellFor(shortProgram.programId)
const tenMinuteCell = cellFor(tenMinuteProgram.programId)
const longCell = cellFor(longProgram.programId)
await shortCell.waitFor({ timeout: 10000 })
await tenMinuteCell.waitFor()
await longCell.waitFor()

// --- ① 3 段階の縮尺と高さの比例 ------------------------------------------
log('\n=== ① 5 分 / 10 分 / 30 分の高さ ===')
const initialHeights = await Promise.all(
  [shortCell, tenMinuteCell, longCell].map((cell) => cell.evaluate((el) => el.getBoundingClientRect().height)),
)
log(`  120px/時: ${JSON.stringify(initialHeights)}`)
if (initialHeights.some((height, index) => Math.abs(height - [10, 20, 60][index]) > 0.5)) {
  ng.push(`① 既定倍率の高さが 10/20/60px ではない（${JSON.stringify(initialHeights)}）`)
}

await page.getByRole('button', { name: '480 px/時' }).click()
await page.getByRole('button', { name: '480 px/時' }).waitFor({ state: 'attached' })
await page.waitForTimeout(100)

const zoomedHeights = await Promise.all(
  [shortCell, tenMinuteCell, longCell].map((cell) => cell.evaluate((el) => el.getBoundingClientRect().height)),
)
log(`  480px/時: ${JSON.stringify(zoomedHeights)}`)
const expectedZoomedHeights = [40, 80, 240]
if (zoomedHeights.some((height, index) => Math.abs(height - expectedZoomedHeights[index]) > 0.5)) {
  ng.push(`① 480px/時の高さが 40/80/240px ではない（${JSON.stringify(zoomedHeights)}）`)
}
const savedScale = await page.evaluate((key) => localStorage.getItem(key), GRID_SCALE_KEY)
if (savedScale !== '480') ng.push(`① 縮尺が localStorage に保存されない（${savedScale}）`)

// --- ② ズーム前後で同じ時刻を表示し続ける -------------------------------
log('\n=== ② ズーム前後の表示時刻 ===')
// ページは「今」の少し前へ初期スクロールする。倍率を戻して比較し、
// sticky ヘッダの下端を基準に同じ軸上時刻が残ることを測る。
await page.getByRole('button', { name: '120 px/時' }).click()
await page.waitForTimeout(100)
// 初期スクロール先は固定時刻やヘッダのレイアウトに依存するため、既知の軸上位置を
// 直接作ってから測る。今日の軸は 12:00 起点なので、番組（12:30〜13:15）が
// この範囲に残る位置を選ぶ。
await grid.evaluate((el) => {
  el.scrollTop = 60
  el.dispatchEvent(new Event('scroll'))
})
await page.waitForTimeout(100)
const beforeZoom = await grid.evaluate((el) => el.scrollTop)
const beforeVisibleHours = (beforeZoom - HEADER_HEIGHT_PX) / 120
await page.getByRole('button', { name: '480 px/時' }).click()
await page.waitForTimeout(100)
const afterZoom = await grid.evaluate((el) => el.scrollTop)
const afterVisibleHours = (afterZoom - HEADER_HEIGHT_PX) / 480
log(`  scrollTop: ${beforeZoom}px -> ${afterZoom}px`)
log(`  軸上の可視起点: ${beforeVisibleHours.toFixed(3)}h -> ${afterVisibleHours.toFixed(3)}h`)
if (Math.abs(beforeVisibleHours - afterVisibleHours) > 0.02) {
  ng.push(`② ズーム前後で可視起点の時刻がずれる（${beforeVisibleHours} -> ${afterVisibleHours}h）`)
}

// --- ③ 隣接セルの境界付近を押しても対象が入れ替わらない ---------------
log('\n=== ③ 隣接セルの誤選択なし ===')
async function clickAndCheck(cell, programId, edge) {
  await cell.scrollIntoViewIfNeeded()
  const box = await cell.boundingBox()
  if (!box) {
    ng.push(`③ programId=${programId} の矩形が取れない`)
    return
  }
  const y = edge === 'top' ? 1 : Math.max(1, box.height - 1)
  await cell.click({ position: { x: Math.min(20, box.width / 2), y } })
  await page.waitForTimeout(50)
  const pressed = await cell.getAttribute('aria-pressed')
  if (pressed !== 'true') ng.push(`③ ${programId} の ${edge} 端で対象を選べない（${pressed}）`)

  const selectedIds = await page.locator('[data-testid="program-grid-cell"][aria-pressed="true"]').evaluateAll((els) =>
    els.map((el) => el.getAttribute('data-program-id')),
  )
  if (selectedIds.length !== 1 || selectedIds[0] !== String(programId)) {
    ng.push(`③ ${programId} の ${edge} 端で別セルを選んだ（${JSON.stringify(selectedIds)}）`)
  }

  // #723 のモーダルが先にマージされても、同じセルを選ぶ判定をこのスクリプトで
  // 継続できるよう、通常の Escape で選択を閉じて次のセルへ進む。
  if ((await page.getByRole('dialog').count()) > 0) {
    await page.keyboard.press('Escape')
    await page.waitForTimeout(50)
  }
}

await clickAndCheck(shortCell, shortProgram.programId, 'bottom')
await clickAndCheck(tenMinuteCell, tenMinuteProgram.programId, 'top')
await clickAndCheck(longCell, longProgram.programId, 'top')

await context.close()
await finish(ng, browser)
