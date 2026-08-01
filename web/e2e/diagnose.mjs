// 原因調査用。合否は出さず、遡行の前後で「添字がどう動き、画素がどうずれたか」を出す。
// checks.mjs が NG になったとき、症状が添字ずれなのか座標ずれなのかを切り分ける。
import { chromium } from 'playwright'

const URL = process.env.E2E_URL ?? 'http://localhost:40773'
const DAY_INDEX = Number(process.env.E2E_DAY_INDEX ?? 6)
const ROUNDS = Number(process.env.E2E_REWINDS ?? 3)

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } })
await page.goto(URL, { waitUntil: 'networkidle' })
await page.waitForSelector('li[data-program-id]')
await page.locator('[role="group"][aria-label="日付"] button').nth(DAY_INDEX).click()
await page.waitForTimeout(2500)
await page.mouse.wheel(0, 1200)
await page.waitForTimeout(800)

const snapshot = () =>
  page.evaluate(() => {
    const rows = [...document.querySelectorAll('li[data-program-id]')]
    const anchor = rows.find((r) => r.getBoundingClientRect().bottom > 0)
    return {
      scrollY: Math.round(window.scrollY),
      docHeight: Math.round(document.documentElement.scrollHeight),
      rendered: rows.length,
      range: `${rows[0]?.getAttribute('data-index')}..${rows.at(-1)?.getAttribute('data-index')}`,
      anchor: anchor && {
        id: anchor.getAttribute('data-program-id'),
        index: Number(anchor.getAttribute('data-index')),
        top: Math.round(anchor.getBoundingClientRect().top),
      },
    }
  })

const locate = (id) =>
  page.evaluate((id) => {
    const el = document.querySelector(`li[data-program-id="${id}"]`)
    if (!el) return null
    const rect = el.getBoundingClientRect()
    const rowsAbove = [...document.querySelectorAll('li[data-program-id]')].filter((o) => {
      const r = o.getBoundingClientRect()
      return r.top < rect.top && r.bottom > 0
    }).length
    return {
      index: Number(el.getAttribute('data-index')),
      top: Math.round(rect.top),
      height: Math.round(rect.height),
      hasDateBand: !!el.querySelector('h2'),
      rowsAbove,
    }
  }, id)

for (let i = 1; i <= ROUNDS; i++) {
  const before = await snapshot()
  const button = page.getByRole('button', { name: /前を読み込む|を読み込む/ })
  if ((await button.count()) === 0) break
  const beforeAnchor = await locate(before.anchor.id)
  await button.first().click()
  await page.waitForTimeout(3000)
  const after = await snapshot()
  const a = await locate(before.anchor.id)
  console.log(`--- ${i} 回目 ---`)
  console.log(`  押す前 : scrollY=${before.scrollY} 総高=${before.docHeight} 描画=${before.rendered}行 添字[${before.range}]`)
  console.log(`           アンカー index=${before.anchor.index} top=${before.anchor.top} 帯=${beforeAnchor?.hasDateBand ? 'あり' : 'なし'} 高さ=${beforeAnchor?.height}`)
  console.log(`  押した後: scrollY=${after.scrollY} 総高=${after.docHeight} 描画=${after.rendered}行 添字[${after.range}]`)
  console.log(`           アンカー index=${a?.index} top=${a?.top} 帯=${a?.hasDateBand ? 'あり' : 'なし'} 高さ=${a?.height}（上に ${a?.rowsAbove} 行）`)
  console.log(`  添字の増加=${a ? a.index - before.anchor.index : '?'} 画素ズレ=${a ? a.top - before.anchor.top : '?'}px 高さ変化=${a && beforeAnchor ? a.height - beforeAnchor.height : '?'}px`)
}

await browser.close()
