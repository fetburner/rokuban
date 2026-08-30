// SSE が 1 通も届かないときの定期 REST 再取得の受け入れ判定。
//
// jsdom の単体テスト（src/lib/events.test.tsx）はフック単体しか通らないので、
// 「画面が実際に使っているクエリキー」を外している取りこぼしを検出できない
// （`epg` トピックが番組リストに一度も届いていなかった実例は docs/api/sse.md
// 「経緯と失敗事例」）。ここでは実ブラウザに**ビルド済みの bundle** を読ませ、
// `/api/**` をスタブして**リクエスト数を数える**。
//
// 合格なら exit 0、1 つでも NG なら exit 1。
//
// 使い方（Go サーバーも Postgres も要らない。API は全部スタブする）:
//
//   cd web && pnpm build && pnpm exec vite preview --port 4173 --strictPort &
//   node e2e/sse-refresh.mjs
//
// SSE の再接続時 invalidate はここでは見ない（実ブラウザで切断を決定的に
// 起こす手段が無い）。単体テスト「再接続したら切断中の変更を全グループ取り直す」の担当。
import { ListReservationsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
/** 運用状態の周期（src/lib/events.ts の operationalRefreshIntervalMs と同じ値をリテラルで書く）。 */
const operationalMs = 60_000
/** EPG の周期（同 epgRefreshIntervalMs）。 */
const epgMs = 600_000

/** 現在測っているページのリクエスト数。ページを変えるたびに差し替える。 */
let counts = new Map()
const ng = []
const count = (path) => counts.get(path) ?? 0
/** check は期待値と実測を突き合わせる。落ちても続行して全部の NG を出す。 */
const check = (label, actual, expected) => {
  if (actual === expected) log(`OK  ${label}: ${actual}`)
  else {
    log(`NG  ${label}: ${actual}（期待 ${expected}）`)
    ng.push(label)
  }
}

// ⓪ 配っている bundle が dist/ の現物と一致するか（e2e/lib.mjs 参照）。
await verifyBundleMatchesOrExit(BASE, ng)

const reservation = {
  id: 111,
  site: 'tokyo',
  programId: 300000,
  source: 'manual',
  state: 'active',
  title: 'e2e 予約',
  serviceName: 'テスト局',
  channelType: 'GR',
  startAt: '2026-08-14T00:00:00.000Z',
  durationMs: 1_800_000,
  createdAt: '2026-08-13T00:00:00.000Z',
  updatedAt: '2026-08-13T00:00:00.000Z',
  skip: false,
}

// 契約検証: フィクスチャが orval 生成の zod スキーマと一致するか
// （`validateFixturesOrExit`。design.mjs / e2e/README.md §デザイン 参照）。
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['reservation', ListReservationsResponseItem, reservation]], ng)

const browser = await launchBrowser()

/**
 * openStubbed は `/api/**` を丸ごと差し替えたページを開く。カウンタ（`counts`）は
 * ページごとに新しくするので、増分はそのページの回復経路だけを表す。
 *
 * `overrides` はパス名 -> レスポンス JSON 文字列の対応。既定のスタブ（`[]` 等）で
 * 画面が要求を満たせないエンドポイントだけ個別に上書きする。
 */
async function openStubbed(pathname, label, overrides = {}) {
  counts = new Map()
  const p = await browser.newPage({ viewport: { width: 1280, height: 800 } })
  p.on('pageerror', (e) => {
    log(`NG  ページ例外（${label}）:`, e.message)
    ng.push(`pageerror（${label}）`)
  })
  await p.route('**/api/**', async (route) => {
    const requested = new URL(route.request().url()).pathname
    counts.set(requested, count(requested) + 1)
    // 接続は張るが通知は 1 通も送らない（notifier がバッファ満杯で捨てた状態）。
    if (requested === '/api/events') return sseKeepAlive(route)
    const body =
      overrides[requested] ??
      (requested === '/api/capabilities'
        ? '{"encode":false,"live":false,"storage":false}'
        : requested === '/api/version'
          ? '{"version":"e2e"}'
          : requested === '/api/sites'
            ? '["tokyo"]'
            : /\/reservation$/.test(requested)
              ? JSON.stringify(reservation)
              : /\/overlaps$/.test(requested)
                ? '{"count":0,"reservations":[]}'
                : '[]')
    await route.fulfill({ status: 200, headers: { 'content-type': 'application/json' }, body })
  })
  // 時計を握ってから開く。10 分を実時間で待たない
  await p.clock.install()
  await p.goto(BASE + pathname, { waitUntil: 'networkidle' })
  await p.waitForTimeout(500)
  return p
}

// 番組リスト（EPG）と予約・ブレーカー（運用状態）が同じ画面から出る
const page = await openStubbed('/programs', '番組表')

const programs = '/api/sites/tokyo/programs'
const services = '/api/sites/tokyo/services'
const programsWallTime = await page.evaluate(() => Date.now())

/**
 * advanceOneOperationalCycle は仮想時計を運用状態 1 周期ぶん進め、その周期で
 * `/programs` が再取得する予約とブレーカーのレスポンスが完了してから返す。
 *
 * `runFor(540_000)` で 9 周期を一気に発火した計測では 20 回中 5 回、予約の
 * refetch が 1〜2 本発行されなかった。route entry だけ待つ実装も、fulfill を
 * 100ms 遅らせた計測で前サイクルの予約・ブレーカーが 18 本 abort されたため、
 * in-flight の重なりを除けない。`response.finished()` で本文受信まで待ち、続く
 * ブラウザ往復で fetch の継続処理を進める形では、同じ 100ms 遅延で番組表区間の
 * abort は 0 本だった。`setSystemTime` はタイマーを発火せず壁時計だけ戻すため、
 * interval の経過は保ったまま、1 周期ずつ settle した計測で仮想 5 分時点に
 * programs が 1 から 2 へ増えた壁時計由来の別 fetch も除く。ここまで同期して
 * から次の仮想 60 秒へ進む。
 *
 * 待つのは各周期の 2 レスポンスだけで、期待総数まではポーリングしない。
 * 周期短縮は最後の `=== 11` / `=== 2` に余剰、周期延長は waitForResponse の
 * timeout または最後の厳密比較に不足として現れる。
 */
async function advanceOneOperationalCycle() {
  const responses = ['/api/reservations', '/api/breakers'].map((expectedPath) =>
    page
      .waitForResponse(
        (response) => new URL(response.url()).pathname === expectedPath,
        { timeout: 2000 },
      )
      .then(async (response) => {
        const error = await response.finished()
        if (error !== null) throw error
      }),
  )
  await page.clock.runFor(operationalMs)
  await Promise.all(responses)
  await page.clock.setSystemTime(programsWallTime)
}

log('初回ロード後:', Object.fromEntries([...counts.entries()].sort()))
check('初回: 予約', count('/api/reservations'), 1)
check('初回: 番組リスト', count(programs), 1)
if (ng.length > 0) {
  log('初回ロードで既に期待とずれている。以降の増分は判定できない')
  await finish(ng, browser)
}

// 運用状態は 60 秒で取り直す。1 周期進めて着弾を待つ。EPG はまだ動かない
await advanceOneOperationalCycle()
check('60 秒後: 予約', count('/api/reservations'), 2)
check('60 秒後: ブレーカー', count('/api/breakers'), 2)
check('60 秒後: 番組リスト（まだ増えない）', count(programs), 1)

// 10 分でちょうど EPG の 1 周。運用状態はこの間にさらに 9 回（計 10 回）。
// まとめて runFor せず 1 周期ずつ settle する理由は advanceOneOperationalCycle 参照
for (let i = 0; i < epgMs / operationalMs - 1; i++) {
  await advanceOneOperationalCycle()
}
// 最後の 1 周期で EPG（programs / services）も 1 回だけ発火する。固定待ちは
// EPG の着弾ぶんだけで、期待総数まではポーリングしない
await page.waitForTimeout(500)
check('10 分後: 番組リスト', count(programs), 2)
check('10 分後: サービス一覧', count(services), 2)
check('10 分後: 予約', count('/api/reservations'), 1 + epgMs / operationalMs)

// 予約詳細（別ページ）。URL は /api/sites/... なので、生成キーのままだと
// EPG グループ（10 分）に落ちる --- ここで見るのは「60 秒側で取り直すこと」。
// キーの先頭要素が所属を決めるという規律（docs/api/sse.md）を、画面が実際に
// 使っているキーの上で確かめる唯一の判定。
log('\n=== 予約詳細（/reservations/$site/$programId）===')
const detailPage = await openStubbed('/reservations/tokyo/300000', '予約詳細')

const detail = '/api/sites/tokyo/programs/300000/reservation'
log('初回ロード後:', Object.fromEntries([...counts.entries()].sort()))
check('初回: 予約詳細', count(detail), 1)
await detailPage.clock.runFor(operationalMs)
await detailPage.waitForTimeout(500)
check('60 秒後: 予約詳細（運用状態グループ）', count(detail), 2)

// ストレージ残高（components/storage-balance.tsx。設置先は pages/recordings.tsx の
// 1 箇所だけ）。SSE トピックを持たず、専用の 5 分周期の定期 invalidate だけで
// 収束することを実ブラウザで確認する（docs/api/sse.md の実測値と対応させる）。
log('\n=== ストレージ残高（/recordings）===')
const storageMs = 300_000 // events.ts の storageRefreshIntervalMs と同じ値をリテラルで書く
// observedAt は実行時刻から 1 分前にする。`page.clock.install()` は実時刻を初期値に
// するので、固定日付を書くと storage-forecast.ts の observationStaleAfterMs（1 時間）を
// 必ず超え、「観測が古い可能性」の表示を測ることになる。判定はリクエスト数だけなので
// 合否は変わらないが、正常な観測が載っている画面を測る。
const observedAt = new Date(Date.now() - 60_000).toISOString()
const storagePage = await openStubbed('/recordings', '録画一覧 / ストレージ残高', {
  '/api/storage':
    '[{"root":"media","path":"/data/media","totalBytes":1000000000000,' +
    '"usedBytes":400000000000,"availableBytes":600000000000,' +
    `"observedAt":"${observedAt}"}]`,
})

log('初回ロード後:', Object.fromEntries([...counts.entries()].sort()))
check('初回: ストレージ', count('/api/storage'), 1)

await storagePage.clock.runFor(storageMs)
await storagePage.waitForTimeout(500)
check('5 分後: ストレージ', count('/api/storage'), 2)

await storagePage.clock.runFor(storageMs)
await storagePage.waitForTimeout(500)
check('10 分後: ストレージ', count('/api/storage'), 3)

await finish(ng, browser)
