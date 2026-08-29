// 番組リストの受け入れ判定。jsdom では測れないものだけをここで見る（e2e/README.md）。
// 合格なら exit 0、1 つでも NG なら exit 1。
import { finish, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL = process.env.E2E_URL ?? 'http://localhost:40773'
/** 日付ストリップの何番目を押すか（0 = 今日）。 */
const DAY_INDEX = Number(process.env.E2E_DAY_INDEX ?? 6)
/** ②で日をまたがせるために、順方向スクロールを試みる最大回数。 */
const maxForwardScrollSteps = 8

const ng = []

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL, ng)

const browser = await launchBrowser()
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } })
// 番組表は M8-3 でホーム（`/`）に `/` を譲り `/programs` へ移設した。
await page.goto(URL + '/programs', { waitUntil: 'networkidle' })
await page.waitForSelector('li[data-program-id]', { timeout: 15000 })

const dayCells = page.locator('[role="group"][aria-label="日付"] button')
const dayLabels = await dayCells.evaluateAll((els) => els.map((e) => e.getAttribute('aria-label')))
const currentDay = () =>
  page
    .locator('[role="group"][aria-label="日付"] button[aria-current="date"]')
    .first()
    .getAttribute('aria-label')
    .catch(() => null)

// ②は DAY_INDEX の 1 つ後ろの日へ「順方向スクロールでまたぐ」ことを前提にする。
// DAY_INDEX が選択肢の最後（後ろに日が無い）だと構造的に成立しないので、
// 判定不能のまま NG にせず、ここで理由を出して落とす。
if (DAY_INDEX >= dayLabels.length - 1) {
  console.error(
    `E2E_DAY_INDEX=${DAY_INDEX} は選択肢の最後（全 ${dayLabels.length} 日）で、② が日をまたげず成立しない。E2E_DAY_INDEX を ${dayLabels.length - 2} 以下にすること。`,
  )
  await browser.close()
  process.exit(1)
}

// --- ① 未キャッシュの日を押したら、その日へ跳ぶ ---
// スケルトンに挿し替わって文書高さが潰れない（issue #299）/ ハイライトが移る /
// 前の日の scrollY を引き継がず選んだ日の先頭に着地する、の 3 点を見る。
log(`\n=== ① 日付ストリップ（未キャッシュ日へのジャンプ） ===`)
const target = dayLabels[DAY_INDEX]
// 下へスクロールしてから跳ぶ（issue #299 の再現手順。scrollY 引き継ぎの判定に要る）。
// スクロールで進行方向の自動読み込みが走り、リストが伸びてから測る。
await page.mouse.wheel(0, 1200)
await page.waitForTimeout(1200)
const heightBefore = await page.evaluate(() => Math.round(document.documentElement.scrollHeight))
// 押下直後をフレーム単位で観測する。placeholderData が無いとクエリ作り直しで
// `isPending` が即 true になり `ListSkeleton`（`animate-pulse` + `scanlines` の
// 走査線 6 本）に挿し替わって、文書高さがビューポート（800px）まで潰れる。
// この 1 フレームは localhost でも見える（issue #299）ので rAF で捕まえる。
await page.evaluate(() => {
  window.__jump = []
  const tick = () => {
    window.__jump.push({
      h: Math.round(document.documentElement.scrollHeight),
      skel: !!document.querySelector('.animate-pulse.scanlines'),
      day: document
        .querySelector('[role="group"][aria-label="日付"] button[aria-current="date"]')
        ?.getAttribute('aria-label'),
    })
    if (window.__jump.length < 90) requestAnimationFrame(tick)
  }
  requestAnimationFrame(tick)
})
await dayCells.nth(DAY_INDEX).click()
await page.waitForTimeout(2500)

const jump = await page.evaluate(() => window.__jump)
const sawSkeleton = jump.some((f) => f.skel)
const minHeight = jump.length ? Math.min(...jump.map((f) => f.h)) : 0
const firstTargetFrame = jump.findIndex((f) => f.day === target)
const highlightReverted =
  firstTargetFrame >= 0 && jump.slice(firstTargetFrame + 1).some((f) => f.day !== target)
const highlighted = await currentDay()
const landedScrollY = await page.evaluate(() => Math.round(window.scrollY))
log(`  押した日付   : ${target}`)
log(`  ハイライト   : ${highlighted}`)
log(`  跳ぶ前の高さ : ${heightBefore}px / 跳躍中の最小高さ: ${minHeight}px`)
log(`  スケルトン   : ${sawSkeleton ? '出た' : '出ない'} / 着地 scrollY: ${landedScrollY}px`)
if (highlighted !== target) ng.push(`① ハイライトが「${highlighted}」（期待「${target}」）`)
if (highlightReverted) ng.push('① ハイライトが選んだ日から前の日へ一時的に戻った')
if (sawSkeleton) ng.push('① 未キャッシュ日へのジャンプで ListSkeleton に挿し替わった')
// 前のリストを残していれば高さは潰れない。ビューポート（+余白）まで落ちたら潰れた。
if (minHeight <= 850) ng.push(`① ジャンプ中に文書高さが ${minHeight}px まで潰れた（前=${heightBefore}px）`)
// 選んだ日の先頭行に着地する（前の日の scrollY=1200 付近を引き継がない）。
if (landedScrollY > 200) ng.push(`① 着地 scrollY=${landedScrollY}px（前の日のスクロールを引き継いだ）`)

// --- ② 順方向にスクロールして日をまたいでから同じ日を再タップする ---
log(`\n=== ② 同じ日付の押し直し ===`)
let strayed = target
for (let i = 0; i < maxForwardScrollSteps; i++) {
  await page.mouse.wheel(0, 4000)
  await page.waitForTimeout(1000)
  strayed = await currentDay()
  if (strayed !== target) break
}
const scrollYBeforeRetap = await page.evaluate(() => Math.round(window.scrollY))
log(`  順方向にスクロールした後のハイライト: ${strayed} (scrollY=${scrollYBeforeRetap}px)`)
await dayCells.nth(DAY_INDEX).click()
await page.waitForTimeout(2500)
const restored = await currentDay()
const scrollYAfterRetap = await page.evaluate(() => Math.round(window.scrollY))
log(`  「${target}」を押し直した後            : ${restored} (scrollY=${scrollYAfterRetap}px)`)
if (strayed === target) {
  log(`  （日をまたげていないため、この判定は成立していない）`)
  ng.push('② 順方向にスクロールしても日をまたがず、押し直しの判定が成立しなかった')
} else {
  // 主たる oracle は scrollY --- ハイライト（visibleDay）は
  // `onVisibleDayChange` の再発火という偶発的な経路でも動きうるため
  // （`programs.tsx` の `nowMs` が毎レンダー変わり、依存に `now` を持つ
  // effect が再タップ後の再レンダーで必ず再発火する）、それだけでは
  // `scrollToDayOffset` が実際にスクロールしたことの証明にならない。
  if (scrollYAfterRetap > 200) {
    ng.push(`② 押し直し後も scrollY=${scrollYAfterRetap}px のまま（先頭へ戻っていない）`)
  }
  if (restored !== target) ng.push(`② 「${target}」を押したのにハイライトが「${restored}」のまま`)
}

await finish(ng, browser)
