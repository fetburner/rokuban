// `--bottom-nav-height`（`web/src/index.css`）がボトムタブの実際の描画高さと
// 一致しているかの受け入れ判定。
//
// **この判定は「行がボトムタブに隠れる」症状そのものを見ていない。** 390×844 の
// 実測では、`--bottom-nav-height` から上辺の境界線ぶんが落ちていた状態でも、
// ドキュメント最下端までスクロールしたときの余白はちょうど 0px（末行の下端が
// タブの上端に接する）で、隠れていた画素は無かった。落ちていたのは
// 「タブの実寸ぶんを確保する」という計算の正しさと、その 1px ぶんの余裕だけ。
//
// 一方、**行がタブの裏に半分入る症状は初回表示や任意のスクロール位置で今も起きる**
// （下の④が毎回その量を実測して表示する。上記の実測では 29px）。これはページ全体
// スクロール + `fixed` なタブというレイアウトの性質で、この変数を直しても消えない
// （詳細は `docs/frontend/scroll.md`「ボトムタブの裏に隠れる行」）。
//
// jsdom は `getBoundingClientRect` を計算しない（常に 0 を返す）ので、ここで見る
// 値はどれもユニットテストでは原理的に取れない。
//
// 見るのは:
//   ⓪ 配っている bundle が dist/ の現物と一致するか
//   ① `main` の計算済み `padding-bottom`（= `--bottom-nav-height`）が、ボトムタブの
//      実際の描画高さ（border 込み）と一致すること。**上辺の境界線ぶんを落とすと
//      64px 対 65px で落ちる**（実測で確認済み）。タブ本体の高さ・padding・border を
//      変えたのに計算を直さなかった場合もここで落ちる
//   ② 最下端までスクロールしたとき、`main` の内容ボックスの下端がタブの上端より
//      下に来ていないこと。①の帰結を実レイアウトで見るもので、**①に加えて
//      「`main` が文書の最下端の箱である」（下に高さを持つ兄弟が無い）ことも
//      確認する**。①と同じ変異（境界線ぶんを落とす）で 780px 対 779px で落ちる
//   ③ 最下端までスクロールしたとき、どの行もタブの上端をまたいでいないこと。
//      **これは余裕ゼロの回帰確認で、検出力は無い** --- 上記のとおり境界線ぶんが
//      落ちていた状態でも余白は 0px で、またいでいる行は無かった（修正前でも通る）。
//      それでも置いてあるのは、①②が捉えない別種の壊れ方（タブの高さは合っている
//      のに行が絶対配置でせり出す等）に対する網として
//   ④ 初回表示でタブの裏に入っている画素数の実測値（**表示のみ。合否には影響しない**）
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
 * programsFor は要求された窓を 30 分番組で埋める。`pages/programs.tsx` の
 * `windowHours`（6 時間）ぶんだけで 390×844 のビューポートより長くなる密度に
 * なる --- 密度が薄いと 1 画面に収まってしまい、最下端でも初回表示でも
 * タブとの関係を測れない。
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
 * measure はボトムタブ・`main`・描かれている行の幾何を 1 回のフレームで読む。
 * 行の下端は DOM 順の最後ではなく `Math.max` で取る（仮想化リストの DOM 順が
 * 視覚順と一致するかどうかに依存しないため）。
 */
async function measure(page) {
  return page.evaluate(() => {
    const nav = document.querySelector('[data-testid="bottom-nav"]')
    const main = document.querySelector('main')
    if (!nav || !main) return { navPresent: false }
    const navRect = nav.getBoundingClientRect()
    if (navRect.height <= 0) return { navPresent: false }
    const mainRect = main.getBoundingClientRect()
    const mainPaddingBottom = Number.parseFloat(getComputedStyle(main).paddingBottom)
    const rows = [...document.querySelectorAll('li[data-program-id]')].map((row) => ({
      rect: row.getBoundingClientRect(),
      text: row.innerText.replace(/\s+/g, ' ').slice(0, 60),
    }))
    // 「上端は見えているが下端がタブに食い込んでいる」行（= 半分だけ隠れて
    // ユーザーの目に入る行）。r.bottom === navRect.top（接するだけ）は隠れて
    // いないので含めない。
    const straddling = rows
      .filter((r) => r.rect.top < navRect.top && r.rect.bottom > navRect.top)
      .map((r) => ({
        text: r.text,
        top: r.rect.top,
        bottom: r.rect.bottom,
        hidden: r.rect.bottom - navRect.top,
      }))
    return {
      navPresent: true,
      navTop: navRect.top,
      navHeight: navRect.height,
      mainPaddingBottom,
      // `main` が下に確保している余白の内側の端。ここがタブの上端より下に
      // 来ていたら、確保量がタブの実寸に足りていない。
      mainContentBottom: mainRect.bottom - mainPaddingBottom,
      rowCount: rows.length,
      lowestRowBottom: rows.length > 0 ? Math.max(...rows.map((r) => r.rect.bottom)) : null,
      straddling,
      atBottom: Math.abs(window.scrollY - (document.documentElement.scrollHeight - window.innerHeight)) < 2,
    }
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

const initial = await measure(page)
if (!initial.navPresent) {
  ng.push('ボトムタブ（[data-testid="bottom-nav"]）が見つからない・高さ 0')
}

// --- ① `--bottom-nav-height` がボトムタブの実際の描画高さ（border 込み）と一致する ---
log('\n=== ① --bottom-nav-height がボトムタブの実測高さと一致する ===')
if (initial.navPresent) {
  // カスタムプロパティ自体は `calc()` / `var()` を含む生の文字列のままなので
  // `getPropertyValue('--bottom-nav-height')` は解決済みの px にならない。
  // `main` は `padding-bottom: var(--bottom-nav-height)` なので、そちらの
  // 計算済み `paddingBottom`（ブラウザが calc/var を解決した px 文字列）を読む。
  log(`  nav 実測高さ（border 込み）: ${initial.navHeight}px`)
  log(`  main の padding-bottom     : ${initial.mainPaddingBottom}px`)
  if (Math.abs(initial.navHeight - initial.mainPaddingBottom) > 0.5) {
    ng.push(
      `① main の padding-bottom（${initial.mainPaddingBottom}px。= --bottom-nav-height）がボトムタブの実測高さ（${initial.navHeight}px）と一致しない --- index.css の --bottom-nav-height の計算がタブの実寸とずれている`,
    )
  } else {
    log('  一致（期待どおり）')
  }
}

// --- ④ 初回表示でタブの裏に入っている量（表示のみ。合否には影響しない） ---
log('\n=== ④ 初回表示でタブの裏に入っている量（未解決。表示のみ） ===')
if (initial.navPresent) {
  if (initial.straddling.length === 0) {
    log('  タブの上端をまたいでいる行は無い')
  } else {
    for (const s of initial.straddling) {
      log(
        `  「${s.text}」が ${s.hidden.toFixed(1)}px タブの裏に入っている（top=${s.top.toFixed(1)}, bottom=${s.bottom.toFixed(1)}, タブ上端=${initial.navTop.toFixed(1)}）`,
      )
    }
    log('  --- これは `--bottom-nav-height` の問題ではなく直っていない。')
    log('      ページ全体スクロール + fixed なタブという前提を変えないと消えない')
    log('      （docs/frontend/scroll.md「ボトムタブの裏に隠れる行」）')
  }
}

// --- 最下端まで実際にスクロールする ---
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
}

const bottom = await measure(page)
if (!bottom.navPresent) {
  ng.push('最下端でボトムタブが見つからない・高さ 0')
} else if (!bottom.atBottom) {
  ng.push('最下端までスクロールできていない（この状態の②③は何も主張しない）')
}

// --- ② 最下端で `main` の確保した余白がタブの上端まで届いている ---
log('\n=== ② 最下端で main の確保した余白がタブの上端まで届いている ===')
if (bottom.navPresent && bottom.atBottom) {
  log(`  main の内容ボックス下端: ${bottom.mainContentBottom}px`)
  log(`  タブの上端            : ${bottom.navTop}px`)
  if (bottom.mainContentBottom > bottom.navTop + 0.5) {
    ng.push(
      `② 最下端で main の内容ボックス下端（${bottom.mainContentBottom}px）がタブの上端（${bottom.navTop}px）より下にある --- 確保量がタブの実寸に足りていない（差 ${(bottom.mainContentBottom - bottom.navTop).toFixed(1)}px）`,
    )
  } else {
    log('  届いている（期待どおり）')
  }
}

// --- ③ 最下端でタブの上端をまたいでいる行が無い（余裕ゼロの回帰確認） ---
log('\n=== ③ 最下端でタブの上端をまたいでいる行が無い（余裕ゼロの回帰確認） ===')
if (bottom.navPresent && bottom.atBottom) {
  log(`  最も下の行の下端: ${bottom.lowestRowBottom}px`)
  log(
    `  タブ上端までの余白: ${bottom.lowestRowBottom === null ? '(行が無い)' : (bottom.navTop - bottom.lowestRowBottom).toFixed(1) + 'px'}`,
  )
  // ここに「余白 >= 1px」のような下限を置かない。上の余白の実測 1px は
  // リストの下端（`ul` の下端）と `main` の内容ボックス下端の間の 1px 差から
  // 来ており、この変数の計算とは無関係なので、期待値にすると意味の無い数字を
  // 固定することになる（②が確保量そのものを見ている）。
  if (bottom.lowestRowBottom === null) {
    ng.push('③ 行が 1 つも描かれていない')
  } else if (bottom.straddling.length > 0) {
    const s = bottom.straddling[0]
    ng.push(
      `③ 最下端で行がタブの裏に ${s.hidden.toFixed(1)}px 入っている: 「${s.text}」（top=${s.top.toFixed(1)}, bottom=${s.bottom.toFixed(1)}, タブ上端=${bottom.navTop.toFixed(1)}）`,
    )
  } else {
    log('  またいでいる行は無い（期待どおり。ただし修正前も 0px で通っていた）')
  }
}

await context.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
