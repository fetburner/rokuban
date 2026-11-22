import path from 'node:path'
import { FIXED_NOW, SITE, apiHandler, fixturePairs, services } from './design-fixtures.mjs'
import {
  URL_BASE,
  checkMissingStrings,
  computedOf,
  createOpen,
  desktop,
  mobile,
  mobileWide,
  prepareScreenshotDir,
  recordingDetailScreen,
  screenOf,
  seriesHubDesktop,
  seriesHubMobile,
  themes,
  viewports,
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

const OUT_DIR = prepareScreenshotDir('design-layout')

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const open = createOpen(browser, ng)

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- 番組ハブ: 400px / デスクトップの 3 塊と操作 -------------------------
//
// 全画面ショットは 360px の共通モバイル幅で揃え、ここでは受け入れ条件の 400px 幅と
// デスクトップ幅の両方で同じ判定を回す。API は design-fixtures.mjs の seriesHubRecordings を使うので、最新の
// 失敗回を飛ばす主ボタン、自動キー、次回、分類メニューを実ブラウザで確認できる。
for (const hubViewport of [seriesHubMobile, seriesHubDesktop])
for (const theme of themes) {
  const { context, page } = await open(hubViewport, theme, screenOf('series-hub'))
  const identity = page.getByRole('region', { name: 'シリーズ情報' })
  const actions = page.getByRole('region', { name: 'シリーズの操作' })
  const episodes = page.getByRole('region', { name: 'このシリーズの録画' })
  const blocks = await Promise.all([identity, actions, episodes].map((block) => block.boundingBox()))
  if (blocks.some((box) => box === null)) {
    ng.push(`[${theme}/${hubViewport.name}] 3 つの塊の矩形を取得できない`)
  } else {
    const [identityBox, actionsBox, episodesBox] = blocks
    const identityBottom = identityBox.y + identityBox.height
    const actionsBottom = actionsBox.y + actionsBox.height
    if (!(identityBox.y < actionsBox.y && actionsBox.y < episodesBox.y)) {
      ng.push(`[${theme}/${hubViewport.name}] 3 つの塊が上から識別・行動・エピソードの順でない`)
    }
    if (actionsBox.y - identityBottom < 24 || episodesBox.y - actionsBottom < 24) {
      ng.push(`[${theme}/${hubViewport.name}] 3 つの塊の間隔が 24px 未満`)
    }
  }
  const primary = actions.locator('[class~="bg-primary"]')
  if (await primary.count() !== 1) {
    ng.push(`[${theme}/${hubViewport.name}] 塗りの主ボタンが 1 つでない（${await primary.count()} 件）`)
  }
  const shot = path.join(OUT_DIR, `${hubViewport.name}-${theme}.png`)
  await page.screenshot({ path: shot })
  log(`  ${path.basename(shot)}`)
  await checkMissingStrings(ng, page, `${hubViewport.name}/${theme}`)

  const trigger = page.getByRole('button', { name: 'シリーズのその他の操作' })
  await trigger.click()
  const menu = page.getByRole('menuitem', { name: '分類を直す（割る・指定する）' })
  const menuVisible = await menu.waitFor({ timeout: 5000 }).then(() => true).catch(() => false)
  if (!menuVisible) {
    ng.push(`[${theme}/${hubViewport.name}] 分類を直すメニューが開かない`)
  } else {
    const menuBox = await menu.boundingBox()
    if (
      menuBox === null ||
      menuBox.x < 0 ||
      menuBox.x + menuBox.width > hubViewport.width ||
      menuBox.y < 0 ||
      menuBox.y + menuBox.height > hubViewport.height
    ) {
      ng.push(`[${theme}/${hubViewport.name}] 分類メニューがビューポートからはみ出す`)
    }
    const menuShot = path.join(OUT_DIR, `series-hub-menu-${hubViewport.name.replace('series-hub-', '')}-${theme}.png`)
    await page.screenshot({ path: menuShot })
    log(`  ${path.basename(menuShot)}`)
  }
  await context.close()

  // 主ボタンの隣に置いた「毎回録画する」が、検索結果とルール作成節を持つ
  // `/search?cond=...` へ実際に着地することも同じ Chromium で確認する。
  const searchContext = await open(hubViewport, theme, screenOf('series-hub'))
  const recurring = searchContext.page.getByRole('link', { name: '毎回録画する' })
  if ((await recurring.count()) === 0) {
    ng.push(`[${theme}/${hubViewport.name}] 「毎回録画する」リンクが見つからない`)
  } else {
    await recurring.click()
    const searchLoaded = await searchContext.page
      .waitForURL('**/search?cond=*', { timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!searchLoaded) {
      ng.push(`[${theme}/${hubViewport.name}] 「毎回録画する」の検索 URL に着地しない`)
    } else {
      const results = searchContext.page.getByRole('region', { name: '検索結果' })
      const createRule = searchContext.page.getByText('この条件でルールを作成').first()
      if ((await results.count()) === 0 || (await createRule.count()) === 0) {
        ng.push(`[${theme}/${hubViewport.name}] 検索結果またはルール作成節が表示されない`)
      }
    }
  }
  await searchContext.context.close()
}

// --- ①-B シリーズ一覧の格子とリスト ---
//
// 格子（2 列 / 4 列）とリストの切替、16:9 のサムネイル、横はみ出しの無さは
// レイアウトの実測でしか分からない（jsdom は測れない）。サムネイルは 404 に
// 落としてあるので、画像が無いときの代替表示（bg-muted。走査線にしない）も測る。
log('\n=== ①-B シリーズ一覧の格子とリスト ===')
for (const viewport of [mobile, desktop]) {
  for (const view of ['card', 'list']) {
    const label = `series/${view}/${viewport.name}`
    const { context, page } = await open(viewport, 'light', screenOf('series'), { recordingView: view })
    await page.screenshot({ path: path.join(OUT_DIR, `series-${view}-${viewport.name}.png`) })
    const m = await page.locator('[data-testid="series-shelf"]').evaluateAll((items) => {
      const rects = items.map((li) => li.getBoundingClientRect())
      const ratios = items.map((li) => {
        const r = li.querySelector('a > span').getBoundingClientRect()
        return r.width / r.height
      })
      const placeholder = items[0].querySelector('a > span > span')
      return {
        n: items.length,
        columns: new Set(rects.map((r) => Math.round(r.left))).size,
        maxRight: Math.max(...rects.map((r) => r.right)),
        ratios,
        docOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        placeholderClass: placeholder?.className ?? null,
        placeholderBg: placeholder ? getComputedStyle(placeholder).backgroundColor : null,
        metaSizes: [...document.querySelectorAll('[data-testid="series-shelf-meta"]')].map((el) => getComputedStyle(el).fontSize),
      }
    })
    log(`  ${label}: タイル=${m.n} 列=${m.columns} 右端=${m.maxRight.toFixed(1)} 比=${m.ratios[0]?.toFixed(3)}`)
    if (m.n !== 4) ng.push(`${label}: タイルが 4 件でない（${m.n}。NULL の棚は出さない）`)
    const expectedColumns = view === 'list' ? 1 : viewport === mobile ? 2 : 4
    if (m.columns !== expectedColumns) ng.push(`${label}: 列数が ${expectedColumns} でない（${m.columns}）`)
    if (m.docOverflow || m.maxRight > viewport.width + 0.5) ng.push(`${label}: 横にはみ出している（右端 ${m.maxRight}px）`)
    if (m.ratios.some((r) => Math.abs(r - 16 / 9) > 0.01)) ng.push(`${label}: サムネイルが 16:9 でない（${m.ratios.map((r) => r.toFixed(3)).join(', ')}）`)
    if (m.placeholderClass === null || /scanlines/.test(m.placeholderClass) || m.placeholderBg === 'rgba(0, 0, 0, 0)') {
      ng.push(`${label}: 画像が無いときの代替表示が bg-muted の塗りでない（class=${m.placeholderClass} bg=${m.placeholderBg}）`)
    }
    if (m.metaSizes.some((size) => size !== '14px')) ng.push(`${label}: メタが text-sm でない（${m.metaSizes.join(', ')}）`)
    await context.close()
  }
}

// ストレージ階層は既定で畳むため、展開状態も画面幅・テーマごとに別途撮る。
for (const viewport of viewports) {
  for (const theme of themes) {
    const { context, page } = await open(viewport, theme, screenOf('recordings'))
    await page.locator('main > header details summary').click()
    const file = path.join(OUT_DIR, `recordings-storage-${theme}-${viewport.name}.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `recordings-storage/${theme}/${viewport.name}`)
    await context.close()
  }
}
// 番組表グリッド（`lg` 以上でしか出ない。現在時刻線・容量超過の帯・ジャンル淡色が
// 一度に並ぶ画面）とブレーカー発動中（destructive の帯）は別途 1 枚ずつ。
for (const theme of themes) {
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) > 0) {
      await grid.first().click()
      await page.waitForTimeout(1200)
      const file = path.join(OUT_DIR, `programs-grid-${theme}-desktop.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await checkMissingStrings(ng, page, `programs-grid/${theme}`)
    } else {
      log(`  （programs-grid-${theme}: 表示形式の切り替えが出ていないので撮らない）`)
    }
    await context.close()
  }
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'), { withBreaker: true })
    const file = path.join(OUT_DIR, `breaker-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `breaker/${theme}`)
    await context.close()
  }
  {
    // issue #467 の罠: 詳細ページを PageHeader（sticky + `--sticky-banners-height`
    // の top）に乗せたので、ブレーカーバナー表示中のレイアウトが崩れていないかを
    // ここで見る（一覧と違う独自ヘッダを持っていたころは撮っていなかった）。
    const { context, page } = await open(desktop, theme, recordingDetailScreen, { withBreaker: true })
    const file = path.join(OUT_DIR, `breaker-recording-detail-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `breaker-recording-detail/${theme}`)
    await context.close()
  }
  {
    // 読み込み中（Skeleton / ListSkeleton の走査線）を撮る。API が即座に
    // 返る作りだと画面遷移からスクリーンショットの間に必ず解決してしまうので、
    // `/api/recordings` だけ遅延させる。`open()` の `wait` ロケータは
    // 解決後の状態を待つ設計なので、ここは自前でナビゲートする
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    await page
      .locator('.scanlines')
      .first()
      .waitFor({ timeout: 5000 })
      .catch(() => {
        ng.push(`[${theme}] 読み込み中の走査線（.scanlines）が出ない`)
      })
    const file = path.join(OUT_DIR, `loading-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `loading/${theme}`)
    await context.close()
  }
  {
    // 空状態（EmptyState）の文言が実際に読める位置のショット。検索フォームの下に
    // あるため、既定ショットだけではビューポートの下端で文言が切れることがある。
    // 読み込み中と同じ縞が付いていないこともここで確かめる。
    const { context, page } = await open(desktop, theme, screenOf('search'))
    const empty = page.getByText('条件を指定して検索してください', { exact: true })
    await empty.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 検索の未検索 EmptyState が出ない`)
    })
    if ((await empty.count()) > 0) {
      const hasScanlines = await empty.evaluate((element) => element.closest('.scanlines') !== null)
      if (hasScanlines) ng.push(`[${theme}] 検索の未検索 EmptyState に走査線がある`)
      await empty.scrollIntoViewIfNeeded()
    }
    await page.waitForTimeout(100)
    const file = path.join(OUT_DIR, `empty-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `empty/${theme}`)
    await context.close()
  }
  {
    // ホームの「全セクションが空」は EmptyState だけを表示する。
    // `home-*-desktop.png`（既定の 4 セクション表示）と対にして見比べられるようにする。
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ emptyHome: true }))
    await page.goto(URL_BASE + '/?mode=ops', { waitUntil: 'domcontentloaded' })
    const empty = page.getByText('表示できる項目がありません', { exact: true })
    await empty.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] ホームの空状態（全セクション空）が出ない`)
    })
    if ((await empty.count()) > 0) {
      const hasScanlines = await empty.evaluate((element) => element.closest('.scanlines') !== null)
      if (hasScanlines) ng.push(`[${theme}] ホームの EmptyState に走査線がある`)
    }
    const file = path.join(OUT_DIR, `home-empty-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `home-empty/${theme}`)
    await context.close()
  }
}

// issue #978: レビュー用に録画詳細の4状態をデスクトップ / モバイルで記録する。
for (const scenario of ['completed', 'recording', 'encode-waiting', 'trash']) {
  for (const viewport of viewports) {
    const pointer = viewport === mobile ? 'coarse' : 'fine'
    const { context, page } = await open(viewport, 'light', recordingDetailScreen, {
      pointer,
      multiSite: true,
      recordingDetailScenario: scenario,
    })
    const file = path.join(OUT_DIR, `recording-detail-${scenario}-${viewport.name}.jpg`)
    await page.evaluate(() => {
      for (const element of document.querySelectorAll('*')) {
        element.scrollTop = 0
      }
      document.scrollingElement?.scrollTo({ top: 0, left: 0, behavior: 'instant' })
      window.scrollTo({ top: 0, left: 0, behavior: 'instant' })
    })
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    const scrollPosition = await page.evaluate(() => ({
      window: window.scrollY,
      main: document.querySelector('main')?.scrollTop ?? 0,
    }))
    if (scrollPosition.window !== 0 || scrollPosition.main !== 0) {
      ng.push(
        `recording-detail/${scenario}/${viewport.name}: 撮影前のスクロール位置が先頭でない ` +
          `(window=${scrollPosition.window}, main=${scrollPosition.main})`,
      )
    }
    const pageHeadingTop = await page
      .getByRole('heading', { name: '録画の詳細' })
      .evaluate((element) => element.getBoundingClientRect().top)
      .catch(() => null)
    if (pageHeadingTop === null || pageHeadingTop < 0 || pageHeadingTop > viewport.height) {
      ng.push(
        `recording-detail/${scenario}/${viewport.name}: ページ見出しが撮影範囲にない ` +
          `(top=${pageHeadingTop ?? '取得不能'})`,
      )
    }
    const titleRow = page.locator('[data-testid="recording-title-row"]')
    if ((await titleRow.getByText('完了', { exact: true }).count()) > 0) {
      ng.push(`recording-detail/${scenario}/${viewport.name}: 見出しに完了バッジが残っている`)
    }
    if (scenario === 'encode-waiting' && (await titleRow.getByText('準備中', { exact: true }).count()) === 0) {
      ng.push(`recording-detail/${viewport.name}: エンコード待ちの録画に準備中が出ない`)
    }
    if (scenario === 'trash' && (await titleRow.getByText('再生不可', { exact: true }).count()) > 0) {
      ng.push(`recording-detail/trash/${viewport.name}: ごみ箱の見出しに結論バッジがある`)
    }
    await page.screenshot({
      path: file,
      type: 'jpeg',
      quality: 88,
      fullPage: viewport.name === 'desktop',
    })
    log(`  ${path.basename(file)}`)
    await checkMissingStrings(ng, page, `recording-detail/${scenario}/${viewport.name}`)
    await context.close()
  }
}

// Issue #1112: desktop / 360px のリスト・カードと録画詳細で結論の表示を確認する。
log('\n=== 録画結論: desktop / 360px、list / card ===')
for (const viewport of [desktop, mobile]) {
  for (const recordingView of ['list', 'card']) {
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), {
      recordingView,
      pointer: viewport === mobile ? 'coarse' : 'fine',
    })
    const row = page
      .locator('main li')
      .filter({ has: page.getByText('連続テレビ小説', { exact: true }) })
      .first()
    const rowReady = await row.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!rowReady) {
      ng.push(`recordings/${viewport.name}/${recordingView}: 対象の録画行が無い`)
    } else {
      if ((await row.getByText('準備中', { exact: true }).count()) === 0) {
        ng.push(`recordings/${viewport.name}/${recordingView}: 取り込み中の録画に準備中が出ない`)
      }
      if ((await row.getByText('完了', { exact: true }).count()) > 0) {
        ng.push(`recordings/${viewport.name}/${recordingView}: 視聴状態に完了バッジがある`)
      }
    }
    const documentWidth = await page.evaluate(() => document.documentElement.scrollWidth)
    if (documentWidth > viewport.width) {
      ng.push(`recordings/${viewport.name}/${recordingView}: 横はみ出し ${documentWidth}px`)
    }
    const file = path.join(OUT_DIR, `recording-verdict-${viewport.name}-${recordingView}.png`)
    await page.screenshot({ path: file, fullPage: true })
    log(`  ${path.basename(file)}`)
    await context.close()
  }
}

// --- ①-A' issue #686: 視聴対象への到達距離 -------------------------------
//
// 変更前の基準値は 360/390px が録画詳細リンクの viewport 上端約 317px、
// デスクトップが約 245px だった。ここでは固定時刻・同じ API モックで状態を分け、
// ①リンクの viewport Y、②固定ヘッダー/ボトムナビに隠れないこと、③必要スクロール量を
// 録画一覧（/recordings）で測る。管理モードのホームから完了録画への到達は
// design-home.mjs の「ホーム管理モード: 完了録画への到達」が測る。
// 数値は docs/frontend/recordings.md にも結果として記録するが、合否の権威はここ。
log('\n=== ①-A\' issue #686 視聴対象への到達距離 ===')
const layoutScenarios = [
  { name: 'normal', label: '正常' },
  { name: 'capacity', label: '満杯見込み' },
  { name: 'stale', label: '古い観測' },
  { name: 'storage-failure', label: 'ストレージ取得失敗' },
  { name: 'no-observation', label: '観測なし' },
  { name: 'encode-queue', label: 'エンコード待機/実行中/確認待ち' },
  { name: 'many-warnings', label: '警告多数' },
]
const layoutViewports = [desktop, mobile, mobileWide]
const layoutMetrics = new Map()

for (const scenario of layoutScenarios) {
  for (const viewport of layoutViewports) {
    const { context, page } = await open(viewport, 'light', screenOf('recordings'), {
      layoutScenario: scenario.name,
    })
    const target = page.locator('main a[href^="/recordings/"]').first()
    await target.waitFor({ timeout: 5000 }).catch(() => {})
    if ((await target.count()) === 0) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 最初の録画詳細リンクが無い`)
      await context.close()
      continue
    }
    const targetMetrics = await target.evaluate((el) => {
      const targetRect = el.getBoundingClientRect()
      const headerRect = document.querySelector('main > header')?.getBoundingClientRect()
      const bottomRect = document.querySelector('[data-testid="bottom-nav"]')?.getBoundingClientRect()
      const visibleTop = headerRect?.bottom ?? 0
      const visibleBottom = window.innerHeight - (bottomRect?.height ?? 0)
      return {
        y: targetRect.top,
        bottom: targetRect.bottom,
        headerBottom: visibleTop,
        visibleBottom,
        requiredScroll: Math.max(0, targetRect.bottom - visibleBottom, visibleTop - targetRect.top),
      }
    })
    // 管理情報行は常に DOM に存在し、`empty:hidden`（recordings.tsx）が子ノード
    // 0 個のときだけ `display: none` にする。`boundingBox()` は非表示要素に
    // 対して null を返す（Playwright の契約）ので、存在確認を挟まず直接呼べる。
    const managementSummary = page.locator('[data-testid="recordings-management-summary"]')
    const managementBox = await managementSummary.boundingBox()
    const summaryLocator = page.locator('main > header details summary').first()
    const summaryText = (await summaryLocator.count()) > 0
      ? await summaryLocator.textContent()
      : ''
    const metric = {
      ...targetMetrics,
      managementHeight: managementBox?.height ?? null,
      summary: summaryText?.replaceAll(/\s+/g, ' ').trim() ?? '',
    }
    layoutMetrics.set(`${scenario.name}/${viewport.name}`, metric)
    log(
      `  録画一覧/${scenario.label}/${viewport.name}: ` +
        `Y=${metric.y.toFixed(1)}px scroll=${metric.requiredScroll.toFixed(1)}px ` +
        `管理行=${metric.managementHeight?.toFixed(1) ?? '—'}px`,
    )
    if (metric.y < metric.headerBottom - 0.5) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 録画リンクが固定ヘッダーに隠れる`)
    }
    if (metric.bottom > metric.visibleBottom + 0.5) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 録画リンクがボトムナビに隠れる`)
    }
    if (scenario.name === 'normal') {
      const yLimit = viewport.name === 'desktop' ? 230 : 280
      if (metric.y > yLimit) {
        ng.push(
          `録画一覧/正常/${viewport.name}: 正常時の到達距離が短縮されていない` +
            `（Y=${metric.y.toFixed(1)}px、上限 ${yLimit}px）`,
        )
      }
      if (metric.managementHeight === null || metric.managementHeight > 45) {
        ng.push(
          `録画一覧/正常/${viewport.name}: 管理情報が 1 行に収まっていない` +
            `（${metric.managementHeight?.toFixed(1) ?? '—'}px）`,
        )
      }
      if (metric.summary.includes('の見込み') || metric.summary.includes('観測:')) {
        ng.push(`録画一覧/正常/${viewport.name}: 正常時の予測/観測を summary に常置している`)
      }
      // B: 展開時のレイアウト崩れ（`open:basis-full` / 展開グリッドの二重
      // パディング）はスクリーンショット（撮るだけ）にしか触れていなかったので、
      // 実際に開いて機械判定する。デスクトップは幅に余裕がありこの崩れ方が
      // 再現しないため、崩れの実測対象だった 360/390px だけで見る。
      if (viewport.name !== 'desktop') {
        await page.locator('main > header details summary').first().click()
        const opened = await page.evaluate(() => {
          const details = document.querySelector('main > header details')
          const management = document.querySelector('[data-testid="recordings-management-summary"]')
          // StorageRootCapacity のカード内 <dl>（総容量/使用済み/空きの 3 列）。
          // 崩れると `<dd>` の内容（例: 「813.7 GB」）が列幅に収まらず折り返す
          // か、行自体が横に溢れる。
          const dl = details?.querySelector('dl') ?? null
          const dds = dl ? [...dl.querySelectorAll('dd')] : []
          // flex item（details）が実際に占有できる幅は管理情報行の border-box
          // 幅ではなく content-box 幅（`px-4` の左右パディングを除いた分）。
          // border-box の幅同士を比べると、パディング分（32px）を「占有できて
          // いない」と誤検出する。
          const managementStyle = management ? getComputedStyle(management) : null
          const managementContentWidth =
            management && managementStyle
              ? management.clientWidth -
                parseFloat(managementStyle.paddingLeft) -
                parseFloat(managementStyle.paddingRight)
              : null
          // 展開グリッド（details の直下、StorageRootCapacity を並べる div）自身の
          // 左右パディング。二重パディング（外側 recordings-management-summary の
          // px-4 + ここの px-4）は、このフィクスチャの桁数だと 3 列 dl が
          // 折り返す/溢れるところまでは追い込めない（幅に余裕がある）ので、
          // 症状（折り返し・溢れ）だけでなく実装がパディングを持たせていないこと
          // 自体も直接測る。
          const grid = details?.querySelector(':scope > div.grid') ?? null
          const gridStyle = grid ? getComputedStyle(grid) : null
          return {
            detailsWidth: details?.getBoundingClientRect().width ?? null,
            managementContentWidth,
            dlOverflow: dl ? dl.scrollWidth > dl.clientWidth : null,
            // text-xs の 1 行の高さは実測で 16px 程度。2 行に折り返すと
            // 目に見えて超える。
            maxDdHeight: dds.length > 0 ? Math.max(...dds.map((d) => d.getBoundingClientRect().height)) : null,
            gridPaddingLeft: gridStyle ? parseFloat(gridStyle.paddingLeft) : null,
            gridPaddingRight: gridStyle ? parseFloat(gridStyle.paddingRight) : null,
          }
        })
        if (
          opened.detailsWidth === null ||
          opened.managementContentWidth === null ||
          Math.abs(opened.detailsWidth - opened.managementContentWidth) > 1
        ) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 開いた詳細が管理情報行の幅いっぱいを占有していない` +
              `（details=${opened.detailsWidth?.toFixed(1) ?? '—'}px, 行の内容幅=${opened.managementContentWidth?.toFixed(1) ?? '—'}px）`,
          )
        }
        if (opened.dlOverflow !== false) {
          ng.push(`録画一覧/正常/${viewport.name}: 展開したカードの内容が横に溢れている`)
        }
        if (opened.maxDdHeight === null || opened.maxDdHeight > 20) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 展開したカードの値が折り返している` +
              `（${opened.maxDdHeight?.toFixed(1) ?? '—'}px）`,
          )
        }
        if (opened.gridPaddingLeft !== 0 || opened.gridPaddingRight !== 0) {
          ng.push(
            `録画一覧/正常/${viewport.name}: 展開グリッドが compact でも px-4 を持ち、` +
              `外側の管理情報行の px-4 と二重になっている` +
              `（left=${opened.gridPaddingLeft ?? '—'}px, right=${opened.gridPaddingRight ?? '—'}px）`,
          )
        }
      }
    }
    if (scenario.name === 'capacity' && !metric.summary.includes('満杯見込み')) {
      ng.push(`録画一覧/満杯見込み/${viewport.name}: 満杯見込みが summary に常置されていない`)
    }
    if ((scenario.name === 'stale' || scenario.name === 'many-warnings') && !metric.summary.includes('古い可能性')) {
      ng.push(`録画一覧/${scenario.label}/${viewport.name}: 古い観測が summary に常置されていない`)
    }
    if (scenario.name === 'encode-queue') {
      // アクセシブル名は表示テキストに従う。sm（640px）未満は「待機 / 実行」の短い名前。
      const narrow = viewport.width < 640
      for (const label of [narrow ? '待機 2件' : '待機中 2件', narrow ? '実行 1件' : '実行中 1件', '確認待ち 1件']) {
        if ((await page.getByRole('button', { name: label, exact: true }).count()) === 0) {
          ng.push(`録画一覧/エンコード待機列/${viewport.name}: ${label} が無い`)
        }
      }
    }
    if (scenario.name === 'storage-failure') {
      // ストレージ取得だけが失敗し、エンコード件数（0/0/0）は解決するケース。
      // `summaryLocator` が 0 件（= StorageBalance が描かれない）だと否定側の
      // 判定が自明に通ってしまう（管理行が丸ごと消えても緑になる指摘）ので、
      // 肯定側（チップが残っていること）と否定側（欠損した容量情報を出さない
      // こと）の両方を測る。
      if ((await page.getByRole('button', { name: viewport.width < 640 ? '待機 0件' : '待機中 0件', exact: true }).count()) === 0) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: エンコードチップが残っていない`)
      }
      const headerText = (
        (await page.locator('main > header').textContent()) ?? ''
      ).replaceAll(/\s+/g, ' ')
      if (headerText.includes('空き') || headerText.includes('の見込み')) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: 欠損した容量情報を表示している`)
      }
    }
    if (scenario.name === 'no-observation') {
      // エンコード件数の取得も失敗させ、StorageBalance と両方が何も描かない
      // 組み合わせにしてある（apiHandler 参照）。管理情報の帯（recordings.tsx の
      // `recordings-management-summary`）は `empty:hidden` で DOM には残るが
      // 子ノード 0 個で非表示になる想定なので、DOM の有無ではなく可視性を測る
      // （count() は常に 1 を返すので判定にならない）。
      if (await page.locator('[data-testid="recordings-management-summary"]').isVisible()) {
        ng.push(`録画一覧/${scenario.label}/${viewport.name}: 両方欠損時に空の管理情報帯が残っている`)
      }
    }
    await context.close()
  }
}

// --- ①-A 広幅の本文と一覧の文字サイズ ---
//
// jsdom は幅も継承後の実フォントサイズも測れない。2560px の実ブラウザで、一覧本文が
// サイドバーを除く main の幅いっぱいに広がることを全対象画面で見る。
// 題名と副情報は各画面の既定フィクスチャから実要素を掴み、計算済み px 値を測る。
log('\n=== ①-A 広幅の本文と一覧の文字サイズ ===')
const boundedListScreens = [
  { screen: 'recordings', title: 'ニュース７', secondary: 'NHK総合' },
  { screen: 'reservations', title: '連続テレビ小説', secondary: 'NHKEテレ' },
  { screen: 'series', title: '作品X', secondary: 'アニメ　作品X　第2話' },
  { screen: 'rules', title: '朝ドラ', secondary: '番組名に「連続テレビ小説」を含む' },
  { screen: 'programs', title: 'ニュース７', secondary: 'NHK総合' },
]
for (const spec of boundedListScreens) {
  const { context, page } = await open(desktop, 'light', screenOf(spec.screen))
  const content = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const contentBox = (await content.count()) === 0 ? null : await content.boundingBox()
  const mainBox = await page.locator('main').boundingBox()
  if (contentBox === null || mainBox === null) {
    ng.push(`${spec.screen}: 本文コンテナが見つからない`)
    await context.close()
    continue
  } else {
    log(`  ${spec.screen}: x=${contentBox.x}, width=${contentBox.width}`)
    if (
      Math.abs(contentBox.x - mainBox.x) > 0.5 ||
      Math.abs(contentBox.width - mainBox.width) > 0.5
    ) {
      ng.push(
        `${spec.screen}: 本文が main 幅いっぱいでない` +
          `（main x=${mainBox.x}, width=${mainBox.width}; 本文 x=${contentBox.x}, width=${contentBox.width}）`,
      )
    }
  }

  const title = content.getByText(spec.title, { exact: true }).first()
  const secondary = content.getByText(spec.secondary, { exact: true }).first()
  // 要素が無い（セレクタが腐った・描画されない）場合を、`.catch(() => null)` で
  // フォントサイズの読み取り失敗に化けさせない。found を待ちの成否そのもので
  // 分岐し、見つからないときは found=false の NG を出して終える（issue #550。
  // 直す前は evaluate() が catch(() => null) を返し、`null !== '16px'` が真になって
  // 「題名が text-base でない（null）」というスタイル回帰と同じ文言の NG になっていた）。
  const [titleFound, secondaryFound] = await Promise.all([
    title
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false),
    secondary
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false),
  ])
  if (!titleFound) ng.push(`${spec.screen}: 一覧の題名（${spec.title}）が見つからない`)
  if (!secondaryFound) ng.push(`${spec.screen}: 一覧の副情報（${spec.secondary}）が見つからない`)
  const titleSize = titleFound ? await title.evaluate((el) => getComputedStyle(el).fontSize) : null
  const secondarySize = secondaryFound
    ? await secondary.evaluate((el) => getComputedStyle(el).fontSize)
    : null
  log(`  ${spec.screen}: 題名=${titleSize ?? '未取得'}, 副情報=${secondarySize ?? '未取得'}`)
  if (titleFound && titleSize !== '16px') {
    ng.push(`${spec.screen}: 一覧の題名が text-base でない（${titleSize}）`)
  }
  if (secondaryFound && secondarySize !== '14px') {
    ng.push(`${spec.screen}: 一覧の副情報が text-sm でない（${secondarySize}）`)
  }
  await context.close()
}

for (const screenName of ['home', 'series-hub']) {
  const { context, page } = await open(
    desktop,
    'light',
    screenOf(screenName),
    screenName === 'home' ? { homeModeFixture: true } : {},
  )
  const content = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const contentBox = (await content.count()) === 0 ? null : await content.boundingBox()
  const mainBox = await page.locator('main').boundingBox()
  if (contentBox === null || mainBox === null) {
    ng.push(`${screenName}: 本文コンテナが見つからない`)
  } else if (
    Math.abs(contentBox.x - mainBox.x) > 0.5 ||
    Math.abs(contentBox.width - mainBox.width) > 0.5
  ) {
    ng.push(`${screenName}: 本文が main 幅いっぱいでない（main=${mainBox.width}px, 本文=${contentBox.width}px）`)
  }
  await context.close()
}

// 広い番組リストは説明列を使い、説明の有無にかかわらず左端と行高を揃える。
{
  const viewports = [
    { name: 'desktop-1280', width: 1280, height: 900 },
    desktop,
  ]
  const measured = new Map()
  for (const viewport of viewports) {
    const { context, page } = await open(viewport, 'light', screenOf('programs'))
    const rows = page.locator('li[data-program-id]')
    const rowsReady = await rows.first().waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
    if (!rowsReady) {
      ng.push(`programs/${viewport.name}: 番組行が見つからない`)
      await context.close()
      continue
    }
    const rowMetrics = await rows.evaluateAll((items) =>
      items.slice(0, 8).flatMap((item) => {
        const row = item.querySelector('[data-testid="program-row"]')
        const description = item.querySelector('[data-testid="program-row-description"]')
        if (!row) return []
        const rowRect = row.getBoundingClientRect()
        const descriptionRect = description?.getBoundingClientRect()
        return [{
          id: item.getAttribute('data-program-id'),
          height: rowRect.height,
          descriptionX: descriptionRect?.width ? descriptionRect.x : null,
          descriptionWidth: descriptionRect?.width ?? 0,
        }]
      }),
    )
    measured.set(viewport.width, rowMetrics)
    if (viewport.width === desktop.width) {
      const descriptions = rowMetrics.filter((row) => row.descriptionX !== null)
      if (descriptions.length < 2) {
        ng.push('programs/2560: 番組説明の列が 2 行以上で表示されていない')
      } else {
        const leftEdges = descriptions.map((row) => row.descriptionX)
        if (Math.max(...leftEdges) - Math.min(...leftEdges) > 0.5) {
          ng.push(`programs/2560: 番組説明列の左端が揃っていない（${leftEdges.join(', ')}）`)
        }
      }
    }
    await context.close()
  }

  const normalRows = new Map((measured.get(1280) ?? []).map((row) => [row.id, row]))
  const wideRows = measured.get(2560) ?? []
  for (const row of wideRows) {
    const normal = normalRows.get(row.id)
    if (normal && Math.abs(row.height - normal.height) > 0.5) {
      ng.push(
        `programs/${row.id}: 説明列で番組行の高さが変わった（1280=${normal.height}px, 2560=${row.height}px）`,
      )
    }
  }
}

// カードの幅を上限し、2560px では固定 4 列より多く並べる。
for (const screenName of ['series', 'recordings']) {
  const { context, page } = await open(desktop, 'light', screenOf(screenName))
  const cardToggle = page.getByRole('button', { name: 'カード表示', exact: true })
  if ((await cardToggle.count()) === 0) {
    ng.push(`${screenName}/2560: カード表示への切り替えが無い`)
    await context.close()
    continue
  }
  if ((await cardToggle.getAttribute('aria-pressed')) !== 'true') await cardToggle.click()
  const cards = page.locator(
    screenName === 'series' ? '[data-testid="series-shelf"]' : '[data-testid="recording-card"]',
  )
  const cardsReady = await cards.first().waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  const boxes = cardsReady ? await cards.evaluateAll((items) => items.map((item) => {
    const { x, y, width, height } = item.getBoundingClientRect()
    return { x, y, width, height }
  })) : []
  if (!cardsReady || boxes.length === 0) {
    ng.push(`${screenName}/2560: カードが見つからない`)
  } else {
    const maxWidth = Math.max(...boxes.map((box) => box.width))
    const rows = new Set(boxes.map((box) => Math.round(box.y)))
    log(`  ${screenName}/2560: cards=${boxes.length}, max-width=${maxWidth}px, rows=${rows.size}`)
    if (maxWidth > 360.5) {
      ng.push(`${screenName}/2560: カード幅 ${maxWidth}px が 360px の上限を超えている`)
    }
    if (boxes.length > 4 && rows.size > 1) {
      ng.push(`${screenName}/2560: 4 列を超えたカードが複数行になっている`)
    }
  }
  await page.screenshot({ path: path.join(OUT_DIR, `${screenName}-cards-light-desktop.png`) })
  await context.close()
}

// ライブ外枠の広幅判定。受け入れ寸法（映像 + 情報 3 行、一覧との隙間なし、
// 複数列、sticky）を実ブラウザで測る。
for (const viewport of [
  { name: 'live-1920', width: 1920, height: 1080 },
  desktop,
]) {
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(
    viewport,
    'light',
    liveScreen,
    { wideLiveFixture: true },
  )
  const preview = page.getByRole('button', { name: 'NHK総合を再生', exact: true })
  const playerColumn = preview.locator('xpath=..')
  const channelList = page.getByRole('navigation', { name: 'チャンネル一覧' })
  const ready = await preview.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  if (!ready || (await channelList.count()) === 0) {
    ng.push(`live/${viewport.width}: 映像またはチャンネル一覧が見つからない`)
    await context.close()
    continue
  }
  const station = page.locator('main').getByText('NHK総合', { exact: true }).first()
  const programLine = page.locator('main p.text-xl.font-semibold').first()
  const timeLine = page.locator('main p').filter({ hasText: /予定:/ }).first()
  const [stationFound, programFound, timeFound] = await Promise.all([
    station.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
    programLine.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
    timeLine.waitFor({ timeout: 5000 }).then(() => true).catch(() => false),
  ])
  const [videoBox, channelBox, stationBox, programBox, timeBox] = await Promise.all([
    preview.boundingBox(),
    channelList.boundingBox(),
    stationFound ? station.boundingBox() : Promise.resolve(null),
    programFound ? programLine.boundingBox() : Promise.resolve(null),
    timeFound ? timeLine.boundingBox() : Promise.resolve(null),
  ])
  if (videoBox === null || channelBox === null || stationBox === null || programBox === null || timeBox === null) {
    ng.push(`live/${viewport.width}: 映像と局名・番組名・時刻の位置を測れない`)
  } else {
    if (
      videoBox.y + videoBox.height > viewport.height ||
      stationBox.y + stationBox.height > viewport.height ||
      programBox.y + programBox.height > viewport.height ||
      timeBox.y + timeBox.height > viewport.height
    ) {
      ng.push(`live/${viewport.width}: 映像または局名・番組情報が初期 viewport に収まらない`)
    }
    // #1022 のラフの列間隔は 20px。映像の幅上限で一覧が右へ離れる（旧 max-w-3xl）のを弾く
    const gap = channelBox.x - (videoBox.x + videoBox.width)
    if (gap < 0 || gap > 24) {
      ng.push(`live/${viewport.width}: 映像とチャンネル一覧の間隔が 0〜24px に収まらない（${gap}px）`)
    }
    log(`  live/${viewport.width}: video=${videoBox.width}×${videoBox.height}, channel-x=${channelBox.x}`)
  }

  const serviceItems = channelList.locator('ul ul').first().locator(':scope > li')
  if (viewport.width === desktop.width) {
    const columnXs = await serviceItems.evaluateAll((items) =>
      [...new Set(items.map((item) => Math.round(item.getBoundingClientRect().x)))],
    )
    if (columnXs.length < 2) ng.push('live/2560: チャンネル一覧が複数列になっていない')

    const scrollRange = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)
    if (scrollRange < 500) {
      ng.push(`live/2560: sticky を判定するスクロール量が足りない（${scrollRange}px）`)
    } else {
      await page.evaluate(() => window.scrollTo({ top: 600, behavior: 'instant' }))
      await page.waitForTimeout(100)
      const [playerAfterScroll, headerAfterScroll] = await Promise.all([
        playerColumn.boundingBox(),
        page.locator('main > header').boundingBox(),
      ])
      const stickyPosition = await playerColumn.evaluate((element) => getComputedStyle(element).position)
      if (
        playerAfterScroll === null ||
        headerAfterScroll === null ||
        stickyPosition !== 'sticky' ||
        Math.abs(playerAfterScroll.y - headerAfterScroll.y - headerAfterScroll.height) > 1
      ) {
        ng.push('live/2560: スクロール後もプレイヤー列がヘッダー直下に sticky されていない')
      }
      await page.evaluate(() => window.scrollTo(0, 0))
    }
  }
  await page.screenshot({ path: path.join(OUT_DIR, `${viewport.name}-light.png`) })
  await context.close()
}

// 高さが足りない desktop viewport でもライブ映像をゼロ幅にしない。
{
  const viewport = { name: 'live-short', width: 1366, height: 400 }
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(viewport, 'light', liveScreen, { wideLiveFixture: true })
  const preview = page.getByRole('button', { name: 'NHK総合を再生', exact: true })
  const ready = await preview.waitFor({ timeout: 10000 }).then(() => true).catch(() => false)
  const videoBox = ready ? await preview.boundingBox() : null
  if (videoBox === null || videoBox.width < 320 || videoBox.height < 180) {
    ng.push(`live/1366x400: 映像プレビューが表示可能な大きさでない（${videoBox?.width ?? 0}×${videoBox?.height ?? 0}px）`)
  }
  await page.screenshot({ path: path.join(OUT_DIR, 'live-short-light.png') })
  await context.close()
}

// 1280px の一覧ラフ比較用。自動判定だけでなく、1 行目・カード密度・右端の余白を
// 2560px のショットと並べて確認する。
for (const screenName of ['programs', 'reservations', 'recordings', 'rules', 'series', 'series-hub']) {
  const viewport = { name: 'desktop-1280', width: 1280, height: 900 }
  const { context, page } = await open(viewport, 'light', screenOf(screenName))
  const file = path.join(OUT_DIR, `${screenName}-light-desktop-1280.png`)
  await page.screenshot({ path: file })
  log(`  ${path.basename(file)}`)
  await context.close()
}

// #1022 の合意済みライブ画面ラフ（幅約 1180px）との比較対象。
{
  const viewport = { name: 'live-rough', width: 1180, height: 900 }
  const liveScreen = {
    ...screenOf('live'),
    path: `/live?service=${services[0].id}&site=${SITE}`,
  }
  const { context, page } = await open(viewport, 'light', liveScreen)
  const file = path.join(OUT_DIR, 'live-light-rough-1180.png')
  await page.screenshot({ path: file })
  log(`  ${path.basename(file)}`)
  await context.close()
}

// 同じ /programs でも番組表グリッドは横幅が情報量なので、本文上限を適用しない。
{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  const gridTrigger = page.getByRole('button', { name: '番組表' })
  if ((await gridTrigger.count()) === 0) {
    ng.push('programs-grid: 表示形式の切り替えが無い')
  } else {
    await gridTrigger.click()
    const grid = page.locator('[data-testid="program-grid"]')
    const gridReady = await grid
      .waitFor({ timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!gridReady) {
      // 待ちの失敗をここで飲んで先へ進むと、以下の「1024px 以下に制限されている」
      // という**スタイル回帰の NG と同じ文言**で「そもそも描画されていない」ことが
      // 報告されてしまう（issue #521 と同じ壊れ方）。加えて要素が 0 件のまま
      // `grid.evaluate(...)` へ進むと、そちらは `locator.evaluate` 自身の既定の
      // 30 秒待った末に例外で落ちる（測定: `locator.evaluate threw after 30006 ms:
      // Timeout 30000ms exceeded`。NG リストに積まれず何も判定していない状態で
      // 終わる）。ここで打ち切って区別できる文言を出す。
      ng.push('programs-grid: 番組表グリッドが描画されない（待ちがタイムアウト）')
    } else {
      const gridBox = await grid.boundingBox()
      if ((await page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]').count()) > 0) {
        ng.push('programs-grid: 番組表グリッドに一覧本文の幅上限が適用されている')
      }
      if (gridBox === null || gridBox.width <= 1024.5) {
        ng.push(`programs-grid: 番組表グリッドが 1024px 以下に制限されている（${gridBox?.width ?? '未取得'}px）`)
      }

      const columnMetrics = await grid.evaluate((element) => {
        const columns = [...element.querySelectorAll('[data-testid="program-grid-column"]')]
        const gridRect = element.getBoundingClientRect()
        const firstRect = columns[0]?.getBoundingClientRect()
        return {
          count: columns.length,
          width: firstRect?.width ?? 0,
          availableWidth: firstRect ? element.clientWidth - (firstRect.left - gridRect.left) : 0,
        }
      })
      const expectedColumnWidth = Math.min(
        260,
        Math.max(176, columnMetrics.availableWidth / services.length),
      )
      if (
        columnMetrics.count !== services.length ||
        Math.abs(columnMetrics.width - expectedColumnWidth) > 0.5
      ) {
        ng.push(
          `programs-grid: 4 局の列幅が画面幅に追従していない（実測 ${columnMetrics.width}px、期待 ${expectedColumnWidth}px）`,
        )
      }
    }
  }
  await context.close()
}

// ルール作成はモバイルだけ本文全幅、lg 以上では PageHeader の右端に置く。
{
  const { context, page } = await open(desktop, 'light', screenOf('rules'))
  const create = page.locator('header').getByRole('link', { name: 'ルールを作成', exact: true })
  const createBox = (await create.count()) === 0 ? null : await create.boundingBox()
  if (createBox === null) {
    ng.push('rules/desktop: PageHeader に「ルールを作成」が無い')
  } else if (createBox.width > 200) {
    ng.push(`rules/desktop: 「ルールを作成」が内容幅でない（${createBox.width}px）`)
  }

  // issue #728: 主編集導線はルール名そのもの（`variant="link"` = `text-primary`）
  // で、「編集」という primary 塗りのボタンはもう無い。**背景色ではなく
  // 文字色**が primary トークンと一致することを見る --- `text-primary` /
  // `bg-primary` はどちらも Tailwind の `--color-primary`（= `--primary`）を
  // 参照するので、リンクの文字色と作成ボタンの背景色は同じ rgba になる。
  const editColor = await computedOf(
    page.getByRole('link', { name: /^ルール「.+」を編集$/ }).first(),
    'color',
  )
  const createBg = await computedOf(create, 'background-color')
  if (
    editColor === null ||
    createBg === null ||
    !editColor.rgba.every((value, index) => value === createBg.rgba[index])
  ) {
    ng.push('rules/desktop: ルール名の編集リンクが primary の文字色でない')
  }
  await context.close()
}
{
  const { context, page } = await open(mobile, 'light', screenOf('rules'))
  const headerCreate = page.locator('header').getByRole('link', { name: 'ルールを作成', exact: true })
  if ((await headerCreate.count()) > 0) {
    ng.push('rules/mobile: PageHeader に「ルールを作成」が出ている')
  }
  const mobileContent = page.locator('[data-testid="page-content"], [data-testid="bounded-page-content"]')
  const mobileCreate = mobileContent.getByRole('link', { name: 'ルールを作成', exact: true })
  const contentBox =
    (await mobileContent.count()) === 0 ? null : await mobileContent.boundingBox()
  const createBox =
    (await mobileContent.count()) === 0 || (await mobileCreate.count()) === 0
      ? null
      : await mobileCreate.boundingBox()
  const padding =
    (await mobileContent.count()) === 0
      ? null
      : await mobileContent.evaluate((el) => {
          const style = getComputedStyle(el)
          return {
            left: Number.parseFloat(style.paddingLeft),
            right: Number.parseFloat(style.paddingRight),
          }
        })
  if (contentBox === null || createBox === null || padding === null) {
    ng.push('rules/mobile: 「ルールを作成」の全幅を測れない')
  } else {
    const expectedX = contentBox.x + padding.left
    const expectedWidth = contentBox.width - padding.left - padding.right
    if (Math.abs(createBox.x - expectedX) > 0.5 || Math.abs(createBox.width - expectedWidth) > 0.5) {
      ng.push(
        `rules/mobile: 「ルールを作成」が本文の全幅でない` +
          `（x=${createBox.x}, width=${createBox.width}, 期待 x=${expectedX}, width=${expectedWidth}）`,
      )
    }
  }
  await context.close()
}

await finish(ng, browser)
