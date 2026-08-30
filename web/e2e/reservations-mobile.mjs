// 予約一覧の行の副情報がモバイル幅で折り返さず、シェブロンに重なる回帰
// （issue #302 のレビュー指摘）の受け入れ判定。jsdom では測れないもの
// （レイアウト・overflow・要素間の重なり）だけをここで見る（e2e/README.md）。
//
// `pages/reservations.test.tsx` の既存テストは「局名の文字列が行の中に居る」ことしか
// 見ておらず、それは jsdom で常に真になる --- 副情報のコンテナが折り返すかどうかは
// 一度も検証されていなかった。
//
// 見るのは:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか
//   ① 360px 幅で、副情報コンテナ（`[data-testid="reservation-secondary"]`）が
//      横方向にオーバーフローしていないこと（`scrollWidth <= clientWidth`）
//   ② 副情報の各子要素の右端が、シェブロン（`[data-testid="reservation-chevron"]`）
//      の左端を超えていないこと --- ①は「コンテナ自身が overflow: visible で
//      中身を外に漏らしていない」ことしか見ないので、コンテナの外形自体が
//      シェブロンへ食い込む場合を捕まえるにはこちらが要る
//   ③ ページ全体が横スクロールしないこと（`documentElement.scrollWidth` が
//      ビューポート幅のまま） --- 重なりは横スクロールを伴わずに起きるので
//      これ単体では①②の代わりにならないが、退行の網としては安く張れる
//
// 局名は実際に重なりが再現した長さのものを使う（レビューの実測と同じ組み合わせ:
// 長い局名 + orphaned バッジ）。
//
// **mirakc も実チューナーも DB も要らない。** `/api/**` を `page.route` で
// ブラウザ側から丸ごと差し替える（e2e/design.mjs と同じ手）。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:reservations-mobile
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { ListReservationsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'

const ng = []

const SITE = 'default'
const HOUR = 3_600_000
const nowMs = Date.now()
const iso = (ms) => new Date(ms).toISOString()

// レビューの実測と同じ組み合わせ: 長い局名 + orphaned（バッジが出る）。
// 3 行目は active（バッジ無し）にして「常時ではない」ことも再現する。
const reservations = [
  {
    id: 1,
    site: SITE,
    programId: 9001,
    source: 'manual',
    state: 'orphaned',
    title: '４Ｋ実況中継スペシャル',
    serviceName: 'ＮＨＫＢＳプレミアム４Ｋ',
    channelType: 'BS',
    startAt: iso(nowMs + HOUR),
    durationMs: HOUR,
    createdAt: iso(nowMs),
    updatedAt: iso(nowMs),
    skip: false,
  },
  {
    id: 2,
    site: SITE,
    programId: 9002,
    source: 'manual',
    state: 'orphaned',
    title: '映画特集',
    serviceName: 'ディズニー・チャンネル',
    channelType: 'CS',
    startAt: iso(nowMs + 2 * HOUR),
    durationMs: HOUR,
    createdAt: iso(nowMs),
    updatedAt: iso(nowMs),
    skip: false,
  },
  {
    id: 3,
    site: SITE,
    programId: 9003,
    source: 'manual',
    state: 'active',
    title: 'ニュース',
    serviceName: 'ＮＨＫ総合１・東京',
    channelType: 'GR',
    startAt: iso(nowMs + 3 * HOUR),
    durationMs: HOUR,
    createdAt: iso(nowMs),
    updatedAt: iso(nowMs),
    skip: false,
  },
]

/** apiHandler は `/api/**` の応答を作る（design.mjs と同じ手）。 */
async function apiHandler({ path: p, json }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: true })
  if (p === '/api/breakers') return json([])
  if (p === '/api/reservations') return json(reservations)
  // 超過区間は空でよい（このスクリプトが見る重なりは orphaned バッジ単独でも
  // 再現する。容量バッジの導線は e2e/badge-links.mjs が別に見ている）。
  if (p === '/api/capacity/overages') return json([])
  return json([])
}

log(`URL: ${URL_BASE}`)

// 契約検証: フィクスチャが orval 生成の zod スキーマと一致するか
// （`validateFixturesOrExit`。design.mjs / e2e/README.md §デザイン 参照）。
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  reservations.map((r, i) => [`reservations[${i}]`, ListReservationsResponseItem, r]),
  ng,
)

// --- ⓪ 配っている bundle が dist/ の現物と一致するか ---
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
// レビューの実測条件（Chromium 360px）に合わせる。
const context = await browser.newContext({
  viewport: { width: 360, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

await page.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })

const rows = page.locator('li:has([data-testid="reservation-secondary"])')
await rows.first().waitFor({ timeout: 15000 }).catch(() => {
  ng.push('行が見つからない（一覧が描画されていない）')
})

const rowCount = await rows.count()
log(`\n=== ①②③ 360px 幅での副情報 overflow / シェブロンとの重なり ===`)
log(`  行数: ${rowCount}`)

for (let i = 0; i < rowCount; i++) {
  const li = rows.nth(i)
  const titleText = await li.locator('.truncate').first().innerText()
  const secondary = li.locator('[data-testid="reservation-secondary"]')
  const chevron = li.locator('[data-testid="reservation-chevron"]')

  const metrics = await secondary.evaluate((el) => ({
    scrollWidth: el.scrollWidth,
    clientWidth: el.clientWidth,
    childRights: Array.from(el.children).map((c) => c.getBoundingClientRect().right),
  }))
  const chevronBox = await chevron.boundingBox()

  const overflow = metrics.scrollWidth - metrics.clientWidth
  const maxChildRight = Math.max(...metrics.childRights, 0)
  const chevronLeft = chevronBox?.x ?? Infinity

  log(
    `  行「${titleText}」: overflow=${overflow}px, 副情報の子の右端最大=${Math.round(maxChildRight)}, シェブロン左端=${Math.round(chevronLeft)}`,
  )

  if (overflow > 0) {
    ng.push(`① 行「${titleText}」の副情報コンテナが横方向に ${overflow}px オーバーフローしている`)
  }
  if (maxChildRight > chevronLeft) {
    ng.push(
      `② 行「${titleText}」の副情報がシェブロンに重なっている（子の右端 ${Math.round(maxChildRight)} > シェブロン左端 ${Math.round(chevronLeft)}）`,
    )
  }
}

// --- ③ ページ全体が横スクロールしない ---
const scrollWidth = await page.evaluate(() => document.documentElement.scrollWidth)
log(`\n  document.documentElement.scrollWidth = ${scrollWidth}px（ビューポート幅 360px）`)
if (scrollWidth > 360) {
  ng.push(`③ ページ全体が横スクロールする（scrollWidth=${scrollWidth}px）`)
}

await finish(ng, browser)
