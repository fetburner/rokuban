// 番組リストの受け入れ判定。jsdom では測れないものだけをここで見る（e2e/README.md）。
// 合格なら exit 0、1 つでも NG なら exit 1。
import { chromium } from 'playwright'

const URL = process.env.E2E_URL ?? 'http://localhost:40773'
/** 日付ストリップの何番目を押すか（0 = 今日）。 */
const DAY_INDEX = Number(process.env.E2E_DAY_INDEX ?? 6)
/** 「前を読み込む」を何回押して確かめるか。 */
const REWINDS = Number(process.env.E2E_REWINDS ?? 3)
/** 押下直後のフレーム跳ねの許容値。これを超えると視覚的な飛びとして認識される。 */
const maxFrameJumpPx = 60
/** 遡行の前後で、見ている行がずれてよい量。1 行ぶん未満に収める。 */
const maxDriftPx = 40

const ng = []
const log = (...a) => console.log(...a)

const browser = await chromium.launch()
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

/**
 * ユーザーが実際に見ている先頭行。sticky な PageHeader と「前を読み込む」ボタンの
 * 下端より下に最初に現れる行を指す（`bottom > 0` で採ると sticky の裏に隠れた行を
 * 掴んでしまい、人が見ているものと食い違う）。
 */
const visibleTopRow = () =>
  page.evaluate(() => {
    const header = document.querySelector('header')?.getBoundingClientRect()
    let cutPx = header ? header.bottom : 0
    // 遡行ボタンが画面上部の帯を占めているときだけ、その下端まで下げる。
    // 通常フローに置かれて画面外（上）へ流れた場合は数えない ——
    // 高さを無条件に足すと、実際には見えているのに隠れている扱いの行が出る。
    const button = [...document.querySelectorAll('button')].find((b) =>
      /前を読み込む|を読み込む/.test(b.textContent || ''),
    )
    if (button) {
      const r = button.getBoundingClientRect()
      if (r.bottom > cutPx && r.top < cutPx + 8) cutPx = r.bottom
    }
    for (const el of document.querySelectorAll('li[data-program-id]')) {
      const rect = el.getBoundingClientRect()
      if (rect.top >= cutPx - 4) {
        return {
          id: el.getAttribute('data-program-id'),
          top: Math.round(rect.top),
          text: el.innerText.replace(/\s+/g, ' ').slice(0, 32),
        }
      }
    }
    return null
  })

const loadPreviousButton = () => page.getByRole('button', { name: /前を読み込む|を読み込む/ })

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

// --- ② 遡行しても、見ている行が保たれる / フレーム跳ねが無い ---
log(`\n=== ② 遡行（${REWINDS} 回） ===`)
await page.mouse.wheel(0, 1200)
await page.waitForTimeout(800)

for (let i = 1; i <= REWINDS; i++) {
  // 実際の操作に合わせて、押す前にリストの上端まで戻る。遡行ボタンは
  // 「読み込み済みの先頭まで戻ってきて、その前日も見たくなった」ときにだけ
  // 使うものなので、画面外のボタンを押しに行く経路は判定しない。
  await page.evaluate(() => window.scrollTo(0, 0))
  await page.waitForTimeout(600)
  const before = await visibleTopRow()
  if (before === null) {
    // sticky の裏より下に見えている行が 1 つも無い ---
    // 多くは E2E_DAY_INDEX が指す日に番組データが無く、リストが空であること。
    // 実装の不具合ではなく判定の前提が満たされていないので、素の TypeError
    // （`before.id` 参照）で落とすのではなく判定不能として明示的に終える。
    log(`  ${i} 回目: 判定不能（見えている行が無い。E2E_DAY_INDEX=${DAY_INDEX} の日に番組データが無いかもしれません）`)
    ng.push(`② ${i} 回目は判定不能（見えている行が無い。E2E_DAY_INDEX の日に番組データが無いかもしれません）`)
    break
  }
  if ((await loadPreviousButton().count()) === 0) {
    log(`  ${i} 回目: ボタンが無い（下限に到達）`)
    break
  }
  // 押した直後をフレーム単位で記録する。差し込んだ DOM が補正より先に描画されると
  // ここに 1 フレームだけ大きな跳ねが出る。
  await page.evaluate((id) => {
    window.__frames = []
    const tick = () => {
      const el = document.querySelector(`li[data-program-id="${id}"]`)
      window.__frames.push(el ? Math.round(el.getBoundingClientRect().top) : null)
      if (window.__frames.length < 60) requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  }, before.id)

  await loadPreviousButton().first().click()
  await page.waitForTimeout(2500)

  const frames = (await page.evaluate(() => window.__frames)).filter((t) => t !== null)
  const settled = frames.at(-1)
  const jump = frames.length ? Math.max(...frames.map((t) => Math.abs(t - settled))) : 0
  const after = await visibleTopRow()
  const same = before && after && before.id === after.id
  const drift = before && after ? after.top - before.top : null

  log(`  ${i} 回目: "${before.text}" (top=${before.top})`)
  log(`          → "${after?.text}" (top=${after?.top})`)
  log(`          同じ行=${same ? 'YES' : 'NO'} ズレ=${drift}px フレーム跳ね=${jump}px`)
  if (!same) ng.push(`② ${i} 回目で見ている行が変わった（${before?.text} → ${after?.text}）`)
  else if (Math.abs(drift) > maxDriftPx) ng.push(`② ${i} 回目で ${drift}px ずれた`)
  if (jump > maxFrameJumpPx) ng.push(`② ${i} 回目の押下直後に ${jump}px のフレーム跳ね`)
}

// --- ③ 遡行して別の日を見た状態から、元の日付を押し直せる ---
log(`\n=== ③ 同じ日付の押し直し ===`)
await page.mouse.wheel(0, -6000)
await page.waitForTimeout(1200)
const strayed = await currentDay()
log(`  遡行 + 上へスクロール後のハイライト: ${strayed}`)
await dayCells.nth(DAY_INDEX).click()
await page.waitForTimeout(2500)
const restored = await currentDay()
log(`  「${target}」を押し直した後            : ${restored}`)
if (strayed === target) {
  log(`  （別の日へ移れていないため、この判定は成立していない）`)
  ng.push('③ 遡行しても別の日に移らず、押し直しの判定が成立しなかった')
} else if (restored !== target) {
  ng.push(`③ 「${target}」を押したのにハイライトが「${restored}」のまま`)
}

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
