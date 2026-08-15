// 番組リストの予約 / 取消ボタンの可視性判定（issue #310）。
//
// **CSS の `:hover` / `:focus-visible` / `pointer:` メディア特性で駆動する開閉は
// jsdom では測れない**（web/e2e/README.md「jsdom が測れないもの」）。jsdom は
// レイアウトを持たず実幅を 0 と返すので、`pnpm test` は「常時開いたまま」という
// クラス名の変異を検出できない。
//
// **開閉の実装手段は列の幅（`w-0` ↔ `w-20`）+ `overflow-hidden` であって
// `opacity` ではない。** 予約列は常時 w-20 を確保せず、ボタンを立てる段になって
// 幅を開く（旧版は常時 w-20 を確保して `visibility` だけ切り替えていたが、
// ボタンが出ていない間の空きが不恰好なので畳む方式に変えた。docs/frontend/
// reservations.md）。開くと行トグル（flex-1）が縮み、その右端のシェブロンが
// 左へスライドしてボタンのスペースを空ける。
//
// 幅 0 の領域はヒットテストの標的を持たないので、折りたたみ行の予約列を生座標で
// タップしても予約は成立しない。最初の実装（別の PR）は `opacity-0` を使い、
// レビューで「折りたたみ行の右端 80×56px が、見た目には無いのに実際にはタップ
// 可能なままで、生の座標へのタップで予約が成立してしまう」欠陥が見つかった。
// 幅 0 + overflow-hidden はその標的自体を消すので、判定 ④ は「折りたたみ行の
// 予約列を生座標でタップしても PUT が飛ばない」ことも見る --- これが実際に
// 見逃していた欠陥そのもの。
//
// 見るのは 4 状態（すべて予約列の実レイアウト幅 `getBoundingClientRect().width`
// で判定する。畳＝約 0px、開＝約 80px）:
//   ① 細ポインタ（既定の Chromium コンテキスト = hover:hover + pointer:fine）で
//      ホバーもフォーカスもしていない行 --- 畳んでいる。ホバー / :focus-visible で
//      開く（両方向）。あわせて**縦方向の CLS が無いこと**（行の高さ不変）も測る
//      --- 横方向（列幅・タイトルの truncate 位置）は開閉で動くのが本設計の
//      仕様なので、ここではむしろ「開いた（幅が 0→約 80 に増えた）」ことを確かめる
//   ② 細ポインタで行を展開すると、その後マウス / フォーカスが行ヘッダから
//      離れても予約列は開いたまま（展開パネルは `.group` の外の兄弟なので
//      `group-hover` / `group-has-[:focus-visible]` はそこから発火しない ---
//      `peer-aria-expanded` を pointer 種別で縛らないことで担保する）
//   ③ タッチ / 粗いポインタ（hover:none + pointer:coarse）で、折りたたみ行は
//      畳んだまま・展開した行（aria-expanded=true）だけ開く。加えて、粗い
//      ポインタでもキーボード（行トグルへの :focus-visible）だけで開くこと
//      （タブレット + 外付けキーボードの想定。WCAG 2.4.7 / 2.4.11）
//   ④ タッチ / 粗いポインタで、折りたたみ行の右端（開いたら予約列が来る位置）を
//      実座標へ生のタップ（`page.touchscreen.tap`。ロケータ経由のアクショナ
//      ビリティを迂回する）で押しても PUT intent が飛ばない・トーストも出ない。
//      あわせて `elementFromPoint` で、その座標のヒットテスト標的が予約ボタン
//      ではない（畳んでいるので行トグル側に属する）ことも確かめる
//
// 別ファイルにしたのは、design.mjs が既に ⑥（Button のフォーカスリング遷移。
// issue #294）を持ち、他 PR がそこを編集している可能性があるため
// （web/e2e/README.md 冒頭のブリーフ参照）。この判定は Button ではなく
// ProgramRow 固有の関心なので、独立させても design.mjs の合否には影響しない。
//
// mirakc も実チューナーも DB も要らない。API は `page.route` で丸ごと差し替える
// （design.mjs / live.mjs と同じ手）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/reserve-visibility.mjs
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'

const ng = []
const log = (...a) => console.log(...a)

const program = {
  programId: 500001,
  networkId: 32736,
  serviceId: 1024,
  eventId: 1,
  // 固定時刻（下記 FIXED_NOW）より先の未放送番組。ON AIR 判定やライブ導線と
  // 無関係にするため、放送中にはしない。
  startAt: '2026-08-13T01:00:00.000Z',
  endAt: '2026-08-13T02:00:00.000Z',
  durationMs: 3_600_000,
  name: 'e2e 可視性確認番組',
  description: '',
  genres: [0],
  isFree: true,
}

const FIXED_NOW = new Date('2026-08-13T00:00:00.000Z')

/** installApiStubs は /programs の描画に要る `/api/**` を丸ごと差し替える。 */
async function installApiStubs(page) {
  await page.route('**/api/**', async (route) => {
    const p = new URL(route.request().url()).pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: false })
    if (p === '/api/reservations') return json([])
    if (p === '/api/capacity/overages') return json([])
    if (p === '/api/encode-profiles') return json([])
    if (p === `/api/sites/${SITE}/services`) {
      return json([
        {
          networkId: program.networkId,
          serviceId: program.serviceId,
          name: 'NHK総合',
          channelType: 'GR',
          channel: '27',
          remoteControlKeyId: 1,
          hasLogoData: false,
          hasPrograms: true,
        },
      ])
    }
    if (p === `/api/sites/${SITE}/programs`) return json([program])
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
    return json([])
  })
}

/**
 * readReserveWidth は予約列の実レイアウト幅（px）を返す。
 *
 * 幅 0 の要素でも `getBoundingClientRect().width` は数値（0）を返すので、
 * boundingBox が null になりうる Playwright API より扱いが安定する。
 */
async function readReserveWidth(locator) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate((el) => el.getBoundingClientRect().width)
}

// 開＝約 80px（w-20）、畳＝約 0px。transition（150ms）の途中を拾わないよう、
// 各測定の前に十分待つ（下の waitForTimeout(250)）。
const isOpen = (w) => typeof w === 'number' && w > 40
const isCollapsed = (w) => typeof w === 'number' && w < 1

// ⓪ 配っている bundle が自分の dist かを先に確かめる（sse-refresh.mjs / badge-links.mjs
// と同じ理由。他 worktree の preview が同じポートに居座っていると無関係な古い
// ビルドを測ったまま判定が進む）。
{
  const rootHtml = await fetch(BASE + '/').then((r) => r.text())
  const served = /assets\/(index-[^"]+\.js)/.exec(rootHtml)?.[1]
  let local
  try {
    local = readdirSync(path.join(process.cwd(), 'dist', 'assets')).find((f) =>
      /^index-.*\.js$/.test(f),
    )
  } catch {
    local = undefined
  }
  if (served === undefined || served !== local) {
    log(`NG  ⓪ 配っている bundle（${served ?? '不明'}）が dist/assets/（${local ?? '不明'}）と違う`)
    log('    別プロセス・古いビルドを測っている。以降の判定に意味が無いので打ち切る')
    process.exit(1)
  }
  log(`OK  ⓪ 配っている bundle は自分の dist（${served}）`)
}

const browser = await chromium.launch()

// --- ① 細ポインタ（既定の Chromium コンテキスト） -----------------------------
log('\n=== ① 細ポインタ（hover:hover かつ pointer:fine） ===')
{
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })

  const row = page.locator(`li[data-program-id="${program.programId}"]`)
  await row.waitFor({ timeout: 15000 })
  const reserve = row.getByTestId('program-row-reserve')
  const toggle = row.locator('button[aria-expanded]').first()

  const before = await readReserveWidth(reserve)
  log(`  ホバー / フォーカス前の列幅: ${before}px`)
  if (!isCollapsed(before)) {
    ng.push(`①-a: ホバーもフォーカスもしていない行で予約列が開いている（幅=${before}px）`)
  }

  // キーボード（:focus-visible）を、まだこのページで一度もポインタ操作を
  // していない時点で確認する。**この順序が要る** --- Chromium の
  // :focus-visible ヒューリスティックは「直近の入力手段」を見るため、
  // 先にマウスで hover / click した後だとプログラムからの `.focus()` が
  // :focus-visible を伴わなくなる（design.mjs ⑥ の同じ注記と同じ理由）。
  // 開閉を `group-focus-within`（ANY フォーカス）ではなく
  // `group-has-[:focus-visible]` にしているのは、行トグルをマウスで
  // クリック / タップした直後に残る「見た目の無いフォーカス」で
  // 折りたたみ行がホバー無しでも開いたままになる回帰を防ぐため
  // （最初の実装で実際に踏んだ。②-c / ③-e 参照）。
  await toggle.focus()
  await page.waitForTimeout(250)
  const focusedFirst = await readReserveWidth(reserve)
  log(`  （ポインタ操作前）行トグルへフォーカス中の列幅: ${focusedFirst}px`)
  if (!isOpen(focusedFirst)) {
    ng.push(`①-f: 行トグルへフォーカスしても予約列が開かない（幅=${focusedFirst}px）`)
  }
  await toggle.evaluate((el) => el.blur())
  await page.waitForTimeout(250)
  const blurredFirst = await readReserveWidth(reserve)
  log(`  フォーカスを外した後の列幅: ${blurredFirst}px`)
  if (!isCollapsed(blurredFirst)) {
    ng.push(`①-g: フォーカスを外しても予約列が開いたまま（幅=${blurredFirst}px）`)
  }

  // 縦方向の CLS 判定用に、ホバー前の行の bounding box を控える。横方向
  // （列幅・タイトルの truncate 位置）は開閉で動くのが本設計の仕様なので測らない。
  const rowBoxBefore = await row.boundingBox()

  await toggle.hover()
  await page.waitForTimeout(250)
  const hovered = await readReserveWidth(reserve)
  log(`  ホバー中の列幅: ${hovered}px`)
  if (!isOpen(hovered)) {
    ng.push(`①-b: 行をホバーしても予約列が開かない（幅=${hovered}px）`)
  }

  const rowBoxHovered = await row.boundingBox()
  if (rowBoxBefore && rowBoxHovered) {
    const heightDelta = Math.abs(rowBoxHovered.height - rowBoxBefore.height)
    log(`  行の高さ差: ${heightDelta}px（ホバー前後）`)
    // **縦の CLS だけ弾く。** 予約ボタン（44px）は行（56px 以上）より低いので
    // 列を開いても行の高さは変わらないはず --- 変わると番組リストの仮想化
    // （program-list.tsx の measureElement）の再計測を誘発する。
    if (heightDelta > 0.5) ng.push(`①-c: ホバーで行の高さが ${heightDelta}px 変わった（縦 CLS）`)
  } else {
    ng.push('①-c: 行の bounding box が取れず縦 CLS を判定できない')
  }

  // ワンタップ予約が「開いているとき」に変わらず機能することも確認する
  // （罠: 幅を変えただけのつもりが pointer-events まで消していないか）。
  let putCalled = false
  await page.route(`**/api/sites/${SITE}/programs/${program.programId}/intent`, async (route) => {
    putCalled = true
    await route.fulfill({ status: 204 })
  })
  await reserve.getByRole('button').click()
  await page.waitForTimeout(250)
  log(`  ホバー中に予約ボタンを押すと PUT intent が呼ばれたか: ${putCalled}`)
  if (!putCalled) ng.push('①-d: ホバー中に予約ボタンを押しても PUT intent が呼ばれない')
  // 予約済みへ切り替わったので、以降のこのコンテキストでの検証は前提が変わる ---
  // ここで context を畳んで、後続の検証は新しい行（新しい context）で行う。

  // マウスを離す（クリックした際にマウスがボタン上に残っているため、hover が
  // 残っているだけで開いているのではないことを切り分けるにはページの無関係な
  // 位置へ動かす必要がある）。予約ボタンのクリック自体は :focus-visible を
  // 伴わない（マウスクリックで得るフォーカスは Chromium の既定でリングが
  // 付かない）ため、フォーカス経由で開いたままになる心配もない。
  await page.mouse.move(0, 0)
  await page.waitForTimeout(250)
  const afterHover = await readReserveWidth(reserve)
  log(`  マウスを離した後の列幅: ${afterHover}px`)
  if (!isCollapsed(afterHover)) {
    ng.push(`①-e: ホバーを外しても予約列が開いたまま（幅=${afterHover}px）`)
  }

  await context.close()
}

// --- ② 細ポインタ: 展開パネルを操作している間も予約列は開いたまま -----------
//
// 展開パネル（encodeProfiles / keepOriginal の欄）は `.group` の外側の兄弟
// なので、そこにマウス / フォーカスが移ると `group-hover` / `group-has-[:focus-visible]`
// はどちらも発火しない。`peer-aria-expanded:w-20` を pointer 種別で縛らずに
// 効かせているのはこのため --- 展開中は行ヘッダへのホバー / フォーカスの
// 有無に関係なく開いたままになる（「予約を押した時点で反映されます」という
// 展開パネルの案内と矛盾しないように）。
log('\n=== ② 細ポインタ: 展開パネル操作中も予約列が開いたまま ===')
{
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })

  const row = page.locator(`li[data-program-id="${program.programId}"]`)
  await row.waitFor({ timeout: 15000 })
  const reserve = row.getByTestId('program-row-reserve')
  const toggle = row.locator('button[aria-expanded]').first()

  await toggle.click()
  await page.waitForTimeout(250)
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') {
    ng.push('②-a: クリックしても行が展開しない')
  }

  // 展開パネル内（`.group` の外）へマウスを移す。行ヘッダからは離れる。
  const detail = page.locator(`#program-row-detail-${program.programId}`)
  await detail.waitFor({ timeout: 5000 }).catch(() => {})
  const detailBox = await detail.boundingBox()
  if (detailBox) {
    await page.mouse.move(detailBox.x + 10, detailBox.y + Math.min(10, detailBox.height - 1))
  }
  await page.mouse.move(0, 0) // フォーカスは残るがマウスは完全に離す
  await page.waitForTimeout(250)

  const whileExpanded = await readReserveWidth(reserve)
  log(`  展開中（行ヘッダからマウスもフォーカスも離れた後）の列幅: ${whileExpanded}px`)
  if (!isOpen(whileExpanded)) {
    ng.push(
      `②-b: 展開パネル操作中（行ヘッダの hover/focus-within が外れている）に予約列が` +
        `開いていない（幅=${whileExpanded}px）`,
    )
  }

  await toggle.click()
  // `.click()` は要素の座標へ実際にマウスを動かしてからクリックするので、
  // 何もしなければマウスは行トグルの上に残ったまま --- それ自体が
  // `pointer-fine:group-hover:w-20` を正当に成立させてしまい、「折りたたみ
  // 直後は畳む」を見たいのに「マウスがまだ乗っている」を見てしまう
  // （最初の実装でこの混同に気付かず誤検知した）。ページの無関係な位置へ
  // 動かして hover を切り離してから判定する。
  await page.mouse.move(0, 0)
  await page.waitForTimeout(250)
  const afterCollapse = await readReserveWidth(reserve)
  log(`  折りたたみ直した後（マウスも離れた）の列幅: ${afterCollapse}px`)
  if (!isCollapsed(afterCollapse)) {
    ng.push(`②-c: 折りたたみ直しても予約列が開いたまま（幅=${afterCollapse}px）`)
  }

  await context.close()
}

// --- ③ タッチ / 粗いポインタ（hover:none かつ pointer:coarse） ---------------
log('\n=== ③ タッチ / 粗いポインタ ===')
{
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  await page.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })

  const row = page.locator(`li[data-program-id="${program.programId}"]`)
  await row.waitFor({ timeout: 15000 })
  const reserve = row.getByTestId('program-row-reserve')
  const toggle = row.locator('button[aria-expanded]').first()

  const collapsed = await readReserveWidth(reserve)
  log(`  折りたたみ行の列幅: ${collapsed}px`)
  if (!isCollapsed(collapsed)) {
    ng.push(`③-a: タッチで折りたたみ行の予約列が開いている（幅=${collapsed}px）`)
  }

  // WCAG 2.4.7 / 2.4.11: 粗いポインタでも外付けキーボードで行トグルへフォーカス
  // すれば開くようになる（`group-has-[:focus-visible]:w-20` を pointer 種別で
  // 縛っていないため）。折りたたんだままで確認する（展開のせいで開いているの
  // ではないことを切り分けるため）。
  await toggle.focus()
  await page.waitForTimeout(250)
  const focusedCoarse = await readReserveWidth(reserve)
  log(`  折りたたみ行のまま行トグルへフォーカス中の列幅: ${focusedCoarse}px`)
  if (!isOpen(focusedCoarse)) {
    ng.push(
      `③-b: 粗いポインタでも行トグルへフォーカスすれば開くはずが開かない` +
        `（幅=${focusedCoarse}px。タブレット + 外付けキーボードで操作不能になる）`,
    )
  }
  await toggle.evaluate((el) => el.blur())
  await page.waitForTimeout(250)
  const blurredCoarse = await readReserveWidth(reserve)
  if (!isCollapsed(blurredCoarse)) {
    ng.push(`③-b': フォーカスを外しても開いたまま（幅=${blurredCoarse}px）`)
  }

  await toggle.tap()
  await page.waitForTimeout(250)
  const expandedAttr = await toggle.getAttribute('aria-expanded')
  if (expandedAttr !== 'true') {
    ng.push(`③-c: タップしても行が展開しない（aria-expanded=${expandedAttr}）`)
  }
  const expanded = await readReserveWidth(reserve)
  log(`  展開後（aria-expanded=true）の列幅: ${expanded}px`)
  if (!isOpen(expanded)) {
    ng.push(`③-d: タッチで展開した行でも予約列が開かない（幅=${expanded}px）`)
  }

  await toggle.tap()
  await page.waitForTimeout(250)
  const collapsedAgain = await readReserveWidth(reserve)
  log(`  再度折りたたんだ後の列幅: ${collapsedAgain}px`)
  if (!isCollapsed(collapsedAgain)) {
    ng.push(`③-e: 折りたたみ直しても予約列が開いたまま（幅=${collapsedAgain}px）`)
  }

  await context.close()
}

// --- ④ タッチ / 粗いポインタ: 折りたたみ行の予約列位置への「見えないタップ」 --
//
// **これがレビューで実際に見逃していた欠陥そのもの。** 別 PR の `opacity-0` 版では
// 折りたたみ行の予約列（80×56px、画面の実座標に存在する）が見た目には
// 無いのに実際にはタップ可能なままで、`page.touchscreen.tap()` で実座標を
// 直接叩くと PUT intent が飛んで予約が成立していた --- ロケータ経由の
// `.click()` / `.tap()` はアクショナビリティ判定（要素が実際に操作可能かの
// 事前検査）を経由するため、この種の「不可視だが hit-testable」な欠陥を
// 素通りさせてしまう。幅 0 + overflow-hidden 版はその標的自体を消すので、
// ここでは意図的にロケータを使わず、行の右端（開いたら予約列が来る位置）の
// 実座標へ `page.touchscreen.tap(x, y)` を投げ、あわせて `elementFromPoint` で
// その座標の実際のヒットテスト結果を確認する。
log('\n=== ④ タッチ / 粗いポインタ: 折りたたみ行の予約列位置への見えないタップ ===')
{
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page)
  let putCalled = false
  await page.route(`**/api/sites/${SITE}/programs/${program.programId}/intent`, async (route) => {
    putCalled = true
    await route.fulfill({ status: 204 })
  })
  await page.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })

  const row = page.locator(`li[data-program-id="${program.programId}"]`)
  await row.waitFor({ timeout: 15000 })
  const reserve = row.getByTestId('program-row-reserve')

  const rowBox = await row.boundingBox()
  if (rowBox === null) {
    ng.push('④: 行の bounding box が取れない')
  } else {
    // 折りたたみ行では予約列は幅 0 なので、行の右端の少し内側（開いたら予約
    // ボタンが占める位置）を狙う。
    const x = rowBox.x + rowBox.width - 2
    const y = rowBox.y + rowBox.height / 2
    log(`  折りたたみ行の予約列幅=${await readReserveWidth(reserve)}px、右端座標=(${x}, ${y}) を素タップ`)

    // ヒットテストの標的が予約ボタンでない（畳んでいるので行トグル側）ことを確認。
    const hitIsReserve = await page.evaluate(
      ({ x, y }) => {
        const el = document.elementFromPoint(x, y)
        return el ? el.closest('[data-testid="program-row-reserve"]') !== null : false
      },
      { x, y },
    )
    log(`  その座標のヒットテスト標的が予約列の内側か: ${hitIsReserve}`)
    if (hitIsReserve) {
      ng.push('④-a: 折りたたみ行の右端が予約列（幅 0 のはず）にヒットする（phantom tap target）')
    }

    await page.touchscreen.tap(x, y)
    await page.waitForTimeout(300)
    log(`  見えない予約列位置を素タップ → PUT intent が飛んだか: ${putCalled}`)
    if (putCalled) {
      ng.push('④-b: 折りたたみ行の予約列位置への生タップで PUT intent が飛んだ（phantom tap target）')
    }
    const toastCandidates = await page.getByText(/予約しました/).count()
    log(`  画面の「予約しました」トースト候補: ${toastCandidates}`)
    if (toastCandidates > 0) {
      ng.push('④-c: 見えない予約列位置への生タップ後に「予約しました」トーストが出た')
    }
  }

  await context.close()
}

await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
