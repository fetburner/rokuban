import path from 'node:path'
import { FIXED_NOW, apiHandler, fixturePairs, iso, recordings } from './design-fixtures.mjs'
import {
  URL_BASE,
  checkMissingStrings,
  computedOf,
  createOpen,
  desktop,
  homeDesktop,
  isAmber,
  isRed,
  mobile,
  mobileWide,
  prepareScreenshotDir,
  screenOf,
  themes,
} from './design-support.mjs'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const ng = []

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(fixturePairs, ng)

const OUT_DIR = prepareScreenshotDir('design-home')

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const open = createOpen(browser, ng)

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- ホーム管理モード M8-26: 時間軸の実寸 -------------------------------
// 3 幅でページ自体は固定し、時間軸の枠だけが横スクロールすることを測る。
// 時間軸は機械配置の推定ではなく既存 API の観測値を時刻に置く表示なので、
// この実ブラウザ検査で初期位置・30 分ブロック・容量ラベルの衝突を確認する。
log('\n=== ホーム管理モード M8-26: 時間軸の実寸 ===')
async function waitForHomeTimelineLayout(page) {
  return page.waitForFunction(
    () => {
      const timeline = document.querySelector('[data-testid="home-ops-timeline"]')
      const frame = timeline?.querySelector('[data-testid="home-ops-timeline-frame"]')
      const content = timeline?.querySelector('[data-testid="home-ops-timeline-content"]')
      const marker = timeline?.querySelector('[data-testid="home-timeline-now"]')
      if (!frame || !content || !marker) return false
      // #1021 の固定 fixture は今日 0:00 から翌々日 0:00 までの 48 時間。
      // スマホは 30px/h、desktop は 64px/h。React Query 完了後に frame が現れ、
      // ResizeObserver と初期スクロールが反映された実寸まで待つ。
      const hourPx = window.innerWidth <= 480 ? 30 : 64
      const expectedContentWidth = 48 * hourPx + 64
      const frameRect = frame.getBoundingClientRect()
      const markerRect = marker.getBoundingClientRect()
      return Math.abs(content.getBoundingClientRect().width - expectedContentWidth) < 1 &&
        frame.scrollLeft > 0 &&
        markerRect.x >= frameRect.x &&
        markerRect.right <= frameRect.right
    },
    null,
    { timeout: 5000 },
  ).then(() => true).catch(() => false)
}
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile, mobileWide]) {
    const { context, page } = await open(viewport, theme, {
      name: 'home-ops-timeline',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    const layoutReady = await waitForHomeTimelineLayout(page)
    if (!layoutReady) {
      ng.push(`[ops-timeline/${theme}/${viewport.width}px] 画面幅に合う時間軸幅・初期スクロールが安定しない`)
    }
    const geometry = await page.evaluate(() => {
      const rect = (element) => {
        if (!element) return null
        const { x, y, width, height, right, bottom } = element.getBoundingClientRect()
        return { x, y, width, height, right, bottom }
      }
      const timeline = document.querySelector('[data-testid="home-ops-timeline"]')
      const frame = timeline?.querySelector('[data-testid="home-ops-timeline-frame"]') ?? null
      const content = timeline?.querySelector('[data-testid="home-ops-timeline-content"]') ?? null
      const marker = timeline?.querySelector('[data-testid="home-timeline-now"]') ?? null
      const thirtyMinuteBlock = timeline?.querySelector(
        '[data-testid="home-timeline-block"][data-duration-ms="1800000"]',
      ) ?? null
      const rowLabels = [...(timeline?.querySelectorAll('[data-testid="home-timeline-row-label"]') ?? [])]
      const overageLabels = [...(timeline?.querySelectorAll('[data-testid="home-overage-label"]') ?? [])]
      const ticks = [...(timeline?.querySelectorAll('[data-testid="home-timeline-tick"]') ?? [])]
      const home = document.querySelector('[data-testid="page-content"], [data-testid="bounded-page-content"]')
      const controls = [...(home?.querySelectorAll('a, button, input, select, summary, [role="button"], [tabindex="0"]') ?? [])]
        .filter((element) => element.getClientRects().length > 0)
      return {
        timeline: rect(timeline),
        frame: rect(frame),
        marker: rect(marker),
        block: rect(thirtyMinuteBlock),
        documentScrollWidth: document.documentElement.scrollWidth,
        bodyScrollWidth: document.body.scrollWidth,
        viewportWidth: window.innerWidth,
        frameClientWidth: frame?.clientWidth ?? null,
        frameScrollWidth: frame?.scrollWidth ?? null,
        frameScrollLeft: frame?.scrollLeft ?? null,
        contentWidth: content?.getBoundingClientRect().width ?? null,
        hourPx: content === null ? null : content.getBoundingClientRect().width <= 48 * 30 + 64 + 1 ? 30 : 64,
        tickRects: ticks.map((element) => ({ rect: rect(element), visibility: getComputedStyle(element).visibility })),
        rowLabelRects: rowLabels.map(rect),
        overageLabelRects: overageLabels.map(rect),
        controls: controls.map((element) => ({ rect: rect(element), tag: element.tagName })),
      }
    })

    if (geometry.timeline === null || geometry.frame === null) {
      ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸またはスクロール枠が無い`)
    } else {
      if (geometry.documentScrollWidth > geometry.viewportWidth || geometry.bodyScrollWidth > geometry.viewportWidth) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] ページ本体が横にはみ出す`)
      }
      if (!(geometry.frameScrollWidth > geometry.frameClientWidth)) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の枠内だけの横スクロールが無い`)
      }
      const expectedContentWidth = 48 * (viewport.width <= 480 ? 30 : 64) + 64
      if (geometry.contentWidth === null || Math.abs(geometry.contentWidth - expectedContentWidth) >= 1) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の縮尺が想定と違う (${geometry.contentWidth}px; expected ${expectedContentWidth}px)`)
      }
      if (geometry.marker === null) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 現在時刻の線が無い`)
      } else if (
        geometry.marker.x < geometry.frame.x ||
        geometry.marker.right > geometry.frame.right ||
        geometry.marker.x < geometry.frame.x - 1
      ) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 現在時刻の線が初期表示範囲に無い`)
      }
      if (viewport.width === mobile.width && (geometry.block === null || geometry.block.width < 12)) {
        ng.push(`[ops-timeline/${theme}/360px] 30 分番組のブロックが 12px 未満`)
      }
      for (const control of geometry.controls) {
        if (control.rect === null || control.rect.width < 24 || control.rect.height < 24) {
          ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸の操作標的が 24×24px 未満 (${control.tag})`)
        }
      }
      if (geometry.overageLabelRects.length !== 2) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 全 jammedTypes の容量超過ラベルが 2 件でない（${geometry.overageLabelRects.length}）`)
      }
      const visibleTicks = geometry.tickRects
        .filter((tick) => tick.visibility !== 'hidden' && tick.rect && tick.rect.right > geometry.frame.x && tick.rect.x < geometry.frame.right)
      if (visibleTicks.some(({ rect: tick }) => tick.x < geometry.frame.x - 0.5 || tick.right > geometry.frame.right + 0.5)) {
        ng.push(`[ops-timeline/${theme}/${viewport.width}px] 初期表示範囲で時刻目盛りラベルがクリップする`)
      }
      for (const label of geometry.overageLabelRects) {
        if (
          label.x < geometry.frame.x ||
          label.right > geometry.frame.right ||
          geometry.rowLabelRects.some((rowLabel) => rowLabel && label.x < rowLabel.right && label.right > rowLabel.x)
        ) {
          ng.push(`[ops-timeline/${theme}/${viewport.width}px] 容量超過ラベルが枠外か行見出しと重なる`)
        }
      }
      log(`  [ops-timeline/${theme}/${viewport.width}px] frame=${geometry.frameClientWidth}px content=${geometry.contentWidth}px (${geometry.hourPx}px/h) scrollLeft=${geometry.frameScrollLeft}px now=${(geometry.marker?.x ?? 0) - geometry.frame.x}px visibleTicks=${visibleTicks.length} hiddenEdgeTicks=${geometry.tickRects.filter((tick) => tick.visibility === 'hidden').length}`)
    }

    {
      const text = ((await page.getByTestId('home-ops-timeline').innerText({ timeout: 3000 }).catch(() => '')) ?? '').replaceAll(/\s+/g, ' ')
      for (const expected of ['今日 0 時 → 明日の終わり', '地デジ / BS ごと', 'チューナー不足の区間', '枠の中を横にスクロールできます']) {
        if (!text.includes(expected)) ng.push(`[ops-timeline/${theme}/${viewport.width}px] 時間軸に「${expected}」が無い`)
      }
    }
    const file = path.join(OUT_DIR, `home-ops-timeline-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)} (${viewport.width}x${viewport.height}, single-site)`)
    await checkMissingStrings(ng, page, `home-ops-timeline/${theme}/${viewport.name}`)
    await context.close()
  }
}

// 警告の順序と overage の説明は、個別の予約を敗者として示さない。
{
  const { context, page } = await open(homeDesktop, 'light', {
    name: 'home-ops-actions',
    path: '/?mode=ops',
  }, { withBreaker: true, homeModeFixture: true, homeOpsFixture: true })
  await page.locator('section[aria-labelledby="home-action-required"]').waitFor({ timeout: 5000 }).catch(() => {})
  const warningRows = page.locator('section[aria-labelledby="home-action-required"] li[data-warning-kind]')
  const warningKinds = await warningRows.evaluateAll((elements) => elements.map((element) => element.dataset.warningKind))
  if (warningKinds.join(',') !== 'breaker,failed,not-recorded,overage,drop') {
    ng.push(`ホーム: 要対応の順序が breaker→failed→not-recorded→overage→drop でない（${warningKinds.join(',')}）`)
  }
  const overageRow = page.locator('li[data-warning-kind="overage"]')
  // 要対応の行が無い実装（M8-25）でも TimeoutError で結果が消えないよう、取れなければ NG に積む。
  const overageText = (await overageRow.innerText({ timeout: 3000 }).catch(() => null))?.replaceAll(/\s+/g, ' ') ?? ''
  if (overageText === '') ng.push('ホーム: チューナー不足の要対応行が無い（時間軸 + 要対応の一覧になっていない）')
  if (!overageText.includes('この時間帯の予約: 大相撲中継')) {
    ng.push(`ホーム: overage の時間帯 subtitle が予約を示していない（${overageText}）`)
  }
  const homeText = (await page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]').innerText()).replaceAll(/\s+/g, ' ')
  if (/(?:チューナー\s*#?\s*\d|tuner\s*#?\s*\d|容量(?:に)?(?:は)?(?:十分|余裕がある)|予約は容量に収まる)/i.test(homeText)) {
    ng.push('ホーム: tuner の個別割当または容量の確約を示す文言がある')
  }
  if (/(?:大相撲中継).{0,20}(?:録画失敗|録れない|失敗する|除外)|(?:録画失敗|録れない|失敗する|除外).{0,20}(?:大相撲中継)/.test(overageText)) {
    ng.push(`ホーム: overage が特定予約を敗者として示している（${overageText}）`)
  }
  await context.close()
}

// 複数サイトは不足区間の site をまたがず、行を site × 種別で分けて描く。
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile, mobileWide]) {
    const { context, page } = await open(viewport, theme, {
      name: 'home-ops-timeline-multisite',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true, multiSite: true })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    if (!(await waitForHomeTimelineLayout(page))) {
      ng.push(`[ops-timeline-multisite/${theme}/${viewport.width}px] 画面幅に合う時間軸幅・初期スクロールが安定しない`)
    }
    const siteRows = await page.locator('[data-testid="home-timeline-row-label"]').allTextContents()
    if (!siteRows.some((label) => label.includes('default · 地デジ')) ||
        !siteRows.some((label) => label.includes('sub · 地デジ'))) {
      ng.push(`[ops-timeline-multisite/${theme}/${viewport.width}px] site × channelType の行が分離されていない`)
    }
    const file = path.join(OUT_DIR, `home-ops-timeline-multisite-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)} (${viewport.width}x${viewport.height}, multi-site)`)
    await context.close()
  }
}

// 横スクロールバーが常に見える（macOS の自動非表示でも枠の中を横に動かせると分かる）。
// Playwright の既定は --hide-scrollbars でバーの高さが常に 0 になるので、この判定だけ
// そのフラグを外した専用のブラウザで測る。
log('\n=== ホーム管理モード: 横スクロールバーの可視 ===')
{
  const sbBrowser = await launchBrowser('chromium', { ignoreDefaultArgs: ['--hide-scrollbars'] })
  for (const viewport of [homeDesktop, mobile]) {
    const context = await sbBrowser.newContext({
      viewport: { width: viewport.width, height: viewport.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: 'light',
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ homeModeFixture: true, homeOpsFixture: true }))
    await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    const height = await page.getByTestId('home-ops-timeline-frame').evaluate((el) => el.offsetHeight - el.clientHeight).catch(() => 0)
    log(`  [ops-scrollbar/${viewport.width}px] バーの高さ ${height}px`)
    if (height < 4) ng.push(`[ops-scrollbar/${viewport.width}px] 横スクロールバーが見えない（高さ ${height}px）`)
    await context.close()
  }
  await sbBrowser.close()
}

// 窓幅を連続して変えたときに縮尺（30 / 64px/h）が往復しない。切替が時間軸の枠の幅
// （= 切替で変わるラベル幅に依存）で決まっていた版は、単一 site で 592–598px、複数
// site で 660–676px で 2 つのモードを往復した（レビュー実測）。
log('\n=== ホーム管理モード: 窓幅での縮尺の安定 ===')
for (const multiSite of [false, true]) {
  const { context, page } = await open({ name: 'home-ops-resize', width: 700, height: 900 }, 'light', {
    name: 'home-ops-resize',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true, multiSite })
  await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
  const widths = multiSite ? [656, 660, 664, 668, 672, 676, 680] : [588, 592, 594, 596, 598, 600, 604]
  const seen = []
  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 })
    await page.waitForTimeout(150)
    const samples = new Set()
    for (let i = 0; i < 10; i += 1) {
      samples.add(await page.evaluate(() => {
        const section = document.querySelector('[data-testid="home-ops-timeline"]')
        const content = document.querySelector('[data-testid="home-ops-timeline-content"]')
        return `${section?.getAttribute('data-hour-px')}/${Math.round(content?.getBoundingClientRect().width ?? -1)}`
      }))
      await page.waitForTimeout(50)
    }
    seen.push(`${width}px=${[...samples].join('|')}`)
    if ([...samples].some((sample) => sample.startsWith('undefined'))) {
      ng.push(`[ops-resize/${multiSite ? 'multi' : 'single'}/${width}px] 時間軸が描かれていない（縮尺を測れない）`)
    } else if (samples.size !== 1) {
      ng.push(`[ops-resize/${multiSite ? 'multi' : 'single'}/${width}px] 縮尺が往復する（${[...samples].join(' ⇄ ')}）`)
    }
  }
  log(`  [ops-resize/${multiSite ? 'multi' : 'single'}] ${seen.join(' ')}`)
  await context.close()
}

/** visibleTickLabels は時間軸の枠の中に見えている目盛りラベル（文言と中心の x）を返す。 */
async function visibleTickLabels(page) {
  return page.evaluate(() => {
    const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')?.getBoundingClientRect()
    if (!frame) return []
    return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
      .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
      .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
      .filter(({ rect }) => rect.x >= frame.x - 0.5 && rect.right <= frame.right + 0.5)
      .map(({ text, rect }) => ({ text, center: rect.x + rect.width / 2 }))
  })
}

// 時刻（午前を含む）とスクロール位置を変えても、見える範囲の境界で目盛りが半端に
// 切れず、「いま」の線が窓の中の正しい位置（現在時刻 × 縮尺）にある。
// 窓の始点が今日 12 時固定だと、午前では「いま」が窓の左端に張り付く。
log('\n=== ホーム管理モード: 午前の「いま」と目盛りの切れ ===')
for (const viewport of [homeDesktop, mobile]) {
  const hourPx = viewport.width <= 480 ? 30 : 64
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-ticks',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true })
  for (const clock of ['09:00', '14:17', '20:42', '23:50']) {
    const now = new Date(`2026-08-12T${clock}:00+09:00`)
    await page.clock.setFixedTime(now)
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.getByTestId('home-ops-timeline-frame').waitFor({ timeout: 5000 }).catch(() => {})
    await page.waitForFunction(
      (expected) => Math.abs((document.querySelector('[data-testid="home-ops-timeline-content"]')?.getBoundingClientRect().width ?? 0) - expected) < 1,
      48 * hourPx + 64,
      { timeout: 5000 },
    ).catch(() => ng.push(`[ops-ticks/${viewport.width}px/${clock}] 窓が 48 時間でない`))
    const [tokyoHour, tokyoMinute] = clock.split(':').map(Number)
    const expectedNowX = (tokyoHour + tokyoMinute / 60) * hourPx
    const measured = await page.evaluate(() => {
      const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const content = document.querySelector('[data-testid="home-ops-timeline-content"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frame || !content || !marker) return null
      return {
        nowInContent: marker.getBoundingClientRect().x - content.getBoundingClientRect().x,
        nowInFrame: marker.getBoundingClientRect().x - frame.getBoundingClientRect().x,
      }
    })
    if (measured === null || Math.abs(measured.nowInContent - expectedNowX) > 1.5) {
      ng.push(`[ops-ticks/${viewport.width}px/${clock}] 「いま」の線が現在時刻の位置にない（${JSON.stringify(measured)}、期待 ${expectedNowX}px）`)
    }
    // 初期表示（現在 − 3 時間から）で見えている目盛り。ラフ（20:42 / 1280px）は
    // 過去側に「18時」が見える。端から 20px 以内の目盛りを固定箱で隠すと消えていた。
    const initialTicks = await visibleTickLabels(page)
    log(`  [ops-ticks/${viewport.width}px/${clock}] 初期表示の目盛り: ${initialTicks.map((tick) => tick.text).join(' ') || '（なし）'}`)
    if (clock === '20:42') {
      const markerX = await page.getByTestId('home-timeline-now').first().evaluate((el) => el.getBoundingClientRect().x).catch(() => null)
      if (markerX === null || !initialTicks.some((tick) => tick.center < markerX)) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}] 初期表示の過去側に目盛りが 1 本も無い（${initialTicks.map((tick) => tick.text).join(',')}）`)
      }
      if (viewport.width === homeDesktop.width && !initialTicks.some((tick) => tick.text === '18時')) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}] 初期表示に「18時」が見えない（ラフでは見える）`)
      }
    }
    for (const scrollLeft of [0, 37, 101, 333, 100000]) {
      await page.evaluate((x) => {
        const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
        if (frame) frame.scrollLeft = x
      }, scrollLeft)
      let viewportSynced = false
      try {
        // scroll 直後は state の再描画前に目盛りが切れて見えるため、固定時間ではなく判定値の反映を待つ。
        await page.waitForFunction(() => {
          const frame = document.querySelector('[data-testid="home-ops-timeline-frame"]')
          if (!frame) return false
          return Number(frame.getAttribute('data-tick-scroll-left')) === frame.scrollLeft &&
            Number(frame.getAttribute('data-tick-client-width')) === frame.clientWidth
        }, undefined, { timeout: 5000 })
        viewportSynced = true
      } catch (error) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}/scrollLeft=${scrollLeft}] 目盛り判定に使う枠の位置が DOM に反映されない（${error.message}）`)
      }
      if (viewportSynced) {
        const cut = await page.evaluate(() => {
          const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
          if (!frameElement) return ['時間軸の枠が無い']
          const frame = frameElement.getBoundingClientRect()
          return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
            .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
            .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
            .filter(({ rect }) => rect.right > frame.x && rect.x < frame.right)
            .filter(({ rect }) => rect.x < frame.x - 0.5 || rect.right > frame.right + 0.5)
            .map(({ text }) => text)
        })
        if (cut.length > 0) {
          ng.push(`[ops-ticks/${viewport.width}px/${clock}/scrollLeft=${scrollLeft}] 見える範囲の境界で目盛りが切れて見える（${cut.join(',')}）`)
        }
      }
      // 「いま」の pill（z-20）の下に隠れる目盛りは出さない。
      const underPill = await page.evaluate(() => {
        const pill = document.querySelector('[data-testid="home-timeline-now-label"]')?.getBoundingClientRect()
        if (!pill) return ['pill が無い']
        return [...document.querySelectorAll('[data-testid="home-timeline-tick"]')]
          .filter((tick) => getComputedStyle(tick).visibility !== 'hidden')
          .map((tick) => ({ text: tick.textContent, rect: tick.getBoundingClientRect() }))
          .filter(({ rect }) => rect.right > pill.x && rect.x < pill.right)
          .map(({ text }) => text)
      })
      if (underPill.length > 0) {
        ng.push(`[ops-ticks/${viewport.width}px/${clock}/scrollLeft=${scrollLeft}] 目盛りが「いま」の pill の下に隠れる（${underPill.join(',')}）`)
      }
    }
    log(`  [ops-ticks/${viewport.width}px/${clock}] now@content=${measured?.nowInContent?.toFixed(1)}px (期待 ${expectedNowX}px)`)
  }
  await context.close()
}

// 0 時をまたぐ録画は 0:00 から終わるまで時間軸に残る。API の `from` は開始時刻で
// 絞るので、窓の始点を `from` に渡すと前日 23:40 開始の録画中が消えていた。
log('\n=== ホーム管理モード: 0 時をまたぐ録画 ===')
{
  const midnight = new Date('2026-08-13T00:20:00+09:00')
  const crossingStart = new Date('2026-08-12T23:40:00+09:00').getTime()
  const crossing = [
    { ...recordings[1], id: 91, title: '日またぎの映画', status: 'recording', startAt: iso(crossingStart), startedAt: iso(crossingStart), durationMs: 140 * 60_000, createdAt: iso(crossingStart) },
    { ...recordings[1], id: 92, title: '日またぎの完了', status: 'finished', startAt: iso(crossingStart - 10 * 60_000), durationMs: 40 * 60_000, createdAt: iso(crossingStart), dropSummary: undefined },
  ]
  for (const viewport of [homeDesktop, mobile]) {
    const hourPx = viewport.width <= 480 ? 30 : 64
    const { context, page } = await open(viewport, 'light', {
      name: 'home-ops-midnight',
      path: '/?mode=ops',
    }, { homeModeFixture: true, homeOpsFixture: true, extraOpsRecordings: crossing })
    await page.clock.setFixedTime(midnight)
    await page.reload({ waitUntil: 'domcontentloaded' })
    try {
      for (const title of ['日またぎの映画', '日またぎの完了']) {
        await page.locator(`[data-testid="home-timeline-block"][title="${title}"]`).waitFor({ timeout: 5000 })
      }
    } catch (error) {
      ng.push(`[ops-midnight/${viewport.width}px] 日またぎの録画ブロックが期限内に描画されない（${error.message}）`)
      await context.close()
      continue
    }
    await page.evaluate(() => document.fonts.ready)
    const blocks = await page.evaluate(() => {
      const content = document.querySelector('[data-testid="home-ops-timeline-content"]')?.getBoundingClientRect()
      return [...document.querySelectorAll('[data-testid="home-timeline-block"]')].map((block) => {
        const rect = block.getBoundingClientRect()
        return { kind: block.dataset.kind, title: block.getAttribute('title'), left: rect.x - (content?.x ?? 0), width: rect.width }
      })
    })
    for (const [title, kind, visibleMinutes] of [['日またぎの映画', 'recording', 120], ['日またぎの完了', 'finished', 10]]) {
      const block = blocks.find((b) => b.title === title)
      if (block === undefined || block.kind !== kind) {
        ng.push(`[ops-midnight/${viewport.width}px] 0 時をまたぐ「${title}」（${kind}）が 00:20 の時間軸に無い（${blocks.map((b) => `${b.kind}:${b.title}`).join(', ')}）`)
      } else if (Math.abs(block.left) > 0.5 || Math.abs(block.width - (visibleMinutes / 60) * hourPx) > 1) {
        ng.push(`[ops-midnight/${viewport.width}px] 「${title}」が窓の左端で切られていない（left=${block.left}px width=${block.width}px、期待 0px / ${(visibleMinutes / 60) * hourPx}px）`)
      }
    }
    log(`  [ops-midnight/${viewport.width}px] ${blocks.map((b) => `${b.kind}:${b.title}@${b.left.toFixed(1)}+${b.width.toFixed(1)}`).join(' ')}`)
    await context.close()
  }
}

// 管理モードのホームから完了録画へ: 「録画・予約の詳細」を開く → 行を選ぶ の 2 操作。
// 「直近の完了へ」アンカーは置かない（docs/frontend/home.md）。
log('\n=== ホーム管理モード: 完了録画への到達 ===')
for (const viewport of [homeDesktop, mobile]) {
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-reach',
    path: '/?mode=ops',
  }, { homeModeFixture: true, homeOpsFixture: true })
  const summary = page.getByTestId('home-timeline-details').locator('summary')
  await summary.waitFor({ timeout: 5000 }).catch(() => {})
  let operations = 0
  if (await summary.count() === 1) {
    await summary.click()
    operations += 1
    const row = page.getByTestId('home-timeline-detail-row').filter({ hasText: '録れた' }).first()
    if (await row.count() === 1) {
      await row.locator('a').click()
      operations += 1
      await page.waitForURL(/\/recordings\/\d+$/, { timeout: 5000 }).catch(() => {})
    }
  }
  const pathname = new URL(page.url()).pathname
  if (!/^\/recordings\/\d+$/.test(pathname) || operations !== 2) {
    ng.push(`[ops-reach/${viewport.width}px] 管理モードのホームから 2 操作で完了録画へ着かない（操作=${operations} 到達=${pathname}）`)
  }
  log(`  [ops-reach/${viewport.width}px] 操作=${operations} 到達=${pathname}`)
  await context.close()
}

// 時計更新と recordings SSE による親画面の再描画後も、手動スクロールを奪わず
// 「いま」の線だけが時間経過分だけ進むことを実ブラウザで確認する。
log('\n=== ホーム管理モード: 手動スクロール保持と現在時刻追従 ===')
for (const viewport of [homeDesktop, mobile]) {
  let releaseSse
  let markSseRequested
  const sseGate = new Promise((resolve) => { releaseSse = resolve })
  const sseRequested = new Promise((resolve) => { markSseRequested = resolve })
  const { context, page } = await open(viewport, 'light', {
    name: 'home-ops-follow',
    path: '/?mode=ops',
  }, {
    // SSE 後の時計購読を測るため、Date だけ固定する setFixedTime ではなく timer も進める。
    installClock: true,
    homeModeFixture: true,
    homeOpsFixture: true,
    homeOpsSseFixture: async () => {
      markSseRequested()
      await sseGate
    },
  })
  const frame = page.getByTestId('home-ops-timeline-frame')
  await frame.waitFor({ timeout: 5000 }).catch(() => {})
  if (!(await waitForHomeTimelineLayout(page))) {
    ng.push(`[ops-follow/${viewport.width}px] 初期の時間軸レイアウトが安定しない`)
    releaseSse()
    await context.close()
    continue
  }
  await Promise.race([
    sseRequested,
    page.waitForTimeout(5000).then(() => { throw new Error('SSE request timeout') }),
  ]).catch(() => ng.push(`[ops-follow/${viewport.width}px] recordings SSE 接続が始まらない`))

  const hourPx = viewport.width <= 480 ? 30 : 64
  const initial = await page.evaluate(() => {
    const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
    const marker = document.querySelector('[data-testid="home-timeline-now"]')
    if (!frameElement || !marker) return null
    return {
      scrollLeft: frameElement.scrollLeft,
      markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
    }
  })
  if (initial === null || Math.abs(initial.markerX - 3 * hourPx) > 1) {
    ng.push(`[ops-follow/${viewport.width}px] 初期表示が現在時刻の約3時間前から始まらない (${JSON.stringify(initial)})`)
  }

  await frame.evaluate((element) => { element.scrollLeft += 48 })
  const manual = await page.evaluate(() => {
    const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
    const marker = document.querySelector('[data-testid="home-timeline-now"]')
    if (!frameElement || !marker) return null
    return {
      scrollLeft: frameElement.scrollLeft,
      markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
    }
  })
  await page.clock.fastForward(60 * 60_000)
  releaseSse()
  const followed = await page.waitForFunction(
    ({ expectedScrollLeft, expectedMarkerX }) => {
      const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frameElement || !marker) return false
      const markerX = marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x
      return Math.abs(frameElement.scrollLeft - expectedScrollLeft) < 1 &&
        Math.abs(markerX - expectedMarkerX) < 1
    },
    {
      expectedScrollLeft: manual?.scrollLeft ?? Number.NaN,
      expectedMarkerX: (manual?.markerX ?? 0) + hourPx,
    },
    { timeout: 5000 },
  ).then(() => true).catch(() => false)
  if (manual === null || !followed) {
    const actual = await page.evaluate(() => {
      const frameElement = document.querySelector('[data-testid="home-ops-timeline-frame"]')
      const marker = document.querySelector('[data-testid="home-timeline-now"]')
      if (!frameElement || !marker) return null
      return {
        scrollLeft: frameElement.scrollLeft,
        markerX: marker.getBoundingClientRect().x - frameElement.getBoundingClientRect().x,
      }
    })
    ng.push(`[ops-follow/${viewport.width}px] SSE 後に手動スクロールを保持して現在時刻だけ進まない (manual=${JSON.stringify(manual)}, actual=${JSON.stringify(actual)})`)
  } else {
    log(`  [ops-follow/${viewport.width}px] 初期now=${initial?.markerX}px 手動scrollLeft=${manual.scrollLeft}px → SSE後scrollLeft維持 / now +${hourPx}px`)
  }
  await context.close()
}

// 初回アクセス（URL も localStorage も空）の既定は「見る」。既存の管理側
// シナリオは上の mode=ops で維持し、既定値だけは `/` を直接開いて確認する。
{
  const screen = { name: 'home-default', path: '/', wait: 'text=続きから再生' }
  const { context, page } = await open(homeDesktop, 'light', screen, { homeModeFixture: true })
  const watchLink = page.getByRole('link', { name: '見る', exact: true })
  const opsLink = page.getByRole('link', { name: '管理', exact: false })
  if ((await watchLink.getAttribute('aria-current')) !== 'page') {
    ng.push('home/default: URL・localStorage が空なのに既定が「見る」でない')
  }
  if ((await opsLink.getAttribute('aria-current')) === 'page') {
    ng.push('home/default: URL・localStorage が空なのに「管理」が選択されている')
  }
  await context.close()
}

// ブレーカー発動中の「見る」帯も desktop/phone・ライト/ダークで撮る。
for (const theme of themes) {
  for (const viewport of [homeDesktop, mobile]) {
    const screen = { name: 'home-watch-breaker', path: '/?mode=watch' }
    const { context, page } = await open(viewport, theme, screen, {
      withBreaker: true,
      homeModeFixture: true,
    })
    if ((await page.getByTestId('home-watch-breaker-band').count()) === 0) {
      ng.push(`[home-watch-breaker/${theme}/${viewport.width}px] 見る側のブレーカー帯が無い`)
    }
    if ((await page.getByText('削除が保留されています', { exact: false }).count()) > 0) {
      ng.push(`[home-watch-breaker/${theme}/${viewport.width}px] 共通バナーと見る側の帯が重複する`)
    }
    const file = path.join(OUT_DIR, `home-watch-breaker-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `home-watch-breaker/${theme}/${viewport.name}`)
    await context.close()
  }
}

// --- ホーム管理モード: timeline / action list の実ブラウザ確認 ---
//
// jsdom が見る構造だけでなく、画面に両方の領域が描かれ、容量警告の導線が
// 既存の番組表へ遷移することを実ブラウザで確認する。
log("\n=== ホーム管理モード: timeline / action list ===")
{
  const { context, page } = await open(desktop, 'light', screenOf('home'), { homeModeFixture: true, homeOpsFixture: true })
  for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
    const found = await page.getByRole('heading', { name: heading }).count()
    if (found === 0) {
      ng.push(`ホーム: 見出し「${heading}」が #1021 fixture で出ていない`)
    }
  }
  const shortageLink = page.getByRole('link', { name: /地デジ・BSが 1 本不足しています/ })
  if ((await shortageLink.count()) === 0) {
    ng.push('ホーム: 全 jammedTypes の不足警告項目が無い')
  } else {
    await shortageLink.first().click()
    await page.waitForTimeout(400)
    const url = new URL(page.url())
    if (url.pathname !== '/programs') {
      ng.push(`ホーム: 容量不足をクリックしても番組表へ飛ばない（${url.pathname}）`)
    }
  }
  await context.close()
}
{
  // 両方向: 全領域が空のときは timeline/action heading を出さず、単一の空状態だけを出す
  const context = await browser.newContext({
    viewport: { width: desktop.width, height: desktop.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: 'light',
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, apiHandler({ emptyHome: true }))
  await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
  await page
    .getByText('表示できる項目がありません')
    .waitFor({ timeout: 5000 })
    .catch(() => {
      ng.push('ホーム: 全セクション空でも単一の空状態が出ない')
    })
  for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
    if ((await page.getByRole('heading', { name: heading }).count()) > 0) {
      ng.push(`ホーム: 全領域空のはずが見出し「${heading}」が出ている`)
    }
  }
  await context.close()
}

log("\n=== ①'' ホーム: 警告の種別ごとの色（琥珀 vs destructive） ===")
{
  // #1021 の各種別と、予約subtitleが実際に重なる fixture を使う。
  const { context, page } = await open(desktop, 'light', screenOf('home'), {
    withBreaker: true,
    homeModeFixture: true,
    homeOpsFixture: true,
  })
  await page.getByRole('heading', { name: '要対応' }).waitFor({ timeout: 5000 }).catch(() => {})

  // 種別チップだけが色を持つ。行の文字色は中立のまま。
  const chipColor = (rowLocator) => computedOf(rowLocator.getByTestId('warning-chip'), 'color')

  // 容量超過 = 琥珀。
  const overageRow = page.locator('li[data-warning-kind="overage"]').first()
  const overageColor = await chipColor(overageRow)
  if (overageColor === null) {
    ng.push('ホーム: 容量不足の警告項目の文字色が取得できない')
  } else if (!isAmber(overageColor.rgba)) {
    ng.push(
      `ホーム: 容量不足の警告項目が琥珀でない（${overageColor.value} = ${overageColor.rgba}）`,
    )
  }

  // サーキットブレーカー = destructive。
  const breakerRow = page.locator('li[data-warning-kind="breaker"]').first()
  const breakerColor = await chipColor(breakerRow)
  if (breakerColor === null) {
    ng.push('ホーム: ブレーカーの警告項目の文字色が取得できない')
  } else if (!isRed(breakerColor.rgba)) {
    ng.push(
      `ホーム: ブレーカーの警告項目が destructive でない（${breakerColor.value} = ${breakerColor.rgba}）`,
    )
  }

  // ドロップ = destructive。
  const dropRow = page.locator('li[data-warning-kind="drop"]').first()
  const dropColor = await chipColor(dropRow)
  if (dropColor === null) {
    ng.push('ホーム: ドロップの警告項目の文字色が取得できない')
  } else if (!isRed(dropColor.rgba)) {
    ng.push(`ホーム: ドロップの警告項目が destructive でない（${dropColor.value} = ${dropColor.rgba}）`)
  }

  // 失敗録画 = destructive（録画が失われたことは取り返しがつかない）。
  const failedRow = page.locator('li[data-warning-kind="failed"]').first()
  const failedColor = await chipColor(failedRow)
  if (failedColor === null) {
    ng.push('ホーム: 失敗録画の警告項目の文字色が取得できない（行が出ていない可能性）')
  } else if (!isRed(failedColor.rgba)) {
    ng.push(
      `ホーム: 失敗録画の警告項目が destructive でない（${failedColor.value} = ${failedColor.rgba}）`,
    )
  }

  log(
    `  容量不足=${overageColor?.rgba} / ブレーカー=${breakerColor?.rgba} / ドロップ=${dropColor?.rgba} / 失敗録画=${failedColor?.rgba}`,
  )
  await context.close()
}

// --- ①''' ホーム: 実時計でのクエリキー安定性（無限再取得の回帰検出） ---
//
// **時計を止めない。** このファイルの他の全判定は `page.clock.setFixedTime` などで
// 時刻を固定して始める。時計を止めると、「時計が動くことに起因する欠陥」（レンダーごとに
// 変わる生の Date.now() をキャッシュキーに載せて無限再取得になる、等）を
// 原理的に検出できない（レビューで発覚。実装 PR で `/api/capacity/overages` の
// `start` に生の Date.now() を渡しており、時計を止めた判定は全部素通りしていた。
// web/e2e/README.md §判定を足すときの規律）。ここだけ時計を止めずに実際の要求回数
// を数える。
//
// **上限だけでなく下限（>= 1）も見る。** 「N 回以下」だけの判定は、クエリを
// 消した・`enabled: false` にした・そもそもページが起動しない、のいずれでも
// 0 回で緑になる（レビュー指摘）。数え始める前にホームの目印を待って画面が
// 実際に立っていることを確かめ、そのうえで回数の下限と上限の両方に掛ける
// （`badge-links.mjs` ⓪ が「配っている bundle が dist の現物と一致するか」を
// 最初に見ているのと同じ思想 --- 前提が崩れていると下流の判定は全部無意味）。
log("\n=== ①''' ホーム: 実時計でのクエリキー安定性（無限再取得の回帰検出） ===")
{
  const context = await browser.newContext({
    viewport: { width: desktop.width, height: desktop.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
  })
  const page = await context.newPage()
  const overagesRequests = []
  page.on('request', (req) => {
    const url = new URL(req.url())
    if (url.pathname === '/api/capacity/overages') overagesRequests.push(url.toString())
  })
  await installApiStubs(page, apiHandler())
  await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
  let homeUp = true
  await page
    .locator(screenOf('home').wait)
    .first()
    .waitFor({ timeout: 15000 })
    .catch(() => {
      homeUp = false
      ng.push(
        `ホーム: 実時計（page.clock を使わない）でホームが立たない（目印「${screenOf('home').wait}」が出ない）`,
      )
    })
  await page.waitForTimeout(2500)
  log(`  /api/capacity/overages への実要求回数（実時計 2.5 秒）: ${overagesRequests.length}`)
  if (homeUp && overagesRequests.length < 1) {
    ng.push(
      'ホーム: 実時計で /api/capacity/overages を一度も要求していない' +
        '（クエリが消えている・enabled: false になっている疑い。' +
        'この下限が無いと「0 回」でこの判定は緑になる）',
    )
  }
  if (overagesRequests.length > 3) {
    ng.push(
      `ホーム: 実時計で /api/capacity/overages への要求が ${overagesRequests.length} 回` +
        '（無限再取得の疑い。start に生の Date.now() を渡していないか確認する。' +
        'レビュー実測では 18〜37 回だった）',
    )
  }
  await context.close()
}

await finish(ng, browser)
