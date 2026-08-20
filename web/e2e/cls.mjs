// 読み込み中のレイアウトシフト（CLS）の受け入れ判定（issue #309）。
//
// **CLS はレイアウトそのものの指標なので jsdom では原理的に測れない**
// （`getBoundingClientRect()` が常に 0 を返す。web/e2e/README.md「jsdom が
// 測れないもの」）。ここが唯一の判定手段になる。
//
// ブラウザの Layout Instability API（`PerformanceObserver({type:
// 'layout-shift'})`）で `hadRecentInput === false` の `value` を単純合計する。
// **これは Lighthouse が実際に報告する CLS の近似であって同一ではない**
// （Lighthouse は session window でグルーピングしてその最大値を採る。ここでは
// windowing をせず全期間の単純合計を見る）。単純合計は session window の
// 最大値より大きくなることしかないので、ここで 0.10 以下なら Lighthouse の
// 値も 0.10 以下になる --- 逆方向の保証はしない（未検証）。
//
// 見るのは issue #309 の受け入れ基準と、レビュー（PR #406）で足りないと
// 指摘された経路の合計 4 点:
//   ① /search をモバイル幅（390x844）で開き、サービス一覧の取得を遅延させた
//      状態（Lighthouse のスロットル下を模す）で読み込み中の CLS が 0.10 以下
//   ② /home をデスクトップ幅（1280x900）で開き、6 本の GET をすべて遅延させた
//      状態（いずれも 0 件ではない）で読み込み中の CLS が 0.10 以下（「セクション
//      が空→載る」「ListSkeleton → 本文の入れ替え」の両方を踏む）
//   ③ /home をデスクトップ幅で開き、「いま録画中」「警告」が 0 件のまま他の
//      セクション（今夜〜明日の予約・直近の完了）より遅れて解決する状態で
//      読み込み中の CLS が 0.10 以下 --- レビューで発覚した経路: 後続セクション
//      が先に見えている間に出したプレースホルダが、0 件で解決すると実データに
//      置き換わらずただ消え、消えた分だけ後続を引き上げる。「いま録画中」
//      「警告」にプレースホルダを出さない対策（`pages/home.tsx`
//      `showReservationPlaceholder` の doc コメント）の判定はここでしかできない
//      --- ①②はどちらも「置き換わって縮む」経路しか踏まず、「消えて引き上げる」
//      経路を通さない
//   ④ /search をデスクトップ幅（1280x900）で開き、①と同じ遅延を掛けた状態で
//      読み込み中の CLS が 0.10 以下 --- issue #309 のラボ計測は検索デスクトップも
//      0.087（しきい値の一歩手前）を報告しており、`ConditionFields` の対策の
//      根拠（`condition-fields.tsx` のコメント）はビューポート依存の議論
//      （「押される側が折り目の外に出る」）なので、モバイルだけでは踏んでいない
//
// ①④のサービス数は issue 本文が言う実測（NHK 総合だけの e2e フィクスチャでは
// 再現しない小さすぎる数）に近づけるため、地上波 + BS + CS 相当の 24 局を用意する
// --- 2 局だけの `search-mobile.mjs` のフィクスチャではチップが 1 行に収まって
// しまい、直す前の実装でも再現しない。
//
// **②は現時点でも NG のまま（未解決）。** レビュー（PR #406）で、③の経路を
// 塞ぐには「いま録画中」「警告」にプレースホルダを出せないことが分かった一方、
// ②が踏む「後続セクションが先に見えている間に先行セクションが実データで挿し
// 込まれる」経路（挿入による押し下げ）はこの 2 セクションにしか出ない。0.10 を
// 切るには「表示順どおりの prefix 描画」のような別の対策が要り、それは
// `docs/frontend/home.md`「セクションの可視性は個別に」を変える設計判断になる
// ため、この PR では見送っている（`pages/home.tsx` の `showReservationPlaceholder`
// の doc コメント参照）。
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` で丸ごと
// 差し替える（design.mjs と同じ手）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/cls.mjs
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const CLS_THRESHOLD = 0.1
/** サービス一覧・ホームの各 GET に足す遅延（ms）。Lighthouse のスロットル下の RTT を模す。 */
const NETWORK_DELAY_MS = 400

const ng = []
const log = (...a) => console.log(...a)

/** services は地上波 + BS + CS 相当の 24 局（issue 本文の再現に要る件数）。 */
const services = Array.from({ length: 24 }, (_, i) => ({
  networkId: 32736 + i,
  serviceId: 1024 + i,
  name: `テスト局${i + 1}`,
  channelType: i < 12 ? 'GR' : i < 18 ? 'BS' : 'CS',
  channel: String(i + 1),
  remoteControlKeyId: i + 1,
  hasLogoData: false,
  hasPrograms: true,
}))

function json(route, body) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
}

async function delay(ms) {
  await new Promise((resolve) => setTimeout(resolve, ms))
}

/** installClsObserver は layout-shift の合計値を `window.__clsTotal` に積む。 */
async function installClsObserver(page) {
  await page.addInitScript(() => {
    window.__clsTotal = 0
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          if (!entry.hadRecentInput) window.__clsTotal += entry.value
        }
      }).observe({ type: 'layout-shift', buffered: true })
    } catch {
      // 実装していないブラウザ（Chromium 系以外）では計測不能。
      window.__clsTotal = undefined
    }
  })
}

async function readCls(page) {
  return page.evaluate(() => window.__clsTotal)
}

/**
 * verifyBundleMatches は他スクリプトと同じ前提確認（web/e2e/README.md「配って
 * いる bundle が dist/ の現物と一致するか」）。
 */
function verifyBundleMatches(servedHtml) {
  const served = /assets\/(index-[^"]+\.js)/.exec(servedHtml)?.[1]
  let local
  try {
    local = readdirSync(path.join(process.cwd(), 'dist', 'assets')).find((f) =>
      /^index-.*\.js$/.test(f),
    )
  } catch {
    local = undefined
  }
  return { served, local, matches: served !== undefined && served === local }
}

log('\n=== ⓪ 前提条件 ===')
{
  const rootHtml = await fetch(URL_BASE + '/').then((r) => r.text())
  const bundle = verifyBundleMatches(rootHtml)
  if (!bundle.matches) {
    log(`NG  ⓪ 配っている bundle（${bundle.served ?? '不明'}）が dist/assets/（${bundle.local ?? '不明'}）と違う`)
    log('    別プロセス・古いビルドを測っている。以降の判定に意味が無いので打ち切る')
    process.exit(1)
  }
  log(`OK  ⓪ 配っている bundle は自分の dist（${bundle.served}）`)
}

const browser = await chromium.launch()

// --- ① /search（モバイル 390x844）: サービス一覧の取得を遅延 -------------------
log('\n=== ① /search モバイル（サービス一覧を遅延） ===')
{
  const context = await browser.newContext({ viewport: { width: 390, height: 844 } })
  const page = await context.newPage()
  await installClsObserver(page)
  await page.route('**/api/**', async (route) => {
    const p = new URL(route.request().url()).pathname
    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json(route, [SITE])
    if (p === '/api/capabilities') return json(route, { live: false })
    if (p === `/api/sites/${SITE}/services`) {
      await delay(NETWORK_DELAY_MS)
      return json(route, services)
    }
    return json(route, [])
  })

  await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
  // サービス一覧が届いて再レイアウトが収まるまで待つ。
  await page.getByRole('button', { name: 'テスト局1', exact: true }).waitFor({ timeout: 15000 })
  await page.waitForTimeout(500)

  const cls = await readCls(page)
  log(`  CLS（累積、windowing なしの近似）: ${cls}`)
  if (cls === undefined) {
    ng.push('①: このブラウザでは layout-shift が計測できない（判定不能）')
  } else if (cls > CLS_THRESHOLD) {
    ng.push(`①: /search モバイルの読み込み CLS が ${cls.toFixed(3)}（しきい値 ${CLS_THRESHOLD} 超）`)
  }

  await context.close()
}

// --- ② /home（デスクトップ 1280x900）: 6 本の GET をすべて遅延 -----------------
log('\n=== ② /home デスクトップ（全 GET を遅延） ===')
{
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await context.newPage()
  await installClsObserver(page)

  const now = new Date('2026-08-20T12:00:00.000Z')
  await page.clock.setFixedTime(now)

  const recordings = Array.from({ length: 3 }, (_, i) => ({
    id: i + 1,
    site: SITE,
    source: 'manual',
    serviceName: `テスト局${i + 1}`,
    channelType: 'GR',
    channel: String(i + 1),
    networkId: 32736 + i,
    serviceId: 1024 + i,
    eventId: i + 1,
    title: `録画中の番組 ${i + 1}`,
    startAt: new Date(now.getTime() - 3_600_000).toISOString(),
    durationMs: 3_600_000,
    status: 'recording',
    createdAt: new Date(now.getTime() - 3_600_000).toISOString(),
  }))
  const reservations = Array.from({ length: 5 }, (_, i) => ({
    id: i + 1,
    site: SITE,
    programId: (i + 1) * 10,
    source: 'manual',
    state: 'active',
    title: `予約 ${i + 1}`,
    serviceName: `テスト局${i + 1}`,
    startAt: new Date(now.getTime() + (i + 1) * 3_600_000).toISOString(),
    durationMs: 1_800_000,
    createdAt: new Date(now.getTime() - 3_600_000).toISOString(),
    updatedAt: new Date(now.getTime() - 3_600_000).toISOString(),
    skip: false,
  }))
  const finished = Array.from({ length: 6 }, (_, i) => ({
    id: 100 + i,
    site: SITE,
    source: 'manual',
    serviceName: `テスト局${i + 1}`,
    channelType: 'GR',
    channel: String(i + 1),
    networkId: 32736 + i,
    serviceId: 1024 + i,
    eventId: 100 + i,
    title: `完了した番組 ${i + 1}`,
    startAt: new Date(now.getTime() - (i + 2) * 3_600_000).toISOString(),
    durationMs: 3_600_000,
    status: 'finished',
    createdAt: new Date(now.getTime() - (i + 2) * 3_600_000).toISOString(),
  }))

  // 6 本の GET の応答タイミングをずらす（実サーバーへの複数リクエストが必ず同時に
  // 揃うとは限らないことを模す。全部同じ遅延だと「セクションが順番に食い違って
  // 挿し直される」経路を通さないまま緑になる）。
  const routeDelays = {
    '/api/reservations': NETWORK_DELAY_MS,
    '/api/breakers': NETWORK_DELAY_MS + 150,
    '/api/capacity/overages': NETWORK_DELAY_MS + 250,
  }
  const routeBodies = {
    '/api/reservations': reservations,
    '/api/breakers': [],
    '/api/capacity/overages': [],
  }

  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json(route, [SITE])
    if (p === '/api/capabilities') return json(route, { live: false })
    if (p === '/api/recordings') {
      const status = url.searchParams.get('status')
      if (status === 'recording') {
        await delay(NETWORK_DELAY_MS + 350)
        return json(route, recordings)
      }
      if (status === 'finished') {
        await delay(NETWORK_DELAY_MS + 50)
        return json(route, finished)
      }
      await delay(NETWORK_DELAY_MS)
      return json(route, [])
    }
    if (routeDelays[p] !== undefined) {
      await delay(routeDelays[p])
      return json(route, routeBodies[p])
    }
    return json(route, [])
  })

  await page.goto(URL_BASE + '/', { waitUntil: 'domcontentloaded' })
  // 全セクションの決着を待つ（最も遅いクエリの遅延 + 余裕）。
  await page.waitForTimeout(NETWORK_DELAY_MS + 600)
  await page.getByText('録画中の番組 1').waitFor({ timeout: 15000 })
  await page.waitForTimeout(500)

  const cls = await readCls(page)
  log(`  CLS（累積、windowing なしの近似）: ${cls}`)
  if (cls === undefined) {
    ng.push('②: このブラウザでは layout-shift が計測できない（判定不能）')
  } else if (cls > CLS_THRESHOLD) {
    ng.push(`②: /home デスクトップの読み込み CLS が ${cls.toFixed(3)}（しきい値 ${CLS_THRESHOLD} 超）`)
  }

  await context.close()
}

// --- ③ /home（デスクトップ）: 「いま録画中」「警告」が 0 件で解決 -------------
log('\n=== ③ /home デスクトップ（いま録画中・警告が 0 件で解決） ===')
{
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await context.newPage()
  await installClsObserver(page)

  const now = new Date('2026-08-20T12:00:00.000Z')
  await page.clock.setFixedTime(now)

  const reservations = Array.from({ length: 5 }, (_, i) => ({
    id: i + 1,
    site: SITE,
    programId: (i + 1) * 10,
    source: 'manual',
    state: 'active',
    title: `予約 ${i + 1}`,
    serviceName: `テスト局${i + 1}`,
    startAt: new Date(now.getTime() + (i + 1) * 3_600_000).toISOString(),
    durationMs: 1_800_000,
    createdAt: new Date(now.getTime() - 3_600_000).toISOString(),
    updatedAt: new Date(now.getTime() - 3_600_000).toISOString(),
    skip: false,
  }))
  const finished = Array.from({ length: 6 }, (_, i) => ({
    id: 100 + i,
    site: SITE,
    source: 'manual',
    serviceName: `テスト局${i + 1}`,
    channelType: 'GR',
    channel: String(i + 1),
    networkId: 32736 + i,
    serviceId: 1024 + i,
    eventId: 100 + i,
    title: `完了した番組 ${i + 1}`,
    startAt: new Date(now.getTime() - (i + 2) * 3_600_000).toISOString(),
    durationMs: 3_600_000,
    status: 'finished',
    createdAt: new Date(now.getTime() - (i + 2) * 3_600_000).toISOString(),
  }))

  // 「今夜〜明日の予約」「直近の完了」を先に解決させ、「いま録画中」「警告」の
  // 材料（recording / breakers / overages / failed）は全部 0 件のまま、それより
  // 遅れて解決させる --- レビューで実測された経路（先に見えているセクションの
  // 上に出したプレースホルダが、0 件で解決すると実データに置き換わらずただ消え、
  // 消えた分だけ後続を引き上げる）を踏む順序。
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json(route, [SITE])
    if (p === '/api/capabilities') return json(route, { live: false })
    if (p === '/api/recordings') {
      const status = url.searchParams.get('status')
      if (status === 'finished') {
        await delay(NETWORK_DELAY_MS)
        return json(route, finished)
      }
      // recording / failed は 0 件のまま他より遅らせる。
      await delay(NETWORK_DELAY_MS + 400)
      return json(route, [])
    }
    if (p === '/api/reservations') {
      await delay(NETWORK_DELAY_MS)
      return json(route, reservations)
    }
    // breakers / overages（警告の残り 2 本）も 0 件のまま遅らせる。
    await delay(NETWORK_DELAY_MS + 400)
    return json(route, [])
  })

  await page.goto(URL_BASE + '/', { waitUntil: 'domcontentloaded' })
  // 全セクションの決着を待つ（最も遅いクエリの遅延 + 余裕）。
  await page.waitForTimeout(NETWORK_DELAY_MS + 600)
  await page.getByText('予約 1').waitFor({ timeout: 15000 })
  await page.waitForTimeout(500)

  const cls = await readCls(page)
  log(`  CLS（累積、windowing なしの近似）: ${cls}`)
  if (cls === undefined) {
    ng.push('③: このブラウザでは layout-shift が計測できない（判定不能）')
  } else if (cls > CLS_THRESHOLD) {
    ng.push(`③: /home デスクトップ（0 件解決）の読み込み CLS が ${cls.toFixed(3)}（しきい値 ${CLS_THRESHOLD} 超）`)
  }

  await context.close()
}

// --- ④ /search（デスクトップ 1280x900）: サービス一覧の取得を遅延 -------------
log('\n=== ④ /search デスクトップ（サービス一覧を遅延） ===')
{
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await context.newPage()
  await installClsObserver(page)
  await page.route('**/api/**', async (route) => {
    const p = new URL(route.request().url()).pathname
    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json(route, [SITE])
    if (p === '/api/capabilities') return json(route, { live: false })
    if (p === `/api/sites/${SITE}/services`) {
      await delay(NETWORK_DELAY_MS)
      return json(route, services)
    }
    return json(route, [])
  })

  await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: 'テスト局1', exact: true }).waitFor({ timeout: 15000 })
  await page.waitForTimeout(500)

  const cls = await readCls(page)
  log(`  CLS（累積、windowing なしの近似）: ${cls}`)
  if (cls === undefined) {
    ng.push('④: このブラウザでは layout-shift が計測できない（判定不能）')
  } else if (cls > CLS_THRESHOLD) {
    ng.push(`④: /search デスクトップの読み込み CLS が ${cls.toFixed(3)}（しきい値 ${CLS_THRESHOLD} 超）`)
  }

  await context.close()
}

await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
