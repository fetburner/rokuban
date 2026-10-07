// 録画一覧のツールバー（検索・期間・絞り込み・並び順）の実ブラウザ判定。
//
// 1 行に並ぶか（Y 座標）・ページが横に溢れないか・パネルがシートとポップオーバーの
// どちらで出るか（位置と高さ）は、jsdom がレイアウトを計算しないので `pnpm test` では
// 測れない。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:recordings-period-toolbar
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

const recording = {
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '期間判定用の録画',
  startAt: '2026-08-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: '2026-08-01T12:30:00Z',
}

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json(['default'])
  if (path === '/api/capabilities') return json({ live: true })
  if (path === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(path)) return route.fulfill({ status: 404 })
  if (path === '/api/recordings') return json([recording])
  return json([])
}

// 「2026 夏」+ ジャンル。期間を指定した状態でも 1 行に収まるかを見る。
const SUMMER = '?from=2026-06-30T15%3A00%3A00.000Z&to=2026-09-30T15%3A00%3A00.000Z&genre=7'

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

/** open は時計を 2026-10-07（水）12:00 JST に固定して録画一覧を開く。 */
async function open(viewport, query = '') {
  const context = await browser.newContext({ viewport, locale: 'ja-JP', timezoneId: 'Asia/Tokyo' })
  const page = await context.newPage()
  page.on('pageerror', (e) => ng.push(`pageerror: ${e.message}`))
  await page.clock.setFixedTime(new Date('2026-10-07T12:00:00+09:00'))
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + '/recordings' + query, { waitUntil: 'domcontentloaded' })
  await page.getByText('期間判定用の録画').waitFor({ timeout: 15000 })
  return { context, page }
}

/** controls はツールバーの 4 つの操作。期間ボタンの名前は今の期間で変わる。 */
function controls(page, periodName) {
  return {
    検索: page.getByRole('searchbox', { name: '番組名・説明で検索' }),
    期間: page.getByRole('button', { name: periodName, exact: true }),
    絞り込み: page.getByRole('button', { name: '絞り込み', exact: true }),
    並び順: page.getByRole('combobox', { name: '並び順' }),
  }
}

/** present は要素が 1 つあるかを返し、無ければ NG を記録する（無い要素を待って止まらない）。 */
async function present(locator, label) {
  if ((await locator.count()) === 1) return true
  ng.push(`${label} が無い`)
  return false
}

const documentOverflow = (page) =>
  page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)

/**
 * reachable は要素をスクロールして出したうえで、その中心のヒットテストが本当に
 * その要素に当たるかを返す（recordings-rule-filter.mjs と同じ判定）。
 */
async function reachable(locator) {
  await locator.scrollIntoViewIfNeeded()
  return locator.evaluate((el) => {
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) return false
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
    return hit !== null && (hit === el || el.contains(hit) || hit.contains(el))
  })
}

log('\n=== ① 360px / 390px: 検索欄と 3 つのボタンが 1 行に並ぶ（期間の指定の有無とも） ===')
for (const width of [360, 390]) {
  for (const [query, periodName] of [
    ['', '期間'],
    [SUMMER, '2026 夏'],
  ]) {
    const label = `① ${width}px ${query === '' ? '条件なし' : '2026 夏'}`
    const { context, page } = await open({ width, height: 800 }, query)
    const boxes = {}
    for (const [name, locator] of Object.entries(controls(page, periodName))) {
      boxes[name] = (await present(locator, `${label}: ${name}`)) ? await locator.boundingBox() : null
    }
    const centers = Object.values(boxes)
      .filter((b) => b !== null)
      .map((b) => b.y + b.height / 2)
    const spread = Math.max(...centers) - Math.min(...centers)
    log(`  ${label}: 中心 Y = ${centers.map((y) => y.toFixed(1)).join(' / ')}`)
    if (centers.length !== 4 || spread > 1) ng.push(`${label}: 4 つの中心 Y が揃わない（差 ${spread.toFixed(1)}px）`)
    for (const name of ['期間', '絞り込み', '並び順']) {
      const b = boxes[name]
      if (b !== null && (b.width < 44 || b.height < 44)) {
        ng.push(`${label}: ${name} の押せる領域が 44×44px 未満（${b.width}×${b.height}）`)
      }
    }
    // 期間のチップは md 未満でだけ出る（ボタンが中身を出さないため）。
    if (query !== '' && !(await page.getByRole('button', { name: '期間: 2026 夏' }).isVisible())) {
      ng.push(`${label}: 期間のチップが出ない`)
    }
    const overflow = await documentOverflow(page)
    if (overflow > 0) ng.push(`${label}: ページが横に ${overflow}px 溢れた`)
    await context.close()
  }
}

log('\n=== ② 390px: 期間と絞り込みが下からのシートで開き、末尾までシート内で届く ===')
{
  // 844px では中身が 85% に収まりスクロールが起きない。スクロールの経路を通すため低くする。
  const height = 560
  const { context, page } = await open({ width: 390, height }, SUMMER)
  for (const [buttonName, title, last] of [
    ['2026 夏', '期間', (dialog) => dialog.getByLabel('終了日（この日を含む）')],
    ['絞り込み', '絞り込み', (dialog) => dialog.getByRole('group', { name: '種別' })],
  ]) {
    const label = `② ${title}`
    const trigger = page.getByRole('button', { name: buttonName, exact: true })
    if (!(await present(trigger, `${label}: ボタン`))) continue
    await trigger.click()
    const dialog = page.getByRole('dialog', { name: title })
    await dialog.waitFor({ timeout: 15000 })
    // 開くアニメーションが無いので位置は即座に確定する。
    const box = await dialog.boundingBox()
    log(`  ${label}: top=${box.y.toFixed(1)} bottom=${(box.y + box.height).toFixed(1)} height=${box.height.toFixed(1)}`)
    if (Math.abs(box.y + box.height - height) > 1) ng.push(`${label}: 画面の下端に付いていない（bottom=${box.y + box.height}）`)
    if (box.x !== 0 || Math.abs(box.width - 390) > 1) ng.push(`${label}: 横幅いっぱいでない（x=${box.x} width=${box.width}）`)
    if (box.height > height * 0.85 + 1) ng.push(`${label}: 高さが画面の 85% を超えた（${box.height}px）`)
    const body = dialog.getByTestId('toolbar-sheet-body')
    if (await present(body, `${label}: シートの本体`)) {
      const scroll = await body.evaluate((el) => ({ scrollHeight: el.scrollHeight, clientHeight: el.clientHeight }))
      if (scroll.scrollHeight <= scroll.clientHeight) {
        ng.push(`${label}: シートの中身がスクロールしていない（判定が空虚になる。${JSON.stringify(scroll)}）`)
      }
      if (!(await reachable(last(dialog)))) ng.push(`${label}: 末尾の要素にシート内のスクロールで届かない`)
      if ((await body.evaluate((el) => el.scrollTop)) === 0) ng.push(`${label}: 末尾へ届くのにシート内がスクロールしていない`)
    }
    if (!(await dialog.getByRole('button', { name: '完了' }).first().isVisible())) {
      ng.push(`${label}: 「完了」が見えない`)
    }
    const overflow = await documentOverflow(page)
    if (overflow > 0) ng.push(`${label}: ページが横に ${overflow}px 溢れた`)
    await page.keyboard.press('Escape')
    await dialog.waitFor({ state: 'detached', timeout: 15000 }).catch(() => ng.push(`${label}: Escape で閉じない`))
  }

  // シートの中で前クールを押すと URL の from/to が変わり、ジャンルは保たれ、シートが閉じる。
  if (!(await present(page.getByRole('button', { name: '2026 夏', exact: true }), '② 期間のボタン'))) {
    await context.close()
    await finish(ng, browser)
  }
  await page.getByRole('button', { name: '2026 夏', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '期間' })
  await dialog.getByRole('button', { name: /^前クール/ }).waitFor({ timeout: 15000 })
  await dialog.getByRole('button', { name: '前の年' }).click()
  await dialog.getByRole('group', { name: '2025 年のクール' }).getByRole('button', { name: '秋' }).click()
  await dialog.waitFor({ state: 'detached', timeout: 15000 }).catch(() => ng.push('② クールを押してもシートが閉じない'))
  const params = new URL(page.url()).searchParams
  if (params.get('from') !== '2025-09-30T15:00:00.000Z' || params.get('to') !== '2025-12-31T15:00:00.000Z') {
    ng.push(`② 2025 秋を選んだ URL が違う（from=${params.get('from')} to=${params.get('to')}）`)
  }
  // TanStack Router は配列を JSON で直列化する（`genre=[7]`）。
  if (params.get('genre') !== '[7]') ng.push(`② 期間を選ぶとジャンルが落ちた（genre=${params.get('genre')}）`)
  await context.close()
}

log('\n=== ③ 1280px: 期間はボタンに出てポップオーバーで開き、並び順は同じ形のボタン ===')
{
  const { context, page } = await open({ width: 1280, height: 800 }, SUMMER)
  const c = controls(page, '2026 夏')
  const trigger = await c.期間.boundingBox()
  if (trigger === null || !(await c.期間.getByText('2026 夏').isVisible())) ng.push('③ 期間ボタンに「2026 夏」が見えない')
  if (await page.getByRole('button', { name: '期間: 2026 夏' }).isVisible()) {
    ng.push('③ md 以上で期間のチップが出ている（ボタンと二重）')
  }
  const filter = await c.絞り込み.boundingBox()
  // select は枠の内側に透明に重ねてあるので、見えているボタン（親の label）を測る。
  const order = await c.並び順.locator('..').boundingBox()
  log(`  期間 ${trigger?.width}×${trigger?.height} / 絞り込み ${filter?.width}×${filter?.height} / 並び順 ${order?.width}×${order?.height}`)
  if (filter === null || order === null || Math.abs(filter.height - order.height) > 0.5 || Math.abs(filter.y - order.y) > 0.5) {
    ng.push('③ 並び順の高さ・位置が絞り込みボタンと揃わない')
  }
  const orderLook = await c.並び順.evaluate((select) => {
    const box = select.parentElement
    const cs = getComputedStyle(box)
    return { text: box.textContent, border: cs.borderTopWidth, chevron: box.querySelector('svg.lucide-chevron-down') !== null }
  })
  const filterLook = await c.絞り込み.evaluate((el) => ({
    border: getComputedStyle(el).borderTopWidth,
    chevron: el.querySelector('svg.lucide-chevron-down') !== null,
  }))
  if (!orderLook.text.includes('新しい順') || orderLook.border !== filterLook.border || !orderLook.chevron || !filterLook.chevron) {
    ng.push(`③ 並び順が絞り込みと同じ形のボタンでない（${JSON.stringify({ orderLook, filterLook })}）`)
  }

  await c.期間.click()
  const dialog = page.getByRole('dialog', { name: '期間' })
  await dialog.waitFor({ timeout: 15000 })
  const box = await dialog.boundingBox()
  log(`  期間ポップオーバー: top=${box.y.toFixed(1)} left=${box.x.toFixed(1)} width=${box.width.toFixed(1)}`)
  if (trigger !== null && (box.y < trigger.y + trigger.height || box.y > trigger.y + trigger.height + 20)) {
    ng.push(`③ 期間がトリガーの直下のポップオーバーで開かない（top=${box.y}）`)
  }
  if (box.width > 400) ng.push(`③ 期間のポップオーバーが画面幅のシートになっている（width=${box.width}）`)
  await page.keyboard.press('Escape')

  await c.絞り込み.click()
  const panel = page.getByRole('dialog', { name: '絞り込み' })
  await panel.waitFor({ timeout: 15000 })
  const panelBox = await panel.boundingBox()
  if (filter !== null && (panelBox.y < filter.y + filter.height || panelBox.width > 400)) {
    ng.push(`③ 絞り込みがトリガーの直下のポップオーバーで開かない（top=${panelBox.y} width=${panelBox.width}）`)
  }
  if ((await panel.getByText('期間（番組開始時刻）').count()) > 0) ng.push('③ 絞り込みパネルに期間の節が残っている')
  await context.close()
}

await finish(ng, browser)
