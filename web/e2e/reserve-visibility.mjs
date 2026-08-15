// 番組リストの予約 / 取消ボタンの可視性判定（issue #310）。
//
// **CSS の `:hover` / `:focus-visible` / `pointer:` メディア特性で駆動する可視性は
// jsdom では測れない**（web/e2e/README.md「jsdom が測れないもの」）。jsdom は
// `getComputedStyle().visibility` にクラス由来の値を反映しないので、`pnpm test` は
// 「常時 visible のまま」というクラス名の変異を検出できない。
//
// **可視性の実装手段は `visibility`（`invisible` / `group-*:visible`）であって
// `opacity` ではない。** 最初の実装は `opacity-0` を使っていたが、レビューで
// 「折りたたみ行の右端 80×56px が、見た目には無いのに実際にはタップ可能な
// ままで、生の座標へのタップで予約が成立してしまう」欠陥が見つかった
// （opacity は見た目を消すだけでヒットテストも tab 順序も残す）。`visibility:
// hidden` はヒットテストからも tab 順序からも要素を外すので、この判定 ④ は
// 「折りたたみ行の予約列を生座標でタップしても PUT が飛ばない」ことも見る ---
// これが実際に見逃していた欠陥そのもの。
//
// 見るのは 4 状態（すべて実描画の `visibility` で判定する。Playwright の
// `.isVisible()` は opacity は見ないが、visibility には反応する。ここでは
// 判定理由を明示するため getComputedStyle を直接読む）:
//   ① 細ポインタ（既定の Chromium コンテキスト = hover:hover + pointer:fine）で
//      ホバーもフォーカスもしていない行 --- 見えない。ホバー / :focus-visible で
//      見える（両方向）。あわせて CLS（行・列の bounding box 不変）も測る
//   ② 細ポインタで行を展開すると、その後マウス / フォーカスが行ヘッダから
//      離れても予約ボタンは見えたまま（展開パネルは `.group` の外の兄弟なので
//      `group-hover` / `group-has-[:focus-visible]` はそこから発火しない ---
//      `peer-aria-expanded` を pointer 種別で縛らないことで担保する）
//   ③ タッチ / 粗いポインタ（hasTouch + isMobile のコンテキスト = hover:none +
//      pointer:coarse）で、折りたたみ行は見えない・展開した行
//      （aria-expanded=true）だけ見える。加えて、粗いポインタでもキーボード
//      （行トグルへの :focus-visible）だけで見えるようになること（タブレット +
//      外付けキーボードの想定。WCAG 2.4.7 / 2.4.11）
//   ④ タッチ / 粗いポインタで、折りたたみ行の予約列を実座標へ生のタップ
//      （`page.touchscreen.tap`。ロケータ経由のアクショナビリティを迂回する
//      ため、可視性チェックをすり抜けた「見えないタップ標的」をそのまま検出
//      できる）で押しても PUT intent が飛ばない・トーストも出ない
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

/** readVisibility は要素の実描画 `visibility` を文字列で返す（'visible' | 'hidden' 等）。 */
async function readVisibility(locator) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate((el) => getComputedStyle(el).visibility)
}

const isVisible = (v) => v === 'visible'
const isHidden = (v) => v === 'hidden'

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

  const before = await readVisibility(reserve)
  log(`  ホバー / フォーカス前の visibility: ${before}`)
  if (!isHidden(before)) {
    ng.push(`①-a: ホバーもフォーカスもしていない行で予約ボタンが見えている（visibility=${before}）`)
  }

  // キーボード（:focus-visible）を、まだこのページで一度もポインタ操作を
  // していない時点で確認する。**この順序が要る** --- Chromium の
  // :focus-visible ヒューリスティックは「直近の入力手段」を見るため、
  // 先にマウスで hover / click した後だとプログラムからの `.focus()` が
  // :focus-visible を伴わなくなる（design.mjs ⑥ の同じ注記と同じ理由）。
  // 可視性を `group-focus-within`（ANY フォーカス）ではなく
  // `group-has-[:focus-visible]` にしているのは、行トグルをマウスで
  // クリック / タップした直後に残る「見た目の無いフォーカス」で
  // 折りたたみ行がホバー無しでも見えたままになる回帰を防ぐため
  // （最初の実装で実際に踏んだ。②-c / ③-e 参照）。
  await toggle.focus()
  await page.waitForTimeout(250)
  const focusedFirst = await readVisibility(reserve)
  log(`  （ポインタ操作前）行トグルへフォーカス中の visibility: ${focusedFirst}`)
  if (!isVisible(focusedFirst)) {
    ng.push(`①-f: 行トグルへフォーカスしても予約ボタンが見えない（visibility=${focusedFirst}）`)
  }
  await toggle.evaluate((el) => el.blur())
  await page.waitForTimeout(250)
  const blurredFirst = await readVisibility(reserve)
  log(`  フォーカスを外した後の visibility: ${blurredFirst}`)
  if (!isHidden(blurredFirst)) {
    ng.push(`①-g: フォーカスを外しても予約ボタンが見えたまま（visibility=${blurredFirst}）`)
  }

  // CLS 判定用に、ホバー前の行・予約列の bounding box を控える。
  const rowBoxBefore = await row.boundingBox()
  const reserveBoxBefore = await reserve.boundingBox()

  await toggle.hover()
  await page.waitForTimeout(250)
  const hovered = await readVisibility(reserve)
  log(`  ホバー中の visibility: ${hovered}`)
  if (!isVisible(hovered)) {
    ng.push(`①-b: 行をホバーしても予約ボタンが見えない（visibility=${hovered}）`)
  }

  const rowBoxHovered = await row.boundingBox()
  const reserveBoxHovered = await reserve.boundingBox()
  if (rowBoxBefore && rowBoxHovered) {
    const heightDelta = Math.abs(rowBoxHovered.height - rowBoxBefore.height)
    const widthDelta = Math.abs(rowBoxHovered.width - rowBoxBefore.width)
    log(`  行の高さ差: ${heightDelta}px / 幅差: ${widthDelta}px（ホバー前後）`)
    if (heightDelta > 0.5) ng.push(`①-c: ホバーで行の高さが ${heightDelta}px 変わった（CLS）`)
    if (widthDelta > 0.5) ng.push(`①-c: ホバーで行の幅が ${widthDelta}px 変わった（CLS）`)
  } else {
    ng.push('①-c: 行の bounding box が取れず CLS を判定できない')
  }
  if (reserveBoxBefore && reserveBoxHovered) {
    const colWidthDelta = Math.abs(reserveBoxHovered.width - reserveBoxBefore.width)
    log(`  予約列の幅差: ${colWidthDelta}px（ホバー前後）`)
    if (colWidthDelta > 0.5) {
      ng.push(`①-c: ホバーで予約列（w-20）の幅が ${colWidthDelta}px 変わった（列幅が固定されていない）`)
    }
  } else {
    ng.push('①-c: 予約列の bounding box が取れず幅の固定を判定できない')
  }

  // ワンタップ予約が「見えているとき」に変わらず機能することも確認する
  // （罠: 可視性を変えただけのつもりが pointer-events まで消していないか）。
  let putCalled = false
  await page.route(`**/api/sites/${SITE}/programs/${program.programId}/intent`, async (route) => {
    putCalled = true
    await route.fulfill({ status: 204 })
  })
  await reserve.getByRole('button').click()
  await page.waitForTimeout(250)
  log(`  ホバー中に予約ボタンを押すと PUT intent が呼ばれたか: ${putCalled}`)
  if (!putCalled) ng.push('①-d: ホバー中に予約ボタンを押しても PUT intent が呼ばれない')
  // 予約済みへ切り替わったので、以降のこのコンテキストでの検証は「取消」を見る
  // 前提が変わってしまう --- ここで context を畳んで、後続の検証は新しい行
  // （新しい context）で行う。

  // マウスを離す（クリックした際にマウスがボタン上に残っているため、hover が
  // 残っているだけで見えているのではないことを切り分けるにはページの無関係な
  // 位置へ動かす必要がある）。予約ボタンのクリック自体は :focus-visible を
  // 伴わない（マウスクリックで得るフォーカスは Chromium の既定でリングが
  // 付かない）ため、フォーカス経由で見えたままになる心配もない。
  await page.mouse.move(0, 0)
  await page.waitForTimeout(250)
  const afterHover = await readVisibility(reserve)
  log(`  マウスを離した後の visibility: ${afterHover}`)
  if (!isHidden(afterHover)) {
    ng.push(`①-e: ホバーを外しても予約ボタンが見えたまま（visibility=${afterHover}）`)
  }

  await context.close()
}

// --- ② 細ポインタ: 展開パネルを操作している間も予約ボタンは見えたまま -------
//
// 展開パネル（encodeProfiles / keepOriginal の欄）は `.group` の外側の兄弟
// なので、そこにマウス / フォーカスが移ると `group-hover` / `group-has-[:focus-visible]`
// はどちらも発火しない。`peer-aria-expanded:visible` を pointer 種別で縛らずに
// 効かせているのはこのため --- 展開中は行ヘッダへのホバー / フォーカスの
// 有無に関係なく見えたままになる（「予約を押した時点で反映されます」という
// 展開パネルの案内と矛盾しないように）。
log('\n=== ② 細ポインタ: 展開パネル操作中も予約ボタンが見える ===')
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

  const whileExpanded = await readVisibility(reserve)
  log(`  展開中（行ヘッダからマウスもフォーカスも離れた後）の visibility: ${whileExpanded}`)
  if (!isVisible(whileExpanded)) {
    ng.push(
      `②-b: 展開パネル操作中（行ヘッダの hover/focus-within が外れている）に予約ボタンが` +
        `見えない（visibility=${whileExpanded}）`,
    )
  }

  await toggle.click()
  // `.click()` は要素の座標へ実際にマウスを動かしてからクリックするので、
  // 何もしなければマウスは行トグルの上に残ったまま --- それ自体が
  // `pointer-fine:group-hover:visible` を正当に成立させてしまい、「折りたたみ
  // 直後は見えなくなる」を見たいのに「マウスがまだ乗っている」を見てしまう
  // （最初の実装でこの混同に気付かず誤検知した）。ページの無関係な位置へ
  // 動かして hover を切り離してから判定する。
  await page.mouse.move(0, 0)
  await page.waitForTimeout(250)
  const afterCollapse = await readVisibility(reserve)
  log(`  折りたたみ直した後（マウスも離れた）の visibility: ${afterCollapse}`)
  if (!isHidden(afterCollapse)) {
    ng.push(`②-c: 折りたたみ直しても予約ボタンが見えたまま（visibility=${afterCollapse}）`)
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

  const collapsed = await readVisibility(reserve)
  log(`  折りたたみ行の visibility: ${collapsed}`)
  if (!isHidden(collapsed)) {
    ng.push(`③-a: タッチで折りたたみ行の予約ボタンが見えている（visibility=${collapsed}）`)
  }

  // WCAG 2.4.7 / 2.4.11: 粗いポインタでも外付けキーボードで行トグルへフォーカス
  // すれば見えるようになる（`group-has-[:focus-visible]:visible` を pointer 種別で
  // 縛っていないため）。折りたたんだままで確認する（展開のせいで見えているの
  // ではないことを切り分けるため）。
  await toggle.focus()
  await page.waitForTimeout(250)
  const focusedCoarse = await readVisibility(reserve)
  log(`  折りたたみ行のまま行トグルへフォーカス中の visibility: ${focusedCoarse}`)
  if (!isVisible(focusedCoarse)) {
    ng.push(
      `③-b: 粗いポインタでも行トグルへフォーカスすれば見えるはずが見えない` +
        `（visibility=${focusedCoarse}。タブレット + 外付けキーボードで操作不能になる）`,
    )
  }
  await toggle.evaluate((el) => el.blur())
  await page.waitForTimeout(250)
  const blurredCoarse = await readVisibility(reserve)
  if (!isHidden(blurredCoarse)) {
    ng.push(`③-b': フォーカスを外しても見えたまま（visibility=${blurredCoarse}）`)
  }

  await toggle.tap()
  await page.waitForTimeout(250)
  const expandedAttr = await toggle.getAttribute('aria-expanded')
  if (expandedAttr !== 'true') {
    ng.push(`③-c: タップしても行が展開しない（aria-expanded=${expandedAttr}）`)
  }
  const expanded = await readVisibility(reserve)
  log(`  展開後（aria-expanded=true）の visibility: ${expanded}`)
  if (!isVisible(expanded)) {
    ng.push(`③-d: タッチで展開した行でも予約ボタンが見えない（visibility=${expanded}）`)
  }

  await toggle.tap()
  await page.waitForTimeout(250)
  const collapsedAgain = await readVisibility(reserve)
  log(`  再度折りたたんだ後の visibility: ${collapsedAgain}`)
  if (!isHidden(collapsedAgain)) {
    ng.push(`③-e: 折りたたみ直しても予約ボタンが見えたまま（visibility=${collapsedAgain}）`)
  }

  await context.close()
}

// --- ④ タッチ / 粗いポインタ: 折りたたみ行の予約列への「見えないタップ」 -----
//
// **これがレビューで実際に見逃していた欠陥そのもの。** `opacity-0` 版では
// 折りたたみ行の予約列（80×56px、画面の実座標に存在する）が見た目には
// 無いのに実際にはタップ可能なままで、`page.touchscreen.tap()` で実座標を
// 直接叩くと PUT intent が飛んで予約が成立していた --- ロケータ経由の
// `.click()` / `.tap()` はアクショナビリティ判定（要素が実際に操作可能かの
// 事前検査）を経由するため、この種の「不可視だが hit-testable」な欠陥を
// 素通りさせてしまう。ここでは意図的にロケータを使わず、控えておいた
// bounding box の実座標へ `page.touchscreen.tap(x, y)` を投げることで、
// ブラウザの実際のヒットテスト結果を確認する。
log('\n=== ④ タッチ / 粗いポインタ: 折りたたみ行への見えないタップ ===')
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
  const toggle = row.locator('button[aria-expanded]').first()

  const box = await reserve.boundingBox()
  if (box === null) {
    ng.push('④: 予約列の bounding box が取れない（折りたたみ行なのに DOM から消えている?）')
  } else {
    log(`  折りたたみ行の予約列 visibility=${await readVisibility(reserve)} box=${JSON.stringify(box)}`)
    const x = box.x + box.width / 2
    const y = box.y + box.height / 2
    await page.touchscreen.tap(x, y)
    await page.waitForTimeout(300)
    log(`  見えない予約列を素タップ → PUT intent が飛んだか: ${putCalled}`)
    if (putCalled) {
      ng.push('④-a: 折りたたみ行の見えない予約列への生タップで PUT intent が飛んだ（phantom tap target）')
    }
    const stillCollapsed = await toggle.getAttribute('aria-expanded')
    log(`  タップ後の aria-expanded: ${stillCollapsed}`)
    if (stillCollapsed === 'true') {
      ng.push('④-b: 見えない予約列へのタップが行の展開トグルとして扱われた（意図しない副作用）')
    }
    const toastCandidates = await page.getByText(/予約しました/).count()
    log(`  画面の「予約しました」トースト候補: ${toastCandidates}`)
    if (toastCandidates > 0) {
      ng.push('④-c: 見えない予約列への生タップ後に「予約しました」トーストが出た')
    }
  }

  await context.close()
}

await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
