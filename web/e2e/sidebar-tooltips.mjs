// 畳んだサイドバーの項目名が、ポインターとキーボードフォーカスで視覚的に出ることを
// 実 Chromium で確認する。jsdom では Tooltip の実ブラウザ入力と遅延を測れない。
// 展開中には出ないこと、sr-only のリンク名が維持され説明として二重に結び付かないことも見る。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:sidebar-tooltips
import { finish, installApiStubs, launchBrowser, log, sseKeepAlive, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []

log(`URL: ${URL_BASE}`)
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await page.addInitScript(() => localStorage.setItem('rokuban:sidebar:collapsed', '1'))
await installApiStubs(page, async ({ path, json, route }) => {
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  return json([])
})

async function openRecordings() {
  await page.goto(`${URL_BASE}/recordings`, { waitUntil: 'domcontentloaded' })
  const sidebar = page.getByRole('navigation', { name: '主ナビゲーション' })
  await sidebar.waitFor()
  await sidebar.getByRole('link', { name: 'ライブ', exact: true }).waitFor()
  return sidebar
}

function tooltip(label) {
  return page.getByRole('tooltip', { name: label, exact: true })
}

async function anyTooltipVisible() {
  return (await page.locator('[role="tooltip"]:visible').count()) > 0
}

async function tooltipVisible(label, timeout) {
  const popup = tooltip(label)
  await popup.waitFor({ state: 'visible', timeout }).catch(() => {})
  return (await popup.count()) > 0 && await popup.isVisible().catch(() => false)
}

const sidebar = await openRecordings()
const toggle = sidebar.getByRole('button', { name: /ナビゲーションを/ })
const home = sidebar.getByRole('link', { name: 'ホーム', exact: true })
const programs = sidebar.getByRole('link', { name: '番組', exact: true })

log('\n=== ① 1280px / 折りたたみ: ホバーは300ms後、隣の項目はすぐに出る ===')
if (await toggle.getAttribute('aria-expanded') !== 'false') {
  ng.push('① サイドバーが畳まれた状態で始まらない')
}
for (const label of ['ホーム', '番組', '録画', '予約', 'ライブ', '検索', 'ルール', 'CM 検出のロゴ']) {
  const link = sidebar.getByRole('link', { name: label, exact: true })
  if ((await link.count()) !== 1) ng.push(`① ${label}: 読み上げ名が一意でない`)
  if (await link.getAttribute('title') !== null) ng.push(`① ${label}: ネイティブ title が残っている`)
  if (await link.getAttribute('aria-describedby') !== null) ng.push(`① ${label}: tooltip が説明として結び付いている`)
}

await page.mouse.move(0, 0)
await home.evaluate((element) => {
  window.__sidebarTooltipHoverAt = null
  element.addEventListener(
    'mouseenter',
    () => { window.__sidebarTooltipHoverAt = performance.now() },
    { once: true },
  )
})
await home.hover()
await page.waitForTimeout(200)
const homeTooltipOpenedEarly = await tooltip('ホーム').isVisible().catch(() => false)
if (homeTooltipOpenedEarly) {
  ng.push('① ホーム: 300ms より前に tooltip が出る')
}
const homeTooltipAppeared = await tooltipVisible('ホーム', 500)
if (!homeTooltipAppeared) {
  ng.push('① ホーム: ホバーで role=tooltip が出ない')
} else if (homeTooltipOpenedEarly) {
  log('  ホーム: 200ms 以内に role=tooltip が表示')
} else {
  const measuredDelay = await page.evaluate(() => {
    const startedAt = window.__sidebarTooltipHoverAt
    return typeof startedAt === 'number' ? performance.now() - startedAt : null
  })
  if (measuredDelay === null || measuredDelay < 250 || measuredDelay > 500) {
    ng.push(`① ホーム: 表示までが300ms付近でない (実測 ${measuredDelay === null ? '不明' : `${Math.round(measuredDelay)}ms`})`)
  }
  log(`  ホーム: 200ms では非表示、表示まで実測 ${measuredDelay === null ? '不明' : `${Math.round(measuredDelay)}ms`}`)
}

await programs.hover()
if (!await tooltipVisible('番組', 180)) {
  ng.push('① 番組: 直前の tooltip を見た後、隣接項目へ移ってもすぐに出ない')
} else {
  log('  番組: 隣へ移動すると180ms以内に表示')
}

log('\n=== ② 折りたたみ: キーボードフォーカスで表示し、名前を二重に読ませない ===')
await page.mouse.move(0, 0)
await page.reload({ waitUntil: 'domcontentloaded' })
const keyboardSidebar = page.getByRole('navigation', { name: '主ナビゲーション' })
await keyboardSidebar.getByRole('link', { name: 'ライブ', exact: true }).waitFor()
const skipLink = page.getByRole('link', { name: '本文へ移動', exact: true })
await skipLink.focus()
await page.keyboard.press('Tab') // Sidebar toggle
await page.keyboard.press('Tab') // First sidebar item
const keyboardHome = keyboardSidebar.getByRole('link', { name: 'ホーム', exact: true })
if (!await keyboardHome.evaluate((element) => document.activeElement === element)) {
  ng.push('② Tab 移動でホームにフォーカスが当たらない')
}
if (!await tooltipVisible('ホーム', 300)) {
  ng.push('② ホーム: キーボードフォーカスで tooltip が出ない')
}
const nameCount = await keyboardSidebar.getByRole('link', { name: 'ホーム', exact: true }).count()
const describedBy = await keyboardHome.getAttribute('aria-describedby')
const title = await keyboardHome.getAttribute('title')
if (nameCount !== 1 || describedBy || title) {
  ng.push(`② ホーム: 名前/説明が二重 (exact-name links=${nameCount}, aria-describedby=${describedBy}, title=${title})`)
} else {
  log('  accessible name は「ホーム」1つ。aria-describedby と title は無し')
}

log('\n=== ③ 1280px / 展開: ホバー・フォーカスとも tooltip を出さない ===')
await toggle.click()
if (await toggle.getAttribute('aria-expanded') !== 'true') {
  ng.push('③ トグルでサイドバーを開けない')
}
await page.getByRole('tooltip').waitFor({ state: 'hidden', timeout: 1000 }).catch(() => {})
const openHome = keyboardSidebar.getByRole('link', { name: 'ホーム', exact: true })
await openHome.hover()
await page.waitForTimeout(400)
if (await anyTooltipVisible()) {
  ng.push('③ 展開中のホームをホバーすると tooltip が出る')
}
await page.mouse.move(0, 0)
await page.waitForTimeout(600) // hover 後の tooltip 状態が落ち着くのを待つ
await toggle.focus()
await page.keyboard.press('Tab') // 展開中の先頭項目（ホーム）
if (!await openHome.evaluate((element) => document.activeElement === element)) {
  ng.push('③ 展開中、Tab 移動でホームにフォーカスが当たらない')
}
await page.waitForTimeout(400)
if (await anyTooltipVisible()) {
  ng.push('③ 展開中のホームにフォーカスすると tooltip が出る')
} else {
  log('  展開中はホバー・フォーカスとも tooltip なし')
}

await context.close()
await finish(ng, browser)
