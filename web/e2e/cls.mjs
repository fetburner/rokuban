// 読み込み中のレイアウトシフト（CLS）の受け入れ判定。
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
// 見るのは検索（`/search`、`components/condition-fields.tsx`）の 2 点:
//   ① モバイル幅（390x844）でサービス一覧の取得を遅延させた状態（Lighthouse の
//      スロットル下を模す）で読み込み中の CLS が 0.10 以下
//   ② デスクトップ幅（1280x900）で①と同じ遅延を掛けた状態で 0.10 以下 ---
//      ラボ計測は検索デスクトップも 0.087（しきい値の一歩手前）を報告しており、
//      `ConditionFields` の対策の根拠（`condition-fields.tsx` のコメント）は
//      ビューポート依存の議論（「押される側が折り目の外に出る」）なので、
//      モバイルだけでは踏んでいない
//
// サービス数は実測（NHK 総合だけの e2e フィクスチャでは再現しない小さすぎる数）
// に近づけるため、地上波 + BS + CS 相当の 24 局を用意する --- 2 局だけの
// `search-mobile.mjs` のフィクスチャではチップが 1 行に収まってしまい、直す前の
// 実装でも再現しない。**直す前の `condition-fields.tsx`（`ServiceFields` が
// `TextMatchFields` の直後）では①が 0.165、②が 0.033 で、①が実際に落ちる。**
//
// **ホーム（`/`）はここでは CLS を測らない。** ラボ計測はホームのデスクトップで
// 0.111（スロットル時のみ。LAN では 0）を報告するが、その原因（4 セクションの
// 表示順は固定なのに解決順は不定で、後続が先に見えている状態で先行セクションが
// 実データごと上に挿し込まれる）を消すには、各セクションの独立した可視性を
// 捨てる必要がある。`docs/frontend/home.md`「セクションの可視性は個別に」は
// その独立を優先し、上界のない予約取得に録画中を引きずらせないためにシフトを
// 許容すると決めた。したがってホームに CLS の受け入れ判定は置かない。
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` で丸ごと
// 差し替える（design.mjs と同じ手）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/cls.mjs
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { finish, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const CLS_THRESHOLD = 0.1
/** サービス一覧の GET に足す遅延（ms）。Lighthouse のスロットル下の RTT を模す。 */
const NETWORK_DELAY_MS = 400

const ng = []

/** services は地上波 + BS + CS 相当の 24 局（実測の再現に要る件数）。 */
const services = Array.from({ length: 24 }, (_, i) => ({
  id: (32736 + i) * 100_000 + (1024 + i),
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

/**
 * installClsObserver は layout-shift の合計値を `window.__clsTotal` に積む。
 *
 * **`supportedEntryTypes` を見てから observe する。** `observe({type: ...})` が
 * 未知の type で throw するかは実装依存（仕様は「無視して警告」も許している）
 * ので、try/catch では「計測できていないのに `__clsTotal` が 0 のまま合格」に
 * なりうる。ここは `chromium` 固定なので現状は必ず対応しているが、対応の有無を
 * 例外の有無で推測する形は残さない。
 */
async function installClsObserver(page) {
  await page.addInitScript(() => {
    const supported = globalThis.PerformanceObserver?.supportedEntryTypes ?? []
    if (!supported.includes('layout-shift')) {
      // このブラウザでは判定不能（0 ではなく undefined にして NG に落とす）。
      window.__clsTotal = undefined
      return
    }
    window.__clsTotal = 0
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) {
        if (!entry.hadRecentInput) window.__clsTotal += entry.value
      }
    }).observe({ type: 'layout-shift', buffered: true })
  })
}

async function readCls(page) {
  return page.evaluate(() => window.__clsTotal)
}

log('\n=== ⓪ 前提条件 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

/**
 * measureSearch は `/search` を指定のビューポートで開き、サービス一覧の取得だけを
 * 遅延させた状態の読み込み CLS を返す。
 */
async function measureSearch(viewport) {
  const context = await browser.newContext({ viewport })
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
  await context.close()
  return cls
}

const cases = [
  { label: '① /search モバイル（サービス一覧を遅延）', viewport: { width: 390, height: 844 } },
  { label: '② /search デスクトップ（サービス一覧を遅延）', viewport: { width: 1280, height: 900 } },
]

for (const c of cases) {
  log(`\n=== ${c.label} ===`)
  const cls = await measureSearch(c.viewport)
  log(`  CLS（累積、windowing なしの近似）: ${cls}`)
  if (cls === undefined) {
    ng.push(`${c.label}: このブラウザでは layout-shift が計測できない（判定不能）`)
  } else if (cls > CLS_THRESHOLD) {
    ng.push(`${c.label}: 読み込み CLS が ${cls.toFixed(3)}（しきい値 ${CLS_THRESHOLD} 超）`)
  }
}

await finish(ng, browser)
