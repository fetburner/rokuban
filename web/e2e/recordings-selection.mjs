// 録画一覧の編集モードと一括ごみ箱送り / Undo の実ブラウザ判定。
// checkbox と全面行リンクの競合、並列操作後の invalidate は jsdom の DOM 存在確認
// だけでは実際のクリック経路まで保証できないため、stateful API stub で往復を見る。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:recordings-selection
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

const recordings = Array.from({ length: 20 }, (_, i) => {
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
    title: id === 1 ? '一つ目の録画' : id === 20 ? '最後の録画' : `未選択の録画${id}`,
    startAt: new Date(Date.parse('2026-01-01T12:00:00Z') + id * 60_000).toISOString(),
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-02T12:30:00Z',
  }
})
let library = [...recordings]
let trash = []
const deletedIds = []
const restoredIds = []

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/encode-profiles' || path === '/api/rules') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/sites\/[^/]+\/services$/.test(path)) return json([])
  if (/^\/api\/recordings\/\d+\/thumbnail$/.test(path)) {
    return route.fulfill({ status: 404 })
  }
  if (path === '/api/recordings' && method === 'GET') {
    return json(url.searchParams.get('trash') === 'true' ? trash : library)
  }

  const deleting = /^\/api\/recordings\/(\d+)$/.exec(path)
  if (deleting && method === 'DELETE') {
    const id = Number(deleting[1])
    deletedIds.push(id)
    const recording = library.find((item) => item.id === id)
    library = library.filter((item) => item.id !== id)
    if (recording) trash.push({ ...recording, deletedAt: '2026-01-03T00:00:00Z' })
    return route.fulfill({ status: 204 })
  }

  const restoring = /^\/api\/recordings\/(\d+)\/restore$/.exec(path)
  if (restoring && method === 'POST') {
    const id = Number(restoring[1])
    restoredIds.push(id)
    const recording = trash.find((item) => item.id === id)
    trash = trash.filter((item) => item.id !== id)
    if (recording) {
      const { deletedAt: _deletedAt, ...restored } = recording
      library.push(restored)
    }
    return route.fulfill({ status: 204 })
  }

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
  viewport: { width: 390, height: 844 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)
await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
await page.getByText('一つ目の録画').waitFor({ timeout: 15000 })

log('\n=== ① 編集モードで全面リンクを外し、checkbox に到達できる ===')
await page.getByRole('button', { name: '選択' }).click()
const firstCheckbox = page.getByRole('checkbox', { name: '一つ目の録画を選択' })
await firstCheckbox.focus()
if (!(await firstCheckbox.evaluate((element) => element === document.activeElement))) {
  ng.push('① checkbox にフォーカスできない')
}
if (await page.getByRole('link', { name: '一つ目の録画' }).count()) {
  ng.push('① 編集モード中も全面行リンクが残っている')
}

await firstCheckbox.click()
// 最終行までスクロールして、固定バーの下に隠れず通常の click が完了することを判定する。
await page.getByRole('checkbox', { name: '最後の録画を選択' }).click()
if ((await page.getByText('2 件を選択中').count()) !== 1) {
  ng.push('① 2 件を選択しても選択件数が 2 にならない')
}
if (new URL(page.url()).pathname !== '/recordings') {
  ng.push(`① checkbox のクリックで詳細へ遷移した（${page.url()}）`)
}

log('\n=== ② 2 件をごみ箱へ送り、再取得後に一覧から消える ===')
await page.getByRole('button', { name: 'ごみ箱へ' }).click()
await page.getByText('2 件をごみ箱へ移動').waitFor({ timeout: 15000 }).catch(() => {
  ng.push('② 2 件の成功トーストが出ない')
})
await page.getByText('一つ目の録画').waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('② 一つ目の録画が一覧から消えない')
})
await page.getByText('最後の録画').waitFor({ state: 'detached', timeout: 15000 }).catch(() => {
  ng.push('② 最後の録画が一覧から消えない')
})
if ((await page.getByText('未選択の録画10').count()) !== 1) {
  ng.push('② 未選択の録画まで一覧から消えた')
}
if (deletedIds.toSorted((a, b) => a - b).join(',') !== '1,20') {
  ng.push(`② DELETE が選択 2 件すべてに届かない（${deletedIds.join(',')}）`)
}

log('\n=== ③ Undo で成功 2 件を復元し、一覧へ戻す ===')
await page.getByRole('button', { name: '元に戻す' }).click()
await page.getByText('一つ目の録画').waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ Undo 後に一つ目の録画が戻らない')
})
await page.getByText('最後の録画').waitFor({ timeout: 15000 }).catch(() => {
  ng.push('③ Undo 後に最後の録画が戻らない')
})
if (restoredIds.toSorted((a, b) => a - b).join(',') !== '1,20') {
  ng.push(`③ restore が成功 2 件すべてに届かない（${restoredIds.join(',')}）`)
}

await finish(ng, browser)
