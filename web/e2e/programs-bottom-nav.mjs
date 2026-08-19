// モバイルの番組リスト末行がボトムタブに隠れる問題（issue #303）の受け入れ判定。
//
// **経緯**: 最初の実装は「初回表示（scrollY=0）に限って重なりを検出したら
// `window.scrollBy` で押し出す」方式だった。しかしページ全体スクロール +
// `fixed` なタブという組み合わせでは、リストの先頭行は常にその日付ヘッダ
// （`sticky`）の直下に隙間なく続くため、末尾側の重なりを消すのに必要な分だけ
// スクロールすると、**その量とちょうど同じだけ先頭行が日付ヘッダの裏へ食い込む**
// （実機計測で確認: 末尾の重なり 29px を消す補正が先頭行に同じ 29px の食い込みを
// 新たに作った）。単一のスクロール位置で両端の重なりを同時に消すことはできないので、
// この方式は削除した（詳細は `docs/frontend/scroll.md`「ボトムタブの裏に隠れる行」）。
//
// 現在ここで見ているのは、実際に直した原因 --- `--bottom-nav-height`
// （`web/src/index.css`）がボトムタブの実際の描画高さ（`border-t` 1px を含む）と
// 食い違っていたため、`main` の `padding-bottom` がドキュメント最下端まで
// スクロールしてもタブの実寸に 1px 足りていなかった --- が直っているかどうか。
// jsdom は `getBoundingClientRect` を計算しないため（常に 0 を返す）、この重なりは
// 原理的にユニットテストでは検出できない（e2e/README.md）。
//
// 見るのは:
//   ⓪ 配っている bundle が dist/ の現物と一致するか
//   ① `--bottom-nav-height`（`main` の `padding-bottom` に使われる）が、ボトムタブの
//      実際の描画高さ（border 込み）と一致すること --- 直した原因そのものの確認。
//      1px でもずれれば、②はドキュメント最下端で必ず重なりを再現する
//   ② ドキュメント最下端まで実際にスクロールしても、行がボトムタブに食い込んで
//      いないこと。**かつ**その余白（最終行の下端からタブ上端までの距離）が
//      負でないことを、0px ちょうどで通る脆いアサーションにせず、①で直した
//      `--bottom-nav-height` の値そのものと突き合わせて確認する
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
 * ある --- 密度が薄いと初回表示で 1 画面に収まってしまい、末尾での重なりの
 * 再現条件が成立しない。
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
 * ボトムタブに食い込んでいる」行を探す。無ければ `null`。
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

// --- ① `--bottom-nav-height` がボトムタブの実際の描画高さ（border 込み）と一致する ---
log('\n=== ① --bottom-nav-height がボトムタブの実測高さと一致する ===')
{
  const measured = await page.evaluate(() => {
    const navRect = document.querySelector('[data-testid="bottom-nav"]')?.getBoundingClientRect()
    // カスタムプロパティ自体は `calc()` / `var()` を含む生の文字列のままなので
    // `getPropertyValue('--bottom-nav-height')` は解決済みの px にならない。
    // `main` は `padding-bottom: var(--bottom-nav-height)` なので、そちらの
    // 計算済み `paddingBottom`（ブラウザが calc/var を解決した px 文字列）を読む。
    const main = document.querySelector('main')
    const varPx = main ? Number.parseFloat(getComputedStyle(main).paddingBottom) : Number.NaN
    return { navHeight: navRect?.height ?? null, varPx }
  })
  log(`  nav 実測高さ        : ${measured.navHeight}`)
  log(`  --bottom-nav-height : ${measured.varPx}`)
  if (measured.navHeight === null) {
    ng.push('① ボトムタブ（[data-testid="bottom-nav"]）が見つからない')
  } else if (Math.abs(measured.navHeight - measured.varPx) > 0.5) {
    ng.push(
      `① --bottom-nav-height（${measured.varPx}px）がボトムタブの実測高さ（${measured.navHeight}px）と一致しない`,
    )
  } else {
    log('  一致（期待どおり）')
  }
}

// --- ② ドキュメント最下端まで実際にスクロールしても重ならない ---
log('\n=== ② 最下端までスクロールしても重ならない（`main` の padding-bottom の回帰確認） ===')
{
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

  // 余白がたまたま 0px ちょうどで通っているだけの状態（①が壊れれば必ずここも
  // 壊れる、余裕ゼロの回帰確認）を避けるため、最終行の下端からタブ上端までの
  // 距離が負でないことも直接見る。
  const gap = await page.evaluate(() => {
    const navTop = document.querySelector('[data-testid="bottom-nav"]')?.getBoundingClientRect()?.top
    const rows = [...document.querySelectorAll('li[data-program-id]')]
    const lastBottom = rows.length > 0 ? rows[rows.length - 1].getBoundingClientRect().bottom : null
    if (navTop === undefined || lastBottom === null) return null
    return navTop - lastBottom
  })
  log(`  最終行下端からタブ上端までの余白: ${gap}px`)
  if (gap === null) {
    ng.push('② 最終行またはボトムタブが見つからず余白を測れない')
  } else if (gap < 0) {
    ng.push(`② 最終行下端からタブ上端までの余白が負（${gap}px）--- 重なっている`)
  } else {
    log('  余白は負ではない（期待どおり）')
  }
}

await context.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
