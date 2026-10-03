// PageHeader が録画・予約の一覧行にある z-10 の操作より手前に描画されることを
// 実ブラウザで確認する（issue #1078）。jsdom では stacking context と hit testing
// を測れないため、capacity badge をヘッダーの位置までスクロールして判定する。
// 録画一覧も 400px / デスクトップ幅でスクロールし、sticky header と追っかけ導線を確認する。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:header-stacking
import {
  ListCapacityOveragesResponseItem,
  ListCircuitBreakersResponseItem,
  ListRecordingsResponseItem,
  ListReservationsResponseItem,
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
const SITE = 'default'
const HOUR = 3_600_000
const baseMs = Date.parse('2026-01-01T12:00:00Z')
const iso = (ms) => new Date(ms).toISOString()
const ng = []

const reservations = Array.from({ length: 24 }, (_, i) => {
  const startAt = baseMs + HOUR + i * (HOUR / 2)
  return {
    id: i + 1,
    site: SITE,
    programId: 9001 + i,
    source: 'manual',
    state: 'active',
    title: `重なり判定の予約 ${i + 1}`,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: iso(startAt),
    durationMs: HOUR / 2,
    createdAt: iso(baseMs),
    updatedAt: iso(baseMs),
    series: null,
    skip: false,
  }
})

const overages = [
  {
    site: SITE,
    startAt: iso(baseMs + HOUR),
    endAt: iso(baseMs + 14 * HOUR),
    shortfall: 1,
    jammedTypes: ['BS'],
  },
]

const breakers = [
  {
    site: SITE,
    name: 'ruler_deletes',
    trippedAt: iso(baseMs),
    pending: 1,
    threshold: 20,
    detail: { total: 1, programs: [] },
  },
]

const recordings = Array.from({ length: 24 }, (_, i) => ({
  id: i + 1,
  site: SITE,
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: i + 1,
  title: `一覧スクロール確認 ${i + 1}`,
  startAt: iso(baseMs + i * 60_000),
  durationMs: HOUR,
  status: i === 0 ? 'recording' : 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  createdAt: iso(baseMs + HOUR),
}))

async function apiHandler({ path, json, route }) {
  if (path === '/api/sites') return json([SITE])
  if (path === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (path === '/api/breakers') return json(breakers)
  if (path === '/api/reservations') return json(reservations)
  if (path === '/api/capacity/overages') return json(overages)
  if (path === '/api/recordings' && route.request().method() === 'GET') return json(recordings)
  if (path === '/api/events') return sseKeepAlive(route)
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(path)) {
    return route.fulfill({ status: 404 })
  }
  return json([])
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ...reservations.map((item, i) => [`reservations[${i}]`, ListReservationsResponseItem, item]),
    ...recordings.map((item, i) => [`recordings[${i}]`, ListRecordingsResponseItem, item]),
    ['overages[0]', ListCapacityOveragesResponseItem, overages[0]],
    ...breakers.map((item, i) => [`breakers[${i}]`, ListCircuitBreakersResponseItem, item]),
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

async function openPage(width) {
  const context = await browser.newContext({
    viewport: { width, height: 800 },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  return { context, page }
}

for (const width of [400, 1280]) {
  const { context, page } = await openPage(width)
  log(`\n=== ① ${width}px: 予約行の z-10 バッジが sticky header に隠れる ===`)
  await page.goto(URL_BASE + '/reservations', { waitUntil: 'domcontentloaded' })
  const badge = page.locator('a.relative.z-10').first()
  await badge.waitFor({ timeout: 15000 }).catch(() => {
    ng.push(`① ${width}px: 容量不足バッジが表示されない`)
  })
  const breakerBanner = page.locator('[role="alert"]', { hasText: '削除が保留されています' })
  await breakerBanner.waitFor({ timeout: 15000 }).catch(() => {
    ng.push(`① ${width}px: サーキットブレーカー帯が表示されない`)
  })

  // 予約行は isolate で z-10 を閉じ込めており、そのままでは PageHeader が z-10 でも
  // 通ってしまう（空虚な成功）。行の isolate を外し、行内 z-10 と素で競合させて測る。
  await page.evaluate(() => {
    for (const li of document.querySelectorAll('li.isolate')) li.classList.remove('isolate')
  })
  const setup = await page.evaluate(() => {
    const header = Array.from(document.querySelectorAll('header')).find(
      (el) => el.querySelector('h1')?.textContent?.trim() === '予約',
    )
    const badge = document.querySelector('a.relative.z-10')
    if (!header || !badge) return null
    const headerBox = header.getBoundingClientRect()
    const badgeBox = badge.getBoundingClientRect()
    return {
      x: badgeBox.left + badgeBox.width / 2,
      targetY: headerBox.top + headerBox.height / 2,
      scrollBy: badgeBox.top + badgeBox.height / 2 - (headerBox.top + headerBox.height / 2),
    }
  })
  if (!setup) {
    ng.push(`① ${width}px: header または容量不足バッジの矩形が取れない`)
  } else {
    await page.evaluate((amount) => window.scrollBy(0, amount), setup.scrollBy)
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    const result = await page.evaluate(({ x, y }) => {
      const header = Array.from(document.querySelectorAll('header')).find(
        (el) => el.querySelector('h1')?.textContent?.trim() === '予約',
      )
      const hit = document.elementFromPoint(x, y)
      const headerBox = header?.getBoundingClientRect()
      const badge = document.querySelector('a.relative.z-10')
      const badgeBox = badge?.getBoundingClientRect()
      return {
        scrollY: window.scrollY,
        hitHeader: Boolean(header && hit && header.contains(hit)),
        hitTag: hit?.tagName ?? '(なし)',
        headerTop: headerBox?.top ?? null,
        headerBottom: headerBox?.bottom ?? null,
        badgeTop: badgeBox?.top ?? null,
        badgeBottom: badgeBox?.bottom ?? null,
        headerZ: header ? getComputedStyle(header).zIndex : 'auto',
        badgeZ: badge ? getComputedStyle(badge).zIndex : 'auto',
        bannerTop: document.querySelector('[role="alert"]')?.getBoundingClientRect().top ?? null,
        bannerBottom: document.querySelector('[role="alert"]')?.getBoundingClientRect().bottom ?? null,
        bannerHeight: document.querySelector('[role="alert"]')?.getBoundingClientRect().height ?? 0,
        hitBadge: Boolean(badge && hit && badge.contains(hit)),
        badgeLeft: badgeBox?.left ?? null,
        badgeRight: badgeBox?.right ?? null,
      }
    }, { x: setup.x, y: setup.targetY })
    log(`  point=(${Math.round(setup.x)},${Math.round(setup.targetY)}) scrollY=${Math.round(result.scrollY)} banner=${result.bannerTop}..${result.bannerBottom} (${result.bannerHeight}px) hit=${result.hitTag} header=${result.headerTop}..${result.headerBottom} z=${result.headerZ} badge=${result.badgeTop}..${result.badgeBottom} z=${result.badgeZ} hitHeader=${result.hitHeader} hitBadge=${result.hitBadge}`)
    if (result.scrollY <= 0 || result.headerTop === null || result.badgeTop === null) {
      ng.push(`① ${width}px: スクロールまたは重なり判定の前提が成立しない`)
    } else if (
      setup.targetY < result.badgeTop ||
      setup.targetY > result.badgeBottom ||
      setup.x < result.badgeLeft ||
      setup.x > result.badgeRight
    ) {
      ng.push(`① ${width}px: 前提が成立しない（判定点がスクロール後のバッジ矩形に入っていない）`)
    } else if (
      result.bannerHeight <= 0 ||
      result.bannerTop === null ||
      Math.abs(result.bannerTop) > 1 ||
      result.bannerBottom === null
    ) {
      ng.push(`① ${width}px: サーキットブレーカー帯が表示されない`)
    } else if (result.headerTop < result.bannerBottom - 1) {
      ng.push(`① ${width}px: PageHeader がサーキットブレーカー帯に重なる`)
    } else if (!result.hitHeader || result.hitBadge) {
      ng.push(
        `① ${width}px: ヘッダーが行内リンクより前面でない（hit=${result.hitTag}, z=${result.headerZ}/${result.badgeZ}）`,
      )
    }
  }
  await context.close()
}

for (const width of [400, 1280]) {
  const { context, page } = await openPage(width)
  log(`\n=== ② ${width}px: 録画一覧をスクロールして header と導線を確認 ===`)
  const capabilities = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/capabilities')
  await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
  await capabilities
  const lastRow = page.getByText('一覧スクロール確認 24', { exact: true })
  await lastRow.waitFor({ timeout: 15000 })
  await lastRow.scrollIntoViewIfNeeded()
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  const result = await page.evaluate(() => {
    const header = Array.from(document.querySelectorAll('header')).find(
      (el) => el.querySelector('h1')?.textContent?.trim() === '録画',
    )
    const box = header?.getBoundingClientRect()
    return {
      scrollY: window.scrollY,
      headerTop: box?.top ?? null,
      headerBottom: box?.bottom ?? null,
      viewportHeight: window.innerHeight,
      bannerBottom: document.querySelector('.sticky.top-0.z-20')?.getBoundingClientRect().bottom ?? 0,
      bottomNavTop: (() => {
        const nav = document.querySelector('[data-testid="bottom-nav"]')
        return nav && getComputedStyle(nav).display !== 'none' ? nav.getBoundingClientRect().top : null
      })(),
    }
  })
  const chaseLinks = await page.getByRole('link', { name: /を追っかけ再生/ }).count()
  log(`  scrollY=${Math.round(result.scrollY)} header=${result.headerTop}..${result.headerBottom} chaseLinks=${chaseLinks}`)
  if (result.scrollY <= 0 || result.headerTop === null || result.headerTop < -1 || result.headerBottom > result.viewportHeight) {
    ng.push(`② ${width}px: 録画一覧のスクロール後に sticky header が表示領域にない`)
  }
  if (result.headerTop < result.bannerBottom - 1) {
    ng.push(`② ${width}px: PageHeader が StickyBanners の領域に重なる`)
  }
  if (result.bottomNavTop !== null && result.headerBottom >= result.bottomNavTop) {
    ng.push(`② ${width}px: PageHeader が下部ナビの領域に重なる`)
  }
  if (chaseLinks !== 0) ng.push(`② ${width}px: 録画中の行に追っかけリンクが残っている`)

  await page.getByRole('button', { name: '選択' }).click()
  const selectionBar = page.getByRole('toolbar', { name: '選択した録画の操作' })
  await selectionBar.waitFor({ timeout: 5000 })
  const lowerBarGeometry = await page.evaluate(() => {
    const header = Array.from(document.querySelectorAll('header')).find(
      (el) => el.querySelector('h1')?.textContent?.trim() === '録画',
    )
    const headerBox = header?.getBoundingClientRect()
    const toolbar = document.querySelector('[role="toolbar"][aria-label="選択した録画の操作"]')
    const toolbarBox = toolbar?.getBoundingClientRect()
    const nav = document.querySelector('[data-testid="bottom-nav"]')
    const navVisible = Boolean(nav && getComputedStyle(nav).display !== 'none')
    return {
      headerBottom: headerBox?.bottom ?? null,
      toolbarTop: toolbarBox?.top ?? null,
      toolbarBottom: toolbarBox?.bottom ?? null,
      bottomNavTop: navVisible ? nav.getBoundingClientRect().top : null,
    }
  })
  log(`  selectionBar headerBottom=${lowerBarGeometry.headerBottom} toolbar=${lowerBarGeometry.toolbarTop}..${lowerBarGeometry.toolbarBottom} bottomNavTop=${lowerBarGeometry.bottomNavTop}`)
  if (lowerBarGeometry.headerBottom === null || lowerBarGeometry.toolbarTop === null || lowerBarGeometry.headerBottom >= lowerBarGeometry.toolbarTop) {
    ng.push(`② ${width}px: PageHeader が固定の選択バーに重なる`)
  }
  if (lowerBarGeometry.bottomNavTop !== null && lowerBarGeometry.toolbarBottom > lowerBarGeometry.bottomNavTop + 1) {
    ng.push(`② ${width}px: 固定の選択バーが下部ナビに重なる`)
  }
  await context.close()
}

await finish(ng, browser)
