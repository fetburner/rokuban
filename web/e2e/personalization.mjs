// 個人化（docs/frontend/design.md §個人化）の実ブラウザ判定。
//
// jsdom では原理的に測れないものだけを見る:
//   - 実際のリロードをまたいだ localStorage の効き（Vitest は再マウントであって
//     リロードではない）
//   - カード表示が本当に段組みになり、サムネイルが行表示より大きいこと（レイアウト）
//   - 検索条件が URL を往復すること（TanStack の stringify + validateSearch の実経路）
//   - 再生速度が録画をまたいでも実際の <video>.playbackRate に効くこと
//     （jsdom の <video> は currentTime/duration 同様 playbackRate も実再生の
//     連動を持たないので、実 <video> でしか確かめられない）
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:personalization
import { ListRecordingsResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  sseKeepAlive,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const ng = []

const recordings = Array.from({ length: 6 }, (_, i) => {
  const id = i + 1
  return {
    id,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: id,
    title: `録画 ${id}`,
    startAt: new Date(Date.parse('2026-01-01T12:00:00Z') + id * 60_000).toISOString(),
    durationMs: 1_800_000,
    status: 'finished',
    // ⑤ の再生速度判定は録画詳細（RecordingPlayer）を出すため、原本 + encoded
    // 派生物を持たせる（他の判定は list 応答しか見ないので影響しない）。
    sizeBytes: 500_000_000,
    encodedAssets: [{ profile: 'h264', sizeBytes: 400_000_000 }],
    createdAt: '2026-01-02T12:30:00Z',
  }
})

const programs = [
  {
    programId: 100,
    networkId: 32736,
    serviceId: 1024,
    eventId: 100,
    startAt: '2026-01-01T20:00:00Z',
    endAt: '2026-01-01T20:30:00Z',
    durationMs: 1_800_000,
    name: 'ニュース7',
    description: '',
    genres: [0],
    isFree: true,
  },
  {
    programId: 101,
    networkId: 32736,
    serviceId: 1024,
    eventId: 101,
    startAt: '2026-01-01T22:00:00Z',
    endAt: '2026-01-01T23:00:00Z',
    durationMs: 3_600_000,
    name: '深夜ドラマ',
    description: '',
    genres: [3],
    isFree: true,
  },
]

/** 検索は条件を無視せずキーワードだけ評価する（固定応答だと URL 往復を検証できない）。 */
const searchBodies = []
async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-profiles' || path === '/api/rules') return json([])
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/capacity/overages') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) {
    return json([
      {
        id: 3273601024,
        networkId: 32736,
        serviceId: 1024,
        name: 'NHK総合',
        channelType: 'GR',
        channel: '27',
        remoteControlKeyId: 1,
        hasLogoData: false,
        hasPrograms: true,
      },
    ])
  }
  if (/^\/api\/recordings\/\d+\/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings' && method === 'GET') {
    return json(url.searchParams.get('trash') === 'true' ? [] : recordings)
  }
  const recordingDetail = /^\/api\/recordings\/(\d+)$/.exec(path)
  if (recordingDetail && method === 'GET') {
    const recording = recordings.find((r) => r.id === Number(recordingDetail[1]))
    return recording ? json(recording) : route.fulfill({ status: 404 })
  }
  if (path === '/api/programs/search' && method === 'POST') {
    const body = JSON.parse(route.request().postData() ?? '{}')
    searchBodies.push(body)
    const keyword = body.textMatches?.[0]?.value ?? ''
    return json(
      programs
        .filter((p) => p.name.includes(keyword))
        .map((p) => ({ site: 'default', programId: p.programId })),
    )
  }
  const program = /^\/api\/sites\/[^/]+\/programs\/(\d+)$/.exec(path)
  if (program) return json(programs.find((p) => p.programId === Number(program[1])) ?? {})
  return json([])
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  recordings.map((recording, i) => [`recordings[${i}]`, ListRecordingsResponseItem, recording]),
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

log('\n=== ① 録画一覧: カード表示がリロードをまたいで残る ===')
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('録画 1').waitFor({ timeout: 15000 })

// 一覧の <ul> を行から辿って掴む。素の `ul li` はサイドバーのナビゲーションにも
// 当たるので、それを測ると「1 行目に 1 枚」のような無関係な値になる（実際に踏んだ）。
const list = page.getByText('録画 1').locator('xpath=ancestor::ul[1]')
// サムネイルは 404 のときプレースホルダ div に差し替わる（`pages/recordings.tsx`）
// ので、img ではなく枠（`aspect-video` の器）を測る。
const thumbFrame = list.locator('li div.aspect-video').first()
const rowThumbBox = await thumbFrame.boundingBox()
await page.getByRole('button', { name: 'カード表示' }).click()
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByText('録画 1').waitFor({ timeout: 15000 })

const pressedAfterReload = await page
  .getByRole('button', { name: 'カード表示' })
  .getAttribute('aria-pressed')
if (pressedAfterReload !== 'true') {
  ng.push(`① リロード後にカード表示へ戻らない（aria-pressed=${pressedAfterReload}）`)
}
if (new URL(page.url()).search !== '') {
  ng.push(`① 表示形式が URL に載っている（${page.url()}）--- 端末ごとの好みは URL の宛先ではない`)
}

log('\n=== ② カードは段組みになり、サムネイルが行表示より大きい ===')
// jsdom はレイアウトを計算しないので、列数もサムネイルの実寸もここでしか測れない。
const boxes = await list.locator('> li').evaluateAll((items) =>
  items.map((el) => {
    const rect = el.getBoundingClientRect()
    return { top: Math.round(rect.top), left: Math.round(rect.left) }
  }),
)
const firstRowColumns = boxes.filter((b) => b.top === boxes[0]?.top).length
if (firstRowColumns < 2) {
  ng.push(`② カード表示が 1 列のまま（1 行目に ${firstRowColumns} 枚）`)
}
const cardThumbBox = await thumbFrame.boundingBox()
if (!rowThumbBox || !cardThumbBox || cardThumbBox.width <= rowThumbBox.width) {
  ng.push(
    `② カードのサムネイルが行表示より大きくない（行 ${rowThumbBox?.width} → カード ${cardThumbBox?.width}）`,
  )
}

log('\n=== ③ 検索: 押した条件が URL に載り、リロードで結果ごと戻る ===')
await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
await page.getByLabel('テキスト条件 1 の値').fill('ニュース')
const searchesBeforeClick = searchBodies.length
await page.getByRole('button', { name: '検索' }).click()
await page.getByText('ニュース7').waitFor({ timeout: 15000 })
// URL を書き換えると検証済みの値（スキーマの既定が埋まった形）が戻ってくるので、
// 素の JSON で「適用済みか」を判定すると同じ条件をもう一度叩く。
await page.waitForTimeout(500)
if (searchBodies.length !== searchesBeforeClick + 1) {
  ng.push(`③ 検索 1 回の押下で ${searchBodies.length - searchesBeforeClick} 回叩いている`)
}

const condParam = new URL(page.url()).searchParams.get('cond')
if (condParam === null || !condParam.includes('ニュース')) {
  ng.push(`③ 押した条件が URL に載らない（${page.url()}）`)
}

const searchesBeforeReload = searchBodies.length
await page.reload({ waitUntil: 'domcontentloaded' })
await page.getByText('ニュース7').waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ リロードで結果が戻らない')
})
if ((await page.getByLabel('テキスト条件 1 の値').inputValue()) !== 'ニュース') {
  ng.push('③ リロードで条件がフォームに戻らない')
}
if (searchBodies.length !== searchesBeforeReload + 1) {
  ng.push(
    `③ リロード後の検索が 1 回でない（${searchBodies.length - searchesBeforeReload} 回）--- URL の条件で二重に叩いている`,
  )
}

log('\n=== ④ 条件なしで開くと前回の条件は戻るが、検索は走らない ===')
const searchesBeforePlain = searchBodies.length
await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })
await page.getByText('条件を指定して検索してください').waitFor({ timeout: 15000 }).catch(() => {
  ng.push('④ 開いただけで検索が走っている（未検索の案内が出ない）')
})
if ((await page.getByLabel('テキスト条件 1 の値').inputValue()) !== 'ニュース') {
  ng.push('④ 前回の条件がフォームに戻らない')
}
if (searchBodies.length !== searchesBeforePlain) {
  ng.push(`④ 開いただけで検索を送っている（${searchBodies.length - searchesBeforePlain} 回）`)
}

log('\n=== ⑤ 録画詳細: 再生速度が別の録画でも <video>.playbackRate に効く ===')
// 今の UI で「別の録画に移る」を実際に起こせるのは一覧を経由する経路だけ
// （`RecordingDetailPage` は `/recordings` と別コンポーネントなので、一覧を
// 挟むと `RecordingPlayer` は毎回新規マウントし直される）。速度は
// localStorage（端末ごとに 1 つ）から新規マウント時に復元されるので、
// select・<video> のどちらも 1.5 のまま録画をまたぐことを実ブラウザで確かめる。
await page.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
await page.locator('video').waitFor({ timeout: 15000 })
await page.getByLabel('再生速度').selectOption('1.5')
const rateOnFirst = await page.locator('video').evaluate((v) => v.playbackRate)
if (rateOnFirst !== 1.5) {
  ng.push(`⑤ 速度を選んでも <video>.playbackRate に反映されない（${rateOnFirst}）`)
}

await page.getByRole('link', { name: '戻る' }).click()
await page.getByRole('link', { name: '録画 2' }).waitFor({ timeout: 15000 })
await page.getByRole('link', { name: '録画 2' }).click()
await page.locator('video').waitFor({ timeout: 15000 })
if ((await page.getByLabel('再生速度').inputValue()) !== '1.5') {
  ng.push('⑤ 別の録画に移ると再生速度セレクトが 1 倍に戻る（端末ごとの好みとして保たれていない）')
}
const rateOnSecond = await page.locator('video').evaluate((v) => v.playbackRate)
if (rateOnSecond !== 1.5) {
  ng.push(`⑤ 別の録画に移ると <video>.playbackRate が 1 倍に戻る（${rateOnSecond}）`)
}

await finish(ng, browser)
