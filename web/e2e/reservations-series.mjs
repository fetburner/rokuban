// 予約を実効シリーズでまとめる一覧の実ブラウザ判定。
// jsdom では測れない横スクロール、実際の操作標的、sticky header と展開行の位置を測る。
//
//   cd web && pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/reservations-series \
//     pnpm e2e:reservations-series

import { mkdirSync } from 'node:fs'
import path from 'node:path'

import {
  ListCapacityOveragesResponseItem,
  ListRecordingShelvesResponseItem,
  ListReservationsResponseItem,
  ListRulesResponseItem,
} from '../src/api/zod.ts'
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
const EVIDENCE_DIR = process.env.E2E_SHOT_DIR
if (EVIDENCE_DIR) mkdirSync(EVIDENCE_DIR, { recursive: true })
const ng = []
const FIXED_NOW = new Date('2026-10-02T12:00:00+09:00')
const STAMP = FIXED_NOW.toISOString()
const iso = (day, hour) => new Date(`2026-10-${String(day).padStart(2, '0')}T${String(hour).padStart(2, '0')}:00:00+09:00`).toISOString()

function reservation({ id, day, hour, title, series, site = 'default', state = 'active', skip = false, source = 'rule', ruleId = 8, durationMinutes = 45, dedupMatchRecordingId }) {
  return {
    site,
    programId: 9000 + id,
    source,
    ...(ruleId === undefined ? {} : { ruleId }),
    state,
    title,
    serviceName: site === 'default' ? 'ＮＨＫ総合１・東京' : '高松総合',
    channelType: 'GR',
    startAt: iso(day, hour),
    durationMs: durationMinutes * 60_000,
    createdAt: STAMP,
    updatedAt: STAMP,
    series,
    skip,
    ...(dedupMatchRecordingId === undefined ? {} : { dedupMatchRecordingId, dedupSimilarity: 0.91 }),
  }
}

function fixtures(multipleSites) {
  const reservations = [
    reservation({ id: 1, day: 2, hour: 18, title: '金曜アニメ 第1話', series: '金曜アニメ', skip: true, dedupMatchRecordingId: 71 }),
    reservation({ id: 2, day: 2, hour: 20, title: '金曜アニメ 第2話', series: '金曜アニメ' }),
    reservation({ id: 3, day: 3, hour: 20, title: '金曜アニメ 第3話', series: '金曜アニメ', state: 'detached' }),
    reservation({ id: 4, day: 4, hour: 21, title: 'ひとり予約', series: '録画の無いシリーズ', source: 'manual', ruleId: undefined }),
    reservation({ id: 5, day: 1, hour: 20, title: '録画されなかった番組', series: null, state: 'orphaned', source: 'manual', ruleId: undefined }),
    reservation({ id: 6, day: 6, hour: 23, title: '要確認外の番組', series: '別のシリーズ', source: 'manual', ruleId: undefined }),
    reservation({ id: 8, day: 7, hour: 18, title: '視聴済みシリーズ 最終話', series: '視聴済みシリーズ', source: 'manual', ruleId: undefined }),
  ]
  if (multipleSites) {
    // 同じ放送を 2 サイトで予約した 2 件。件数は予約件数として数える。
    reservations.push(
      reservation({ id: 7, day: 2, hour: 20, title: '金曜アニメ 第2話', series: '金曜アニメ', site: 'takamatsu' }),
    )
  }
  const overages = [
    {
      site: 'default',
      startAt: iso(3, 20),
      endAt: iso(3, 21),
      shortfall: 1,
      jammedTypes: ['BS'],
    },
  ]
  const shelves = [
    {
      value: '金曜アニメ',
      title: '金曜アニメ 第3話',
      count: 12,
      playableCount: 10,
      unwatchedCount: 4,
      latestStartAt: iso(1, 20),
      representativeId: 701,
    },
    {
      value: '視聴済みシリーズ',
      title: '視聴済みシリーズ 最終話',
      count: 2,
      playableCount: 2,
      unwatchedCount: 0,
      latestStartAt: iso(1, 21),
      representativeId: 702,
    },
    // null の棚は null series の予約と照合してはいけない。
    {
      value: null,
      title: '対応するシリーズなし',
      count: 99,
      playableCount: 99,
      unwatchedCount: 99,
      latestStartAt: iso(1, 22),
      representativeId: 799,
    },
  ]
  const rules = [{
    id: 8,
    name: '金曜アニメ',
    enabled: true,
    priority: 10,
    keepOriginal: 'always',
    createdAt: STAMP,
    updatedAt: STAMP,
  }]
  return { reservations, overages, shelves, rules }
}

async function validateAll(fixturesForRun) {
  await validateFixturesOrExit(
    [
      ...fixturesForRun.reservations.map((item, index) => [`reservations[${index}]`, ListReservationsResponseItem, item]),
      ...fixturesForRun.overages.map((item, index) => [`overages[${index}]`, ListCapacityOveragesResponseItem, item]),
      ...fixturesForRun.shelves.map((item, index) => [`shelves[${index}]`, ListRecordingShelvesResponseItem, item]),
      ...fixturesForRun.rules.map((item, index) => [`rules[${index}]`, ListRulesResponseItem, item]),
    ],
    ng,
  )
}

async function apiHandler({ path: requestPath, json, route }, data) {
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/sites') return json(data.multipleSites ? ['default', 'takamatsu'] : ['default'])
  if (requestPath === '/api/capabilities') return json({ live: true })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/reservations') return json(data.reservations)
  if (requestPath === '/api/rules') return json(data.rules)
  if (requestPath === '/api/capacity/overages') return json(data.overages)
  if (requestPath === '/api/recording-shelves') return json(data.shelves)
  if (requestPath === '/api/storage') return json([])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  return json([])
}

async function screenshot(page, name, fullPage = true) {
  if (!EVIDENCE_DIR) return
  await page.screenshot({ path: path.join(EVIDENCE_DIR, name), fullPage, animations: 'disabled' })
  log(`  screenshot: ${path.join(EVIDENCE_DIR, name)}`)
}

async function openPage(browser, width, theme, multipleSites) {
  const data = fixtures(multipleSites)
  const context = await browser.newContext({
    viewport: { width, height: 820 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: theme,
    isMobile: width <= 390,
    hasTouch: width <= 390,
  })
  const page = await context.newPage()
  await page.clock.install({ time: FIXED_NOW })
  await installApiStubs(page, (args) => apiHandler(args, { ...data, multipleSites }))
  await page.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })
  return { context, page, data }
}

async function checkPage(browser, width, theme, multipleSites, saveShot) {
  const { context, page } = await openPage(browser, width, theme, multipleSites)
  const label = `${width}px/${theme}/${multipleSites ? 'multi' : 'single'}`
  const toggle = page.getByRole('group', { name: '予約のまとめ方' })
  if (await toggle.count() === 0) {
    ng.push(`${label}: 予約のシリーズ/時間順トグルが無い`)
    await context.close()
    return
  }
  const seriesButton = toggle.getByRole('button', { name: 'シリーズ' })
  const timeButton = toggle.getByRole('button', { name: '時間順' })
  if (!(await seriesButton.getAttribute('aria-pressed')) || await seriesButton.getAttribute('aria-pressed') !== 'true') {
    ng.push(`${label}: 初期表示がシリーズになっていない`)
  }

  const seriesRow = page.locator('[data-testid="reservation-series-row"]').filter({ hasText: '金曜アニメ' }).first()
  await seriesRow.waitFor({ timeout: 15000 }).catch(() => ng.push(`${label}: 金曜アニメのシリーズ行が無い`))
  if (await seriesRow.count() === 0) {
    await context.close()
    return
  }
  const title = seriesRow.locator('[data-testid="reservation-series-title"]')
  const titleText = await title.innerText()
  if (titleText !== '金曜アニメ') ng.push(`${label}: シリーズ名が不正 (${titleText})`)
  if (!(await seriesRow.getByText(/20:00/).count()) || await seriesRow.getByText(/18:00/).count()) {
    ng.push(`${label}: 次回が skip を飛ばしていない`)
  }
  const expectedCount = multipleSites ? '今後 4 本' : '今後 3 本'
  if (!(await seriesRow.getByText(expectedCount, { exact: true }).count())) {
    ng.push(`${label}: 絞り込み前の予約件数が不正 (${expectedCount})`)
  }
  // 空箱の文言は、棚の無いシリーズの行の中で数える（シリーズ名と同じ文字列にしない）。
  // デスクトップ幅は破線の空枠、モバイル幅はメタ行の文言だけが見える。
  const noShelfRow = page.locator('[data-testid="reservation-series-row"][data-series-value="録画の無いシリーズ"]')
  const emptyBox = noShelfRow.getByTestId('reservation-recording-shelf-empty')
  const visibleEmptyText = noShelfRow.getByText('まだ録画なし', { exact: true }).locator('visible=true')
  if ((await visibleEmptyText.count()) !== 1) {
    ng.push(`${label}: 録画棚が無いシリーズの「まだ録画なし」が 1 つ見えていない`)
  }
  if (width >= 1024 && !(await emptyBox.getByText('まだ録画なし', { exact: true }).isVisible())) {
    ng.push(`${label}: デスクトップ幅の空箱に「まだ録画なし」が無い`)
  }
  if ((await emptyBox.isVisible()) !== (width >= 1024)) {
    ng.push(`${label}: 空箱の表示がデスクトップ幅だけになっていない`)
  }
  if (await noShelfRow.getByRole('link', { name: /番組ハブ/ }).count()) {
    ng.push(`${label}: 録画の無いシリーズが番組ハブへリンクしている`)
  }
  if (await page.getByText(/99 本|未視聴 99/).count()) {
    ng.push(`${label}: value=null の棚が null series へ誤って結合された`)
  }
  const watchedRow = page.locator('[data-testid="reservation-series-row"][data-series-value="視聴済みシリーズ"]')
  if (!(await watchedRow.getByRole('link', { name: /すべて視聴済み/ }).count())) {
    ng.push(`${label}: 未視聴 0 件をすべて視聴済みと表示しない`)
  }

  const expand = seriesRow.getByRole('button', { name: /金曜アニメ/ })
  // 開閉できる行の右端は ∨、遷移する行は ›。形で見分けられること。
  if (!(await seriesRow.locator('svg.lucide-chevron-down').count()) || await seriesRow.locator('svg.lucide-chevron-right').count()) {
    ng.push(`${label}: 開閉できる行の右端の印が ∨ でない（遷移の › と区別できない）`)
  }
  if (!(await noShelfRow.locator('svg.lucide-chevron-right').count()) || await noShelfRow.locator('svg.lucide-chevron-down').count()) {
    ng.push(`${label}: 1 本だけの行の右端の印が › でない`)
  }
  const target = await expand.boundingBox()
  if (!target || target.width < 44 || target.height < 44) {
    ng.push(`${label}: シリーズ行の主操作が 44x44px 未満 (${JSON.stringify(target)})`)
  }
  const hub = seriesRow.getByRole('link', { name: /録画 12 本/ })
  const hubBox = await hub.boundingBox()
  if (!hubBox || hubBox.width < 24 || hubBox.height < 24) {
    ng.push(`${label}: 番組ハブへの導線が 24x24px 未満 (${JSON.stringify(hubBox)})`)
  }
  if ((await hub.getAttribute('href')) !== '/recordings/701/series') {
    ng.push(`${label}: ハブの宛先が代表録画 ID でない`)
  }
  const origin = seriesRow.getByRole('link', { name: 'ルール「金曜アニメ」' })
  const originBox = await origin.boundingBox()
  if (!originBox || originBox.width < 24 || originBox.height < 24) {
    ng.push(`${label}: 出自リンクが 24x24px 未満 (${JSON.stringify(originBox)})`)
  }
  if ((await origin.getAttribute('href')) !== '/search?ruleId=8') {
    ng.push(`${label}: 出自リンクの宛先が不正`)
  }
  const capacity = seriesRow.getByRole('link', { name: /該当する予約 1 件/ })

  const hitTarget = async (locator) => locator.evaluate((el) => {
    const rect = el.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
    return hit !== null && (hit === el || el.contains(hit))
  })
  if (!(await hitTarget(hub))) ng.push(`${label}: 番組ハブの hit-test を行本体が奪っている`)
  // 開閉ボタンは行全体に広がる。タイトル文字の中心を押したとき、ボタン自身が hit target になること。
  const titleBox = await title.boundingBox()
  const expandHit = titleBox && await expand.evaluate((el, p) => {
    const hit = document.elementFromPoint(p.x, p.y)
    return hit !== null && (hit === el || el.contains(hit))
  }, { x: titleBox.x + titleBox.width / 2, y: titleBox.y + titleBox.height / 2 })
  if (!expandHit) ng.push(`${label}: タイトル位置の hit-test が開閉ボタンでない（行本体を押しても開閉しない）`)
  if (!(await hitTarget(origin))) ng.push(`${label}: 出自リンクの hit-test を行本体が奪っている`)
  if ((await capacity.count()) && !(await hitTarget(capacity))) {
    ng.push(`${label}: 容量不足バッジの hit-test を行本体が奪っている`)
  }

  if (saveShot) await screenshot(page, `${width}-series-${multipleSites ? 'multi' : 'single'}-${theme}.png`)

  // 手前に出ていない要素は Playwright の click が既定 30 秒待って例外で落ちる。
  // 文言まで辿り着くよう短い timeout で NG に変え、遷移できなかったときは戻らない
  // （戻ると一覧ごと離れてしまう）。
  const followAndReturn = async (locator, pattern, failure, inspect) => {
    const clicked = await locator.click({ timeout: 2000 }).then(() => true, () => false)
    const arrived = clicked && (await page.waitForURL(pattern, { timeout: 3000 }).then(() => true, () => false))
    if (!arrived) {
      ng.push(`${label}: ${failure}`)
      return
    }
    inspect?.()
    await page.goBack()
    await seriesRow.waitFor({ timeout: 3000 }).catch(() => ng.push(`${label}: 予約一覧へ戻れない`))
  }
  await followAndReturn(hub, /\/recordings\/701\/series/, '番組ハブへのリンクが行本体から分離されていない')
  if (await capacity.count()) {
    await followAndReturn(capacity, /\/programs\?/, '容量不足バッジから番組表へ移れない', () => {
      const capacitySearch = new URL(page.url()).searchParams
      if (capacitySearch.get('view') !== 'grid' || !capacitySearch.has('at')) {
        ng.push(`${label}: 容量不足バッジの番組表宛先に grid/at が無い`)
      }
    })
  }
  await followAndReturn(
    page.getByRole('link', { name: 'ルール「金曜アニメ」' }).first(),
    /\/search\?ruleId=8/,
    '出自リンクが行本体から分離されていない',
  )

  // 見出しをページヘッダーの下へ 24px だけ潜らせてから開く（行の下側はまだ見えている）。
  // 開いた後に見出しがヘッダーの下へ戻らなければ、何を開いたか見失う。
  // 本文が短いとスクロールできないので、下に余白を足して必ず動けるようにする。
  // click() は Playwright が自分でスクロールし直すので、要素の click を直接呼ぶ。
  await page.evaluate(() => { document.body.style.paddingBottom = '2000px' })
  await title.evaluate((el) => {
    const header = document.querySelector('header')
    const offset = header?.getBoundingClientRect().bottom ?? 0
    window.scrollTo(0, window.scrollY + el.getBoundingClientRect().top - offset + 24)
  })
  await page.waitForTimeout(80)
  const hiddenTop = await title.evaluate((el) => el.getBoundingClientRect().top)
  const hiddenHeaderBottom = await page.locator('header').evaluate((el) => el.getBoundingClientRect().bottom)
  if (hiddenTop >= hiddenHeaderBottom) ng.push(`${label}: 前提を作れない（見出しがヘッダーの下に潜っていない）`)
  await expand.evaluate((el) => el.click())
  const episodeList = seriesRow.locator('ul[aria-label="金曜アニメの予約"]')
  await episodeList.waitFor({ timeout: 3000 }).catch(() => {
    ng.push(`${label}: 展開した各回の詳細が無い`)
  })
  if (!(await episodeList.getByText('第2話', { exact: true }).count())) {
    ng.push(`${label}: 展開した各回の題名が無い`)
  }
  if (!(await episodeList.getByText('条件外', { exact: true }).count())) {
    ng.push(`${label}: 次回以外のルール条件外予約に出自が表示されない`)
  }
  const episodeLinks = episodeList.locator(':scope > li > a.absolute')
  for (let index = 0; index < await episodeLinks.count(); index += 1) {
    const box = await episodeLinks.nth(index).boundingBox()
    if (!box || box.width < 44 || box.height < 44) {
      ng.push(`${label}: 展開した各回の予約詳細リンクが 44x44px 未満 (${JSON.stringify(box)})`)
    }
  }
  if (multipleSites && !(await seriesRow.getByText('takamatsu', { exact: true }).count())) {
    ng.push(`${label}: 複数サイトの各回に site が出ない`)
  }
  if (!multipleSites && await seriesRow.getByText('default', { exact: true }).count()) {
    ng.push(`${label}: 単一サイトなのに site が表示されている`)
  }

  const top = await title.evaluate((el) => el.getBoundingClientRect().top)
  const headerBottom = await page.locator('header').evaluate((el) => el.getBoundingClientRect().bottom)
  if (top < headerBottom - 1) ng.push(`${label}: 展開行の見出しがページヘッダーに隠れる (${top}px < ${headerBottom}px)`)

  const dimensions = await page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    page: document.documentElement.scrollWidth,
  }))
  if (dimensions.page > dimensions.viewport) {
    ng.push(`${label}: ページが横スクロールする (${dimensions.page}px > ${dimensions.viewport}px)`)
  }

  // 判定用の余白をスクリーンショットに写さない。
  await page.evaluate(() => { document.body.style.paddingBottom = '' })
  if (saveShot) await screenshot(page, `${width}-expanded-${multipleSites ? 'multi' : 'single'}-${theme}.png`)

  // 表示形式の好みだけが localStorage に残り、絞り込みの URL は変更しない。
  await timeButton.click()
  if (await timeButton.getAttribute('aria-pressed') !== 'true') ng.push(`${label}: 時間順に切り替わらない`)
  if (saveShot && width === 360 && !multipleSites && theme === 'light') {
    await screenshot(page, '360-time-single-light.png')
  }
  const stored = await page.evaluate(() => localStorage.getItem('rokuban:reservations:group'))
  if (stored !== 'time') ng.push(`${label}: 表示設定が localStorage に保存されない (${stored})`)
  if (new URL(page.url()).search !== '') ng.push(`${label}: 表示形式の変更が URL に混ざる`)
  if (await page.locator('[data-testid="reservation-series-row"]').count()) {
    ng.push(`${label}: 時間順でシリーズのグループが残っている`)
  }
  if (!(await page.locator('li.relative:has([data-testid="reservation-secondary"])').count())) {
    ng.push(`${label}: 時間順で既存の予約一覧が表示されない`)
  }
  await context.close()
}

async function checkHomeWarning(browser, width) {
  const data = fixtures(false)
  const context = await browser.newContext({
    viewport: { width, height: 820 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
    isMobile: width <= 390,
    hasTouch: width <= 390,
  })
  const page = await context.newPage()
  await page.clock.install({ time: FIXED_NOW })
  await installApiStubs(page, (args) => apiHandler(args, data))
  await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
  const warning = page.locator('[data-warning-kind="not-recorded"]')
  await warning.waitFor({ timeout: 15_000 }).catch(() => {
    ng.push(`home/${width}px: orphaned 予約の「録画されず」警告が無い`)
  })
  if (!(await warning.getByText('録画されず', { exact: true }).count())) {
    ng.push(`home/${width}px: 録画されずの種別チップが表示されない`)
  }
  const warningLink = warning.getByRole('link')
  if ((await warningLink.getAttribute('href').catch(() => null)) !== '/reservations/default/9005') {
    ng.push(`home/${width}px: 録画されず警告が予約詳細へリンクしない`)
  }
  await screenshot(page, `home-${width}-not-recorded-light.png`)
  await context.close()
}

log(`URL: ${URL_BASE}`)
for (const multipleSites of [false, true]) await validateAll(fixtures(multipleSites))
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
for (const width of [360, 390, 1280]) {
  await checkPage(browser, width, 'light', false, width !== 390)
}
for (const width of [360, 1280]) await checkPage(browser, width, 'light', true, true)
for (const [width, multipleSites] of [[360, false], [1280, false], [360, true], [1280, true]]) {
  await checkPage(browser, width, 'dark', multipleSites, true)
}
for (const width of [360, 1280]) await checkHomeWarning(browser, width)

// 要確認とルールの両条件を先に適用し、その後のシリーズ件数・バッジが残った集合
// だけで再計算されることを確かめる（モック 4）。
{
  const { context, page } = await openPage(browser, 1280, 'light', true)
  await page.goto(URL_BASE + '/reservations?only=attention&ruleId=8', { waitUntil: 'domcontentloaded' })
  const filtered = page.locator('[data-testid="reservation-series-row"]').filter({ hasText: '金曜アニメ' })
  await filtered.waitFor({ timeout: 15000 }).catch(() => ng.push('filter: 絞り込み後のシリーズ行が無い'))
  if (!(await filtered.getByText('今後 1 本', { exact: true }).count())) {
    ng.push('filter: 今後 N 本が絞り込み後の予約数にならない')
  }
  if (!(await filtered.getByRole('link', { name: /この時間帯はチューナーが不足しています.*該当する予約 1 件/ }).count())) {
    ng.push('filter: 絞り込み後の予約結論（不足時間帯）が表示されない')
  }
  if (!(await filtered.getByText('容量不足 1', { exact: true }).count())) {
    ng.push('filter: 絞り込み後の容量不足件数が不正')
  }
  if (await filtered.getByText(/録画しない（重複）/).count()) {
    ng.push('filter: 絞り込みで外れた重複スキップが件数に残る')
  }
  if (await filtered.getByRole('button', { name: /予約を開く/ }).count()) {
    ng.push('filter: 1 件に絞られた行が開閉ボタンのまま')
  }
  const filteredDetailLink = filtered.locator(':scope > div > a.absolute')
  if ((await filteredDetailLink.getAttribute('href')) !== '/reservations/default/9003') {
    ng.push('filter: 1 件だけのシリーズ行が次回の予約詳細へリンクしない')
  }
  const filteredDetailBox = await filteredDetailLink.boundingBox()
  if (!filteredDetailBox || filteredDetailBox.width < 44 || filteredDetailBox.height < 44) {
    ng.push(`filter: 単件の予約詳細リンクが 44x44px 未満 (${JSON.stringify(filteredDetailBox)})`)
  }
  await screenshot(page, '1280-filtered-attention-rule-light.png')
  await context.close()
}

await finish(ng, browser)
