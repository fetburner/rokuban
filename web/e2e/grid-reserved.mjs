// 番組表グリッドの予約済み印の受け入れ判定（issue #307）。
//
// jsdom はレイアウトも色も測れないので、`pnpm test` の
// 「data-reserved / aria-label に『予約済み』がある」だけでは、
// 見える印が 1px の輪と 6px の点だけの実装を通せない
// （web/e2e/README.md「jsdom が測れないもの」、issue #307 の出典）。
// ここは実描画を見る:
//   ① 予約済みセルに、aria ではない見える「予約」がある。箱が 6px の点より
//      大きい（幅 16px 以上）。未予約セルには無い
//   ② 5 分（10px）の予約済みセルでも印が消えない --- セルの高さの 8 割以上を
//      覆う縦の帯がある（点は高さ 6px なので 10px セルでもこの判定をすり抜けない）
//   ③ 予約済み（未選択）と選択中（未予約）は別の形。予約済みだけに見える
//      「予約」があり、選択中だけに太い ring がある
//   ④ 予約の印はタリー / 琥珀 / destructive ではない（色は信号のみ）
//
// 別ファイルにしたのは、design.mjs が既にグリッドの現在時刻線・容量帯を持ち、
// 他 PR がそこを編集している可能性があるため（reserve-visibility.mjs と同じ理由）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:grid-reserved
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'

const ng = []
const log = (...a) => console.log(...a)

const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')
const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const service = {
  networkId: 32736,
  serviceId: 1024,
  name: 'NHK総合',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
}

// 3 本ともニュース（genres: [0] = bg-sky-50）。ジャンル色が同じでないと
// 「予約済みがジャンル色に埋もれる」を測れない。
const reserved = {
  programId: 307001,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 1,
  startAt: iso(nowMs + 6 * 60_000),
  endAt: iso(nowMs + 26 * 60_000),
  durationMs: 20 * 60_000,
  name: '予約確認ニュース',
  description: '',
  genres: [0],
  isFree: true,
}

const unreserved = {
  programId: 307002,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 2,
  startAt: iso(nowMs + 26 * 60_000),
  endAt: iso(nowMs + 46 * 60_000),
  durationMs: 20 * 60_000,
  name: '未予約ニュース',
  description: '',
  genres: [0],
  isFree: true,
}

// 5 分 = 10px（120px/時）。下限を設けないので、点や本文はここで切れる。
const shortReserved = {
  programId: 307003,
  networkId: service.networkId,
  serviceId: service.serviceId,
  eventId: 3,
  startAt: iso(nowMs + 46 * 60_000),
  endAt: iso(nowMs + 51 * 60_000),
  durationMs: 5 * 60_000,
  name: '短尺予約ニュース',
  description: '',
  genres: [0],
  isFree: true,
}

const reservation = (program) => ({
  id: program.programId,
  site: SITE,
  programId: program.programId,
  source: 'manual',
  state: 'active',
  title: program.name,
  startAt: program.startAt,
  durationMs: program.durationMs,
  createdAt: iso(nowMs - 3_600_000),
  updatedAt: iso(nowMs - 3_600_000),
  skip: false,
})

/** installApiStubs は /programs の描画に要る `/api/**` を丸ごと差し替える。 */
async function installApiStubs(page) {
  await page.route('**/api/**', async (route) => {
    const p = new URL(route.request().url()).pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: false })
    if (p === '/api/reservations') return json([reservation(reserved), reservation(shortReserved)])
    if (p === '/api/capacity/overages') return json([])
    if (p === '/api/encode-profiles') return json([])
    if (p === `/api/sites/${SITE}/services`) return json([service])
    if (p === `/api/sites/${SITE}/programs`) return json([reserved, unreserved, shortReserved])
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
    return json([])
  })
}

/**
 * readColor は oklch の計算値を 1px 塗って sRGB に落とす（design.mjs と同じ手）。
 * getComputedStyle の文字列を正規表現で読んではいけない。
 */
const readColor = (el, prop) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  ctx.clearRect(0, 0, 1, 1)
  ctx.fillStyle = getComputedStyle(el).getPropertyValue(prop)
  ctx.fillRect(0, 0, 1, 1)
  const d = ctx.getImageData(0, 0, 1, 1).data
  return [d[0], d[1], d[2], d[3]]
}

const isRed = ([r, g, b]) => r > 100 && r - g > 60 && r - b > 60 && Math.abs(g - b) < 60
const isAmber = ([r, g, b]) => r > g && g > b && r - b > 60 && g - b > 20

/**
 * inspectCell は予約印として見えるものを実レイアウトから拾う。
 *
 * 属性（data-reserved / aria-label）は見ない。見える「予約」の箱と、
 * セル高さをほぼ覆う細い縦帯だけを返す。直す前の実装（1px 輪 + 6px 点）では
 * どちらも空になる。
 */
const inspectCell = (el) => {
  const cell = el.getBoundingClientRect()
  const texts = []
  const walk = (node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      const t = node.textContent.trim()
      if (t === '予約') {
        const parent = node.parentElement
        const r = parent.getBoundingClientRect()
        const cs = getComputedStyle(parent)
        texts.push({
          width: r.width,
          height: r.height,
          visibility: cs.visibility,
          color: cs.color,
          backgroundColor: cs.backgroundColor,
        })
      }
    }
    for (const child of node.childNodes) walk(child)
  }
  walk(el)

  const bars = []
  for (const child of el.children) {
    const r = child.getBoundingClientRect()
    const cs = getComputedStyle(child)
    const bg = cs.backgroundColor
    if (
      r.height >= cell.height * 0.8 &&
      r.width > 0 &&
      r.width <= 12 &&
      cell.right - r.right <= 4 &&
      cs.visibility !== 'hidden'
    ) {
      bars.push({
        width: r.width,
        height: r.height,
        backgroundColor: bg,
      })
    }
  }

  return {
    cellWidth: cell.width,
    cellHeight: cell.height,
    boxShadow: getComputedStyle(el).boxShadow,
    texts,
    bars,
  }
}

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
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
await page.clock.setFixedTime(FIXED_NOW)
await installApiStubs(page)
await page.goto(BASE + '/programs', { waitUntil: 'domcontentloaded' })

await page.getByRole('button', { name: '番組表' }).click()
const grid = page.getByTestId('program-grid')
await grid.waitFor({ timeout: 15000 })

const reservedCell = page.locator(`[data-testid="program-grid-cell"][data-program-id="${reserved.programId}"]`)
const unreservedCell = page.locator(
  `[data-testid="program-grid-cell"][data-program-id="${unreserved.programId}"]`,
)
const shortCell = page.locator(
  `[data-testid="program-grid-cell"][data-program-id="${shortReserved.programId}"]`,
)
await reservedCell.waitFor({ timeout: 10000 })
await unreservedCell.waitFor()
await shortCell.waitFor()

// --- ① 予約済みに見える「予約」。未予約には無い ------------------------------
log('\n=== ① 見える「予約」 ===')
const reservedInfo = await reservedCell.evaluate(inspectCell)
const unreservedInfo = await unreservedCell.evaluate(inspectCell)
log(`  予約済みセル ${reservedInfo.cellWidth}x${reservedInfo.cellHeight} texts=${JSON.stringify(reservedInfo.texts)} bars=${JSON.stringify(reservedInfo.bars)}`)
log(`  未予約セル   ${unreservedInfo.cellWidth}x${unreservedInfo.cellHeight} texts=${JSON.stringify(unreservedInfo.texts)} bars=${JSON.stringify(unreservedInfo.bars)}`)

const visibleLabel = reservedInfo.texts.find(
  (t) => t.visibility === 'visible' && t.width >= 16 && t.height >= 8,
)
if (!visibleLabel) {
  ng.push(
    `① 予約済みセルに見える「予約」（幅 16px 以上）が無い（${JSON.stringify(reservedInfo.texts)}）`,
  )
}
if (unreservedInfo.texts.length > 0) {
  ng.push(`① 未予約セルに「予約」の文字がある（${JSON.stringify(unreservedInfo.texts)}）`)
}

// --- ② 5 分セルでも印が残る ------------------------------------------------
log('\n=== ② 短い予約済みセル ===')
const shortInfo = await shortCell.evaluate(inspectCell)
log(`  短尺セル ${shortInfo.cellWidth}x${shortInfo.cellHeight} texts=${JSON.stringify(shortInfo.texts)} bars=${JSON.stringify(shortInfo.bars)}`)
if (shortInfo.cellHeight < 8 || shortInfo.cellHeight > 14) {
  ng.push(`② 5 分セルの高さが 10px 近傍でない（${shortInfo.cellHeight}px）`)
}
const shortLabel = shortInfo.texts.find((t) => t.visibility === 'visible' && t.width >= 16)
const shortBar = shortInfo.bars[0]
if (!shortLabel && !shortBar) {
  ng.push(
    `② 5 分の予約済みセルに見える「予約」も高さ方向の帯も無い（texts=${JSON.stringify(shortInfo.texts)} bars=${JSON.stringify(shortInfo.bars)}）`,
  )
}

// --- ③ 予約済みと選択中は別の形 --------------------------------------------
log('\n=== ③ 予約済み ≠ 選択中 ===')
await unreservedCell.click()
await page.waitForTimeout(200)
const selectedPressed = await unreservedCell.getAttribute('aria-pressed')
const reservedPressed = await reservedCell.getAttribute('aria-pressed')
const selectedInfo = await unreservedCell.evaluate(inspectCell)
const reservedStill = await reservedCell.evaluate(inspectCell)
log(`  未予約を選択: aria-pressed=${selectedPressed} shadow=${selectedInfo.boxShadow}`)
log(`  予約済みのまま: aria-pressed=${reservedPressed} shadow=${reservedStill.boxShadow}`)

if (selectedPressed !== 'true') {
  ng.push(`③ 未予約セルを押しても aria-pressed が true にならない（${selectedPressed}）`)
}
if (selectedInfo.texts.some((t) => t.visibility === 'visible')) {
  ng.push('③ 選択中の未予約セルに見える「予約」がある（予約と選択が同じ印）')
}
const reservedLabelAfter = reservedStill.texts.find(
  (t) => t.visibility === 'visible' && t.width >= 16,
)
if (!reservedLabelAfter) {
  ng.push('③ 未予約を選んだあと、予約済みセルの見える「予約」が消えた')
}
// 選択中は ring（box-shadow）。予約済み未選択は輪以外の印なので、
// 同じ box-shadow だけが差、という現状を落とす。
const sameShadow =
  selectedInfo.boxShadow !== 'none' &&
  reservedStill.boxShadow !== 'none' &&
  selectedInfo.boxShadow.replace(/\d+(\.\d+)?px/g, '') ===
    reservedStill.boxShadow.replace(/\d+(\.\d+)?px/g, '')
if (sameShadow && !reservedLabelAfter) {
  ng.push('③ 予約済みと選択中の差が ring の太さだけで、別の形が無い')
}

// --- ④ 印の色は信号色ではない ----------------------------------------------
log('\n=== ④ 色は信号のみ ===')
const markSource = visibleLabel ?? shortLabel ?? shortBar
if (!markSource) {
  ng.push('④ 測る印が無い（①②が落ちている）')
} else {
  const colorTarget = visibleLabel
    ? reservedCell.getByText('予約', { exact: true }).first()
    : shortBar
      ? shortCell.locator(':scope > *').nth(0)
      : reservedCell
  const fg = await colorTarget.evaluate(readColor, 'color').catch(() => null)
  const bg = await colorTarget.evaluate(readColor, 'background-color').catch(() => null)
  log(`  印 文字=${fg} 地=${bg}`)
  for (const [name, rgba] of [
    ['文字', fg],
    ['地', bg],
  ]) {
    if (!rgba) continue
    if (rgba[3] === 0) continue
    if (isRed(rgba) || isAmber(rgba)) {
      ng.push(`④ 予約の印の${name}が信号色（${rgba}）`)
    }
  }
}

await context.close()
await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
