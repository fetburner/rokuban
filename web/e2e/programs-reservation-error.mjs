// `GET /api/reservations` 失敗時の番組表（`/programs`）の受け入れ判定
// （PR #716 レビュー指摘 2 / 1）。
//
// `pages/programs.tsx` のグリッドは
// `height: calc(100dvh - var(--page-header-height, 0px) - var(--sticky-banners-height, 0px))`
// で高さ予算を決める。`reservations.isPending` / `isError` のバナーを
// `PageHeader` の外（通常フローの兄弟）に置くと、バナーの高さがどちらの CSS
// 変数にも入らず、**100dvh で組んだ画面なのに文書がビューポートを超えて
// ページ全体がスクロールする**（実測: 外に置くと 1440x900 で 949px / 900px、
// `PageHeader` の中なら 900px / 900px）。jsdom はレイアウトを計算しない
// （`getBoundingClientRect()` が常に 0 を返す）ので、この壊れ方はユニット
// テストでは原理的に検出できない（web/e2e/README.md「jsdom が測れないもの」）。
//
// なお `pages/programs.tsx` のコメントが挙げる「グリッドの sticky ヘッダが
// 画面外へ出る」という症状そのものは、**この構造では再現しなかった** ---
// 外に置いた状態で文書を最後までスクロールしても、グリッド内の
// `GenreLegend`（見出し行より上にある帯）が緩衝になり 39px の余裕が残った。
// 判定 B はその症状の再現ではなく、緩衝が無くなったときに気付くための網。
//
// 見るのは:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか
//   デスクトップ（1440x900、`lg` 以上）で `/programs?view=grid` を開く:
//     A. 文書がビューポートをはみ出さない（`document.documentElement.scrollHeight`
//        が `window.innerHeight` を超えない。1〜2px は許容）
//     B. グリッドのサービス列見出し（`program-grid-header-cell`）の上端が
//        `PageHeader`（`<header>`）の下端より下にあり、ビューポート内に見えている
//     C. グリッドのセルを選ぶと出る選択済み番組の行の「予約」ボタンが disabled
//        （予約状態が不明なまま record intent を送らないことの実ブラウザ確認。
//        表示形式ごとに boolean prop を渡していた実装ではグリッドだけが
//        渡し忘れており、実際に `PUT .../intent` が飛ぶことを測った ---
//        いまは `ReservationActions` 経由なので渡し忘れは起きないが、
//        リストと同じ経路に留まっていることをここで見る）
//   モバイル（390x844）で `/programs`（リスト）を開く:
//     D. 失敗バナーが sticky ヘッダに入った代償として `<header>` の
//        `offsetHeight` を測る（合否ではなく測定値。念のため viewport 高さの
//        半分未満という緩い上限だけ置く）
//     E. その状態でも番組リストの最初の行がヘッダの下に見えている
//
// **mirakc も実チューナーも DB も要らない。** `/api/**` を `page.route` で
// ブラウザ側から丸ごと差し替える（design.mjs / grid-reserved.mjs と同じ手）。
// `/api/reservations` だけを毎回 500 で返す（react-query のリトライも含めて
// 常に失敗のまま）。
//
//   cd web && pnpm build && go build -o /tmp/rokuban ./cmd/rokuban
//   /tmp/rokuban server --roles api --config dev.local.yml
//   pnpm e2e:programs-reservation-error
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

const BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'

const ng = []

const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

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

const program1 = {
  programId: 716001,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 1,
  startAt: iso(nowMs + 10 * 60_000),
  endAt: iso(nowMs + 40 * 60_000),
  durationMs: 30 * 60_000,
  name: '予約状態不明ニュース',
  description: '',
  genres: [0],
  isFree: true,
}

const program2 = {
  programId: 716002,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 2,
  startAt: iso(nowMs + 40 * 60_000),
  endAt: iso(nowMs + 70 * 60_000),
  durationMs: 30 * 60_000,
  name: '予約状態不明バラエティ',
  description: '',
  genres: [0],
  isFree: true,
}

/** apiHandler は /programs の描画に要る `/api/**` の応答を作る。 */
async function apiHandler({ path: p, json, route }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === '/api/encode-profiles') return json([])
  if (p === '/api/capacity/overages') return json([])
  // ここが本題: 予約一覧は毎回失敗させる（react-query のリトライぶんも含めて
  // 常に 500）。他の /api/** は成功させ、番組・チャンネルの取得自体は
  // 妨げない（`programs.tsx` は予約一覧の失敗で番組まで隠さない設計）。
  if (p === '/api/reservations') {
    return route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'reservations unavailable' }),
    })
  }
  if (p === `/api/sites/${SITE}/services`) return json([service])
  if (p === `/api/sites/${SITE}/programs`) return json([program1, program2])
  if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
  if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
  return json([])
}

log(`URL      : ${BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// 契約検証: フィクスチャが orval 生成の zod スキーマと一致するか
// （`validateFixturesOrExit`。/api/reservations は常に失敗させるので検証対象に
// 含めない --- 契約に沿った成功形を返す気が無い応答だから）。
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['service', ListServicesResponseItem, service],
    ['program1', ListProgramsResponseItem, program1],
    ['program2', ListProgramsResponseItem, program2],
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(BASE, ng)

const browser = await launchBrowser()

// === デスクトップ（lg 以上）: /programs?view=grid ===
log('\n=== デスクトップ（1440x900、view=grid） ===')
const desktopContext = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const desktopPage = await desktopContext.newPage()
await desktopPage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(desktopPage, apiHandler)
await desktopPage.goto(BASE + '/programs?view=grid', { waitUntil: 'domcontentloaded' })

// 失敗バナーとグリッドの両方が出るまで待つ。
await desktopPage.getByText('予約状態の取得に失敗しました').waitFor({ timeout: 15000 })
const grid = desktopPage.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 })
// ResizeObserver が --page-header-height を publish し終わるのを待つ。
await desktopPage.waitForTimeout(300)

// --- A. 文書がビューポートをはみ出さない ---
log('\n=== A. 文書がビューポートをはみ出さない ===')
const docMetrics = await desktopPage.evaluate(() => ({
  scrollHeight: document.documentElement.scrollHeight,
  innerHeight: window.innerHeight,
}))
log(`  document.documentElement.scrollHeight: ${docMetrics.scrollHeight}px`)
log(`  window.innerHeight                   : ${docMetrics.innerHeight}px`)
if (docMetrics.scrollHeight > docMetrics.innerHeight + 2) {
  ng.push(
    `A 文書の高さ（${docMetrics.scrollHeight}px）がビューポート（${docMetrics.innerHeight}px）を ${(docMetrics.scrollHeight - docMetrics.innerHeight).toFixed(1)}px 超えている --- ページ全体がスクロールする状態になっている`,
  )
}

// --- B. グリッドの sticky ヘッダ（サービス列見出し）が隠れない ---
// 文書がはみ出していれば（A）、その最悪ケースは outer document を最後まで
// スクロールした状態 --- ここでグリッド自身のブロックが最も上に押し上げられる。
log('\n=== B. グリッドの sticky ヘッダが隠れない（文書を最後までスクロールした状態） ===')
await desktopPage.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight))
await desktopPage.waitForTimeout(200)
const headerBottom = await desktopPage
  .locator('header')
  .first()
  .evaluate((el) => el.getBoundingClientRect().bottom)
const gridHeaderCell = desktopPage.getByTestId('program-grid-header-cell').first()
await gridHeaderCell.waitFor({ timeout: 10000 })
const cellRect = await gridHeaderCell.evaluate((el) => {
  const r = el.getBoundingClientRect()
  return { top: r.top, bottom: r.bottom, height: r.height }
})
const scrolledY = await desktopPage.evaluate(() => window.scrollY)
log(`  document 最大スクロール後の scrollY: ${scrolledY}px`)
log(`  PageHeader の下端        : ${headerBottom}px`)
log(`  サービス列見出しの上端   : ${cellRect.top}px（高さ ${cellRect.height}px）`)
if (cellRect.top < headerBottom - 1) {
  ng.push(
    `B グリッドのサービス列見出し（上端 ${cellRect.top}px）が PageHeader の下端（${headerBottom}px）より上にある --- PageHeader の下に隠れている`,
  )
}
if (cellRect.top < 0 || cellRect.top >= docMetrics.innerHeight || cellRect.height <= 0) {
  ng.push(
    `B グリッドのサービス列見出しがビューポート内に見えていない（top=${cellRect.top}px, height=${cellRect.height}px, viewport高さ=${docMetrics.innerHeight}px）`,
  )
}

// --- C. グリッドでも予約が押せない（reservationStateUnknown のまま） ---
log('\n=== C. グリッドの選択行の「予約」ボタンが disabled ===')
await desktopPage.locator('[data-testid="program-grid-cell"]').first().click()
const reserveButton = desktopPage.getByTestId('program-row-reserve').getByRole('button')
await reserveButton.waitFor({ timeout: 10000 })
const reserveLabel = (await reserveButton.textContent())?.trim()
const reserveDisabled = await reserveButton.isDisabled()
log(`  ボタンの文言: 「${reserveLabel}」 disabled=${reserveDisabled}`)
if (reserveLabel !== '予約') {
  ng.push(`C 選択行のボタンが「予約」ではない（「${reserveLabel}」） --- 選んだセルが未予約でない可能性`)
}
if (!reserveDisabled) {
  ng.push('C 予約状態が不明（/api/reservations 失敗中）なのに「予約」ボタンが押せる状態になっている')
}

await desktopContext.close()

// === モバイル: /programs（リスト） ===
log('\n=== モバイル（390x844、リスト） ===')
const mobileContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const mobilePage = await mobileContext.newPage()
await mobilePage.clock.setFixedTime(FIXED_NOW)
await installApiStubs(mobilePage, apiHandler)
await mobilePage.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })
await mobilePage.getByText('予約状態の取得に失敗しました').waitFor({ timeout: 15000 })
const firstRow = mobilePage.locator('li[data-program-id]').first()
await firstRow.waitFor({ timeout: 15000 })
await mobilePage.waitForTimeout(300)

// --- D. 失敗バナーを飲み込んだ header の実測高さ（合否ではなく測定値） ---
log('\n=== D. header の実測高さ（測定値） ===')
const headerHeight = await mobilePage
  .locator('header')
  .first()
  .evaluate((el) => el.offsetHeight)
log(`  <header>.offsetHeight: ${headerHeight}px（viewport 844px）`)
// 緩い上限のみ（合否の主目的ではない --- sticky に入れた代償の定量化）。
if (headerHeight >= 844 * 0.5) {
  ng.push(`D header の実測高さ（${headerHeight}px）が viewport 高さの半分以上になっている`)
}

// --- E. その状態でも番組リストの先頭行がヘッダの下に見えている ---
log('\n=== E. リストの先頭行がヘッダの下に見えている ===')
const mobileHeaderRect = await mobilePage
  .locator('header')
  .first()
  .evaluate((el) => el.getBoundingClientRect())
const rowRect = await firstRow.evaluate((el) => el.getBoundingClientRect())
log(`  header の下端: ${mobileHeaderRect.bottom}px`)
log(`  先頭行        : top=${rowRect.top}px bottom=${rowRect.bottom}px`)
if (rowRect.top < mobileHeaderRect.bottom - 1) {
  ng.push(
    `E 先頭行の上端（${rowRect.top}px）が header の下端（${mobileHeaderRect.bottom}px）より上にある --- 隠れている`,
  )
}
if (rowRect.bottom <= mobileHeaderRect.bottom || rowRect.top >= 844) {
  ng.push(`E 先頭行がビューポート内に見えていない（top=${rowRect.top}px, bottom=${rowRect.bottom}px）`)
}

await mobileContext.close()

await finish(ng, browser)
