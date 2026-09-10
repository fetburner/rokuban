// 番組表のセル選択後の操作をモーダルへ寄せる受け入れ判定（issue #723）。
//
// jsdom はレイアウトとフォーカスの実挙動を測れないため、単体テストでは
// 「モーダル内に予約ボタンがある」までを確認し、ここでは実ブラウザで次を測る:
//   - セルをクリックすると番組名でラベル付けされたダイアログが開く
//   - hover なしでモーダル内の予約ボタンが可視・操作可能で、1 回のクリックで予約できる
//   - Escape / overlay クリックで閉じ、クリック元セルへフォーカスが戻る
//
// API は `page.route` で差し替える。mirakc・実チューナー・DB は要らない。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-dialog
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

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const PROGRAM_ID = 723001
const FIXED_NOW = new Date('2026-08-13T00:00:00.000Z')

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
  programId: PROGRAM_ID,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 1,
  startAt: '2026-08-13T01:00:00.000Z',
  endAt: '2026-08-13T02:00:00.000Z',
  durationMs: 3_600_000,
  name: 'モーダル予約確認番組',
  // モーダル内でスクロールが発生するくらい長くする（閉じるボタンが
  // スクロールで画面外へ出ないことの確認に使う。レビュー指摘）。
  description: 'モーダルから予約できることを確認する番組。'.repeat(300),
  genres: [0],
  isFree: true,
}

const ng = []
let intentPutCount = 0

/** apiHandler は番組表モーダルの描画と予約操作に必要な応答を作る。 */
async function apiHandler({ path: p, json, route }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === '/api/reservations') return json([])
  if (p === '/api/capacity/overages') return json([])
  if (p === '/api/encode-profiles') return json([])
  if (p === `/api/sites/${SITE}/services`) return json([service])
  if (p === `/api/sites/${SITE}/programs`) return json([program])
  if (p === `/api/sites/${SITE}/programs/${PROGRAM_ID}/intent` && route.request().method() === 'PUT') {
    intentPutCount++
    return route.fulfill({ status: 204 })
  }
  if (p === `/api/sites/${SITE}/programs/${PROGRAM_ID}`) {
    return json({ extended: {}, audios: [] })
  }
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  return json([])
}

/** isFocusedCell は閉じたダイアログの復帰先が選択セルかを実ブラウザで確認する。 */
async function isFocusedCell(page) {
  return page.evaluate((programId) => {
    const active = document.activeElement
    return (
      active instanceof HTMLElement &&
      active.matches('[data-testid="program-grid-cell"]') &&
      active.getAttribute('data-program-id') === String(programId)
    )
  }, PROGRAM_ID)
}

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ['program', ListProgramsResponseItem, program],
  ],
  ng,
)

// ⓪ 配っている bundle が dist/ の現物と一致するか（e2e/lib.mjs 参照）。
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page, apiHandler)

await page.goto(`${BASE}/programs?view=grid`, { waitUntil: 'domcontentloaded' })
const grid = page.locator('[data-testid="program-grid"]')
await grid.waitFor({ timeout: 15000 })
const cell = page.locator(
  `[data-testid="program-grid-cell"][data-program-id="${PROGRAM_ID}"]`,
)
await cell.waitFor({ timeout: 15000 })

log('\n=== セル選択でモーダルを開く ===')
await cell.click()
const dialog = page.getByRole('dialog', { name: program.name })
await dialog.waitFor({ timeout: 15000 })
const labelledBy = await dialog.getAttribute('aria-labelledby')
if (!labelledBy) ng.push('aria-labelledby がダイアログに設定されていない')
if (labelledBy && (await page.locator(`#${labelledBy}`).textContent()) !== program.name) {
  ng.push('aria-labelledby が番組名の Dialog.Title を指していない')
}

const reserveButton = dialog.getByRole('button', { name: '予約', exact: true })
const reserveBox = await reserveButton.boundingBox()
log(`  モーダル内の予約ボタン: ${reserveBox ? `${reserveBox.width}x${reserveBox.height}px` : '見つからない'}`)
if (!(await reserveButton.isVisible()) || !reserveBox || reserveBox.width <= 0 || reserveBox.height <= 0) {
  ng.push('モーダル内の予約ボタンが hover なしで可視・操作可能になっていない')
}
if ((await cell.getAttribute('aria-pressed')) !== 'true') {
  ng.push('モーダル表示中も選択セルのハイライトが維持されていない')
}

log('\n=== 長い番組概要でスクロールしても閉じるボタンが画面外へ出ない ===')
const closeButton = dialog.getByRole('button', { name: '閉じる', exact: true })
const dialogBody = page.locator('[data-testid="program-dialog-body"]')
const scrollTopBefore = await dialogBody.evaluate((el) => el.scrollTop)
await dialogBody.evaluate((el) => {
  el.scrollTop = el.scrollHeight
})
const scrolled = await dialogBody.evaluate((el) => el.scrollTop > 0)
if (!scrolled) ng.push('モーダル本文がスクロールしていない（テスト前提が崩れている）')
// `boundingBox()`/`isVisible()` は overflow で clip されているかを見ない
// （clip されていても要素自体の矩形は正の幅高さのまま返る）。閉じるボタンが
// ダイアログの可視領域の外へ出ていないかは、ダイアログ自身の矩形に完全に
// 収まっているかで判定する。
const dialogBoxAfterScroll = await dialog.boundingBox()
const closeBoxAfterScroll = await closeButton.boundingBox()
const closeButtonWithinDialog =
  dialogBoxAfterScroll &&
  closeBoxAfterScroll &&
  closeBoxAfterScroll.x >= dialogBoxAfterScroll.x &&
  closeBoxAfterScroll.y >= dialogBoxAfterScroll.y &&
  closeBoxAfterScroll.x + closeBoxAfterScroll.width <= dialogBoxAfterScroll.x + dialogBoxAfterScroll.width &&
  closeBoxAfterScroll.y + closeBoxAfterScroll.height <= dialogBoxAfterScroll.y + dialogBoxAfterScroll.height
if (
  !(await closeButton.isVisible()) ||
  !closeBoxAfterScroll ||
  closeBoxAfterScroll.width <= 0 ||
  closeBoxAfterScroll.height <= 0 ||
  !closeButtonWithinDialog
) {
  ng.push('本文をスクロールすると閉じるボタンがダイアログの外へ出る')
}
await dialogBody.evaluate((el, top) => {
  el.scrollTop = top
}, scrollTopBefore)

log('\n=== Tab 走査をモーダル内に閉じ込める ===')
for (let i = 0; i < 8; i++) {
  await page.keyboard.press('Tab')
  // Base UI はフォーカスガードから requestAnimationFrame で実要素へ戻すため、
  // key press 直後の一瞬だけ document 外側の guard が activeElement になる。
  const focusStayedInside = await page
    .waitForFunction(
      () => {
        const active = document.activeElement
        return active instanceof HTMLElement && active.closest('[role="dialog"]') !== null
      },
      undefined,
      { timeout: 1000 },
    )
    .then(() => true)
    .catch(() => false)
  if (!focusStayedInside) {
    ng.push('Tab 走査中にモーダルの外へフォーカスが移動した')
    break
  }
}

// 予約一覧の初回取得が終わるまでは ProgramRow の安全ガードで disabled になる。
// 「表示されている」だけでクリックすると、このガードを待たずに空虚な成功になる。
await page.waitForFunction(
  () => {
    const button = document.querySelector(
      '[data-testid="program-dialog"] [data-testid="program-row-reserve"] button',
    )
    return button instanceof HTMLButtonElement && !button.disabled
  },
  undefined,
  { timeout: 15000 },
)

const intentResponse = page.waitForResponse(
  (response) =>
    response.url().includes(`/api/sites/${SITE}/programs/${PROGRAM_ID}/intent`) &&
    response.request().method() === 'PUT',
)
await reserveButton.click()
await intentResponse
log(`  予約 PUT: ${intentPutCount} 回`)
if (intentPutCount !== 1) ng.push(`予約 PUT が 1 回ではない（${intentPutCount} 回）`)

log('\n=== Escape で閉じてセルへフォーカス復帰 ===')
await page.keyboard.press('Escape')
await dialog.waitFor({ state: 'detached', timeout: 15000 })
if (!(await isFocusedCell(page))) ng.push('Escape 後にクリック元セルへフォーカスが戻らない')
if ((await cell.getAttribute('aria-pressed')) !== 'false') ng.push('Escape 後にセルの選択が解除されない')

log('\n=== 閉じるボタンで閉じてセルへフォーカス復帰 ===')
await cell.click()
await dialog.waitFor({ timeout: 15000 })
await dialog.getByRole('button', { name: '閉じる', exact: true }).click()
await dialog.waitFor({ state: 'detached', timeout: 15000 })
if (!(await isFocusedCell(page))) ng.push('閉じるボタンの後にクリック元セルへフォーカスが戻らない')

log('\n=== overlay クリックで閉じてセルへフォーカス復帰 ===')
await cell.click()
await dialog.waitFor({ timeout: 15000 })
const overlay = page.locator('[data-slot="dialog-overlay"]')
await overlay.click({ position: { x: 8, y: 8 } })
await dialog.waitFor({ state: 'detached', timeout: 15000 })
if (!(await isFocusedCell(page))) ng.push('overlay クリック後にクリック元セルへフォーカスが戻らない')
if ((await cell.getAttribute('aria-pressed')) !== 'false') {
  ng.push('overlay クリック後にセルの選択が解除されない')
}

await context.close()
await finish(ng, browser)
