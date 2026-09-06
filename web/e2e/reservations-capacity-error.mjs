// 予約一覧の容量 API が失敗したときに、「要確認なし」を確定しないことの受け入れ判定。
//
// 見るのは:
//   ① 容量 API の全リトライが失敗しても、容量確認失敗の理由と再試行を表示する
//   ② その状態で attention フィルタの空状態を表示せず、全件表示へ戻れる
//   ③ 新しいブラウザコンテキストで同じ予約と正常な容量応答を返すと、バッジと
//      要確認チップが表示される
//
// 実 API / mirakc / DB は要らない。API は page.route で差し替える。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:reservations-capacity-error
import { ListCapacityOveragesResponseItem, ListReservationsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const SITE = 'default'
const HOUR = 3_600_000
const startMs = Date.now() + HOUR
const iso = (ms) => new Date(ms).toISOString()

const reservations = [
  {
    site: SITE,
    programId: 9001,
    source: 'manual',
    state: 'active',
    title: '容量確認の予約',
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: iso(startMs),
    durationMs: HOUR,
    createdAt: iso(Date.now()),
    updatedAt: iso(Date.now()),
    skip: false,
  },
]

const overages = [
  {
    site: SITE,
    startAt: iso(startMs),
    endAt: iso(startMs + HOUR),
    shortfall: 1,
    jammedTypes: ['BS'],
  },
]

const ng = []
const errorMessage = '容量の確認に失敗しました。要確認の判定が不完全です'

log('URL: ' + URL_BASE)

await validateFixturesOrExit(
  [
    ['reservations[0]', ListReservationsResponseItem, reservations[0]],
    ['overages[0]', ListCapacityOveragesResponseItem, overages[0]],
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

function installReservationsStubs(page, mode) {
  return installApiStubs(page, async ({ path: p, json, route }) => {
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/reservations') return json(reservations)
    if (p === '/api/capacity/overages') {
      if (mode === 'failure') {
        return route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'capacity unavailable' }),
        })
      }
      return json(overages)
    }
    return json([])
  })
}

const browser = await launchBrowser()

// --- ① 失敗: リトライ終了後も「確認が要る予約はありません」を出さない ---
const failedContext = await browser.newContext({
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const failedPage = await failedContext.newPage()
await installReservationsStubs(failedPage, 'failure')
await failedPage.goto(URL_BASE + '/reservations?only=attention', {
  waitUntil: 'domcontentloaded',
})

log('\n=== ① 容量 API 失敗時の予約一覧 ===')
await failedPage.getByText(errorMessage).waitFor({ timeout: 15_000 })
if ((await failedPage.getByText('確認が要る予約はありません').count()) > 0) {
  ng.push('① 容量 API 失敗時に「確認が要る予約はありません」が表示された')
}
if ((await failedPage.getByRole('button', { name: '再試行' }).count()) !== 1) {
  ng.push('① 容量 API 失敗時に再試行ボタンが 1 個表示されない')
}
if ((await failedPage.getByRole('button', { name: 'すべて（1）' }).count()) !== 1) {
  ng.push('① 容量 API 失敗時に全件表示へ戻るチップが表示されない')
} else {
  await failedPage.getByRole('button', { name: 'すべて（1）' }).click()
  await failedPage.getByText('容量確認の予約').waitFor()
}
await failedContext.close()

// --- ② 正常応答: 同じ予約なら不足バッジと要確認チップが復旧する ---
const successContext = await browser.newContext({
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const successPage = await successContext.newPage()
await installReservationsStubs(successPage, 'success')
await successPage.goto(URL_BASE + '/reservations?only=attention', {
  waitUntil: 'domcontentloaded',
})

log('\n=== ② 容量 API 正常時の予約一覧 ===')
await successPage.getByText('チューナー不足（BS が 1 本）').waitFor({ timeout: 15_000 })
const attentionChip = successPage.getByRole('button', { name: '要確認（1）' })
if ((await attentionChip.count()) !== 1) {
  ng.push('② 正常応答時に要確認（1）チップが表示されない')
} else if ((await attentionChip.getAttribute('aria-pressed')) !== 'true') {
  ng.push('② attention フィルタが選択状態になっていない')
}

await finish(ng, browser)
