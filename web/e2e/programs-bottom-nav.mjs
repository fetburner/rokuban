// モバイルの番組リスト末行がボトムタブに隠れる問題（issue #303）の受け入れ判定。
//
// `main` の `padding-bottom`（`--bottom-nav-height`）はドキュメント最下端まで
// スクロールしたときにしか重なりを防げない --- 番組表は 1 時間窓ぶんの番組が
// 初回表示（スクロール前）で既にビューポートより長いことがあり、その場合
// たまたま行の境界がボトムタブの上端とずれた位置に来ると、時刻や「予約」
// ボタンを含む行がタブの裏に半分だけ隠れる。jsdom は `getBoundingClientRect`
// を計算しないため（常に 0 を返す）、この重なりは原理的にユニットテストでは
// 検出できない（e2e/README.md）。
//
// 見るのは:
//   ⓪ 配っている bundle が dist/ の現物と一致するか
//   ① 390×844 で `/programs` を開き、**スクロールせずに**見える範囲で、
//      行がボトムタブに一部だけ隠れていない（上端は見えるが下端がタブに
//      食い込んでいる行が無い）こと
//   ② ドキュメント最下端まで実際にスクロールしても、①と同じ意味で重ならない
//      こと（`main` の `padding-bottom` が本来保証する状態が壊れていないことの
//      回帰確認）
//   ③ `lg` 以上（ボトムタブが無い画面幅）では、この補正が何も動かさない
//      （スクロール位置が 0 のまま）こと --- 補正の実装ミスでデスクトップにも
//      効いてしまう退行を防ぐ
//
// **mirakc も実チューナーも DB も要らない。** `/api/**` を `page.route` で
// ブラウザ側から丸ごと差し替える（e2e/design.mjs と同じ手）。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:programs-bottom-nav
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'

const ng = []
const log = (...a) => console.log(...a)

/** verifyBundleMatches は `e2e/badge-links.mjs` と同じ手（詳細はそちらのコメント参照）。 */
function verifyBundleMatches(servedHtml) {
  const servedMatch = /assets\/(index-[^"]+\.js)/.exec(servedHtml)
  const served = servedMatch?.[1]
  const distDir = path.join(process.cwd(), 'dist', 'assets')
  let local
  try {
    local = readdirSync(distDir).find((f) => /^index-.*\.js$/.test(f))
  } catch {
    local = undefined
  }
  return { served, local, matches: served !== undefined && served === local }
}

const SITE = 'default'
const HOUR = 3_600_000
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const service = {
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

/**
 * programsFor は要求された窓を 30 分番組で埋める。1 時間窓（`pages/programs.tsx`
 * の `windowHours`）だけで既に 390×844 のビューポートより長くなる密度にして
 * ある --- 密度が薄いと初回表示で 1 画面に収まってしまい、①の再現条件
 * （スクロール前から重なりが起きる）が成立しない。
 */
function programsFor(startISO, endISO) {
  const start = Date.parse(startISO)
  const end = Date.parse(endISO)
  const out = []
  const slot = 1800_000
  let t = Math.floor(start / slot) * slot - slot
  let i = 0
  while (t < end) {
    out.push({
      programId: Math.floor(t / 1000) * 100 + (service.serviceId % 100),
      networkId: service.networkId,
      serviceId: service.serviceId,
      eventId: i + 1,
      startAt: iso(t),
      endAt: iso(t + slot),
      durationMs: slot,
      name: `番組${i}`,
      description: '',
      genres: [0],
      isFree: true,
    })
    t += slot
    i++
  }
  return out
}

/** installApiStubs は `/api/**` をすべてブラウザ側で差し替える（design.mjs と同じ手）。 */
async function installApiStubs(page) {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/reservations') return json([])
    if (p === '/api/capacity/overages') return json([])
    if (p === `/api/sites/${SITE}/services`) return json([service])
    if (p === `/api/sites/${SITE}/programs`) {
      return json(
        programsFor(
          url.searchParams.get('start') ?? iso(nowMs),
          url.searchParams.get('end') ?? iso(nowMs + 6 * HOUR),
        ),
      )
    }
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    return json([])
  })
}

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- ⓪ 配っている bundle が dist/ の現物と一致するか ---
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
const rootHtml = await fetch(URL_BASE + '/').then((r) => r.text())
const bundleCheck = verifyBundleMatches(rootHtml)
log(`  配っている bundle: ${bundleCheck.served ?? '(取得できない)'}`)
log(`  dist/assets/     : ${bundleCheck.local ?? '(見つからない。web/ で実行しているか確認)'}`)
if (!bundleCheck.matches) {
  ng.push(
    `⓪ ${URL_BASE} が配っている bundle（${bundleCheck.served ?? '不明'}）が dist/assets/ の現物（${bundleCheck.local ?? '不明'}）と一致しない --- 別プロセス・古いビルドを測っている可能性が高いので、これ以降の判定を打ち切る`,
  )
  log('\n=== 結果 ===')
  ng.forEach((f) => log('  NG: ' + f))
  process.exit(1)
}
log('  一致（このサーバーは自分のビルドを配っている）')

const browser = await chromium.launch()

/**
 * findOverlappingRow は、現在描かれている行のうち「上端は見えているが下端が
 * ボトムタブに食い込んでいる」行を探す（`lib/tab-clearance.ts` の
 * `computeInitialTabClearanceScroll` と同じ判定条件）。無ければ `null`。
 */
async function findOverlappingRow(page) {
  return page.evaluate(() => {
    const navRect = document
      .querySelector('[data-testid="bottom-nav"]')
      ?.getBoundingClientRect()
    if (!navRect || navRect.height <= 0) return { navPresent: false, overlap: null }
    const rows = [...document.querySelectorAll('li[data-program-id]')]
    for (const row of rows) {
      const r = row.getBoundingClientRect()
      if (r.top < navRect.top && r.bottom > navRect.top) {
        return {
          navPresent: true,
          overlap: {
            text: row.innerText.replace(/\s+/g, ' ').slice(0, 80),
            top: r.top,
            bottom: r.bottom,
            navTop: navRect.top,
          },
        }
      }
    }
    return { navPresent: true, overlap: null }
  })
}

// --- ① 390×844、スクロールせずに見た状態で重なりが無い ---
log('\n=== ① 390×844・初回表示（スクロールなし）でボトムタブに隠れる行が無い ===')
{
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
  await page.locator('li[data-program-id]').first().waitFor({ timeout: 15000 })
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(500)

  const scrollY = await page.evaluate(() => window.scrollY)
  log(`  scrollY（補正後）: ${scrollY}`)

  const result = await findOverlappingRow(page)
  if (!result.navPresent) {
    ng.push('① ボトムタブ（[data-testid="bottom-nav"]）が見つからない・高さ 0')
  } else if (result.overlap) {
    ng.push(
      `① 初回表示で行がボトムタブに食い込んでいる: 「${result.overlap.text}」` +
        `（top=${result.overlap.top.toFixed(1)}, bottom=${result.overlap.bottom.toFixed(1)}, タブ上端=${result.overlap.navTop.toFixed(1)}）`,
    )
  } else {
    log('  食い込んでいる行は無い（期待どおり）')
  }
  await context.close()
}

// --- ② ドキュメント最下端まで実際にスクロールしても重ならない（回帰確認） ---
log('\n=== ② 最下端までスクロールしても重ならない（`main` の padding-bottom の回帰確認） ===')
{
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
  await page.locator('li[data-program-id]').first().waitFor({ timeout: 15000 })
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(500)

  // 8 日ぶんのローリングウィンドウ全体を読み終えるまで、成長が止まるまで
  // 繰り返しスクロールする（`pages/programs.tsx` の `selectableDays`）。
  let prevHeight = -1
  let stableCount = 0
  for (let i = 0; i < 300; i++) {
    const h = await page.evaluate(() => {
      window.scrollTo(0, document.documentElement.scrollHeight)
      return document.documentElement.scrollHeight
    })
    await page.waitForTimeout(150)
    if (h === prevHeight) {
      stableCount++
      if (stableCount >= 8) break
    } else {
      stableCount = 0
    }
    prevHeight = h
  }
  await page.waitForTimeout(300)

  const result = await findOverlappingRow(page)
  if (!result.navPresent) {
    ng.push('② ボトムタブが見つからない・高さ 0')
  } else if (result.overlap) {
    ng.push(
      `② 最下端までスクロールしても行がボトムタブに食い込んでいる: 「${result.overlap.text}」` +
        `（top=${result.overlap.top.toFixed(1)}, bottom=${result.overlap.bottom.toFixed(1)}, タブ上端=${result.overlap.navTop.toFixed(1)}）`,
    )
  } else {
    log('  食い込んでいる行は無い（期待どおり）')
  }
  await context.close()
}

// --- ③ lg 以上（ボトムタブが無い）ではスクロール位置を動かさない ---
log('\n=== ③ 1280px（ボトムタブが無い）では補正が何もしない ===')
{
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(URL_BASE + '/programs', { waitUntil: 'domcontentloaded' })
  await page.locator('li[data-program-id]').first().waitFor({ timeout: 15000 })
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(500)

  const scrollY = await page.evaluate(() => window.scrollY)
  log(`  scrollY: ${scrollY}`)
  if (scrollY !== 0) {
    ng.push(`③ 1280px でも scrollY が 0 のままであるべきだが ${scrollY} だった（補正が誤って動いている）`)
  } else {
    log('  scrollY は 0 のまま（期待どおり）')
  }
  await context.close()
}

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
