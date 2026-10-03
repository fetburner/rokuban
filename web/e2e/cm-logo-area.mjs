// CM 検出のロゴ位置を教える画面の実ブラウザ判定。
//
// jsdom では表示比・ポインタ座標・カーソルを測れないため、CM ロゴ画面の契約を
// 実ブラウザで判定する。API はブラウザ側で差し替えるので、mirakc・DB・原本は要らない。
// 判定側は画面の実装を import せず、coded size と実測した表示寸法から期待値を計算する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:cm-logo-area

import { GetRecordingResponse, ListCMLogosResponseItem, ListRecordingsResponseItem } from '../src/api/zod.ts'
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
const ng = []
const longError = `raw-log-${'x'.repeat(4096)}`

const logo = {
  networkId: 32678,
  serviceId: 5168,
  serviceName: 'e2e CM ロゴ局',
  site: 'tokyo',
  state: 'failed',
  recordingCount: 2,
  failedCount: 1,
  pendingCount: 0,
  lastFailureStage: 'logo',
  detectedCount: 0,
  redetectableCount: 0,
  // L-4 前の zod 契約でも検証できるように残す。L-4 後は未知フィールドとして無視される。
  lastError: longError,
  frameRecordingId: 7,
}

// ブラウザが画像として認識できればよい 1x1 PNG。画像の画素数は coded size と分ける。
const FRAME_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

// ロゴのプレビュー用。幅 100px 超の実寸（今のロゴ 240x120、候補 200x100）にして、
// 狭い列での縮み（SAR が掛からない）と見切れを実ブラウザで測れるようにする。
const CURRENT_LOGO_PNG = 'iVBORw0KGgoAAAANSUhEUgAAAPAAAAB4CAIAAABD1OhwAAABW0lEQVR4nO3SQQkAMAzAwMqpfxWTNRODQTg4AXlkzi5kzPcCeMjQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUkxNCmGJsXQpBiaFEOTYmhSDE2KoUm5FI4TNhwFSAEAAAAASUVORK5CYII='
const CANDIDATE_LOGO_PNG = 'iVBORw0KGgoAAAANSUhEUgAAAMgAAABkCAIAAABM5OhcAAABG0lEQVR4nO3SUQkAIBTAQOO8/imMZQmHIAcXYB9bMxuuW88L+JKxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLhLFIGIuEsUgYi4SxSBiLxAHyZHf8ZP6KHgAAAABJRU5ErkJggg=='
const SAR = 4 / 3

let savedArea
let candidate
let adopted = false
let adoptBody
const frameRequests = []

// 録画 7 の詳細。スライダーの範囲（尺）はここから決まる。
const recording7 = {
  id: 7,
  site: 'tokyo',
  source: 'rule',
  serviceName: logo.serviceName,
  channelType: 'GR',
  channel: '27',
  networkId: logo.networkId,
  serviceId: logo.serviceId,
  eventId: 1,
  title: 'e2e 番組',
  startAt: '2026-01-01T00:00:00Z',
  durationMs: 1800000,
  status: 'finished',
  keepOriginal: 'always',
  sizeBytes: 123456789,
  // L-4 後は生ログが録画の cmDetection.error に載る。現行の zod では未知フィールドとして無視される。
  cmDetection: { state: 'failed', error: longError },
  createdAt: '2026-01-01T00:00:00Z',
}

// 720x480 SAR 8:9。SAR でしか 4:3 と 16:9 を区別できない録画。
const recording8 = { ...recording7, id: 8, title: 'e2e 720x480' }
const frameHeaders = {
  7: { 'X-Coded-Width': '1440', 'X-Coded-Height': '1080', 'X-Sample-Aspect-Ratio': '4:3' },
  8: { 'X-Coded-Width': '720', 'X-Coded-Height': '480', 'X-Sample-Aspect-Ratio': '8:9' },
}

async function apiHandler({ path, url, json, route }) {
  const method = route.request().method()
  if (path === '/api/sites') return json(['tokyo'])
  if (path === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (path === '/api/breakers') return json([])
  if (path === '/api/events') return sseKeepAlive(route)
  if (path === '/api/cm-logos' && method === 'GET') {
    return json([{
      ...logo,
      pendingCount: adopted ? 2 : logo.pendingCount,
      ...(savedArea === undefined ? {} : { logoArea: { ...savedArea, updatedAt: '2026-01-02T00:00:00Z' } }),
      ...(candidate === undefined ? {} : { candidate }),
      ...(candidate?.state === 'ready' || adopted
        ? {
            previewPng: CURRENT_LOGO_PNG,
            codedWidth: 1440,
            codedHeight: 1080,
            learnedAt: adopted ? '2026-01-03T00:00:00Z' : '2026-01-01T00:00:00Z',
          }
        : {}),
    }])
  }
  if (path === '/api/recordings' && method === 'GET') return json([recording7, recording8])
  if (path === '/api/recordings/7' && method === 'GET') return json(recording7)
  const frameMatch = path.match(/^\/api\/media\/recordings\/(\d+)\/frame$/)
  if (frameMatch && method === 'GET') {
    frameRequests.push(url.searchParams.get('at'))
    return route.fulfill({
      status: 200,
      contentType: 'image/png',
      headers: frameHeaders[frameMatch[1]],
      body: FRAME_PNG,
    })
  }
  if (path === '/api/cm-logos/32678/5168/candidate/adopt' && method === 'POST') {
    adoptBody = JSON.parse(route.request().postData() ?? '{}')
    adopted = true
    candidate = undefined
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/area' && method === 'PUT') {
    // 実バックエンドの PutCMLogoArea は旧候補を消して job を積むだけで、running 行は
    // worker が job を拾ってから作る。ここでも candidate は作らない。
    candidate = undefined
    savedArea = JSON.parse(route.request().postData() ?? '{}')
    return route.fulfill({ status: 204 })
  }
  if (path === '/api/cm-logos/32678/5168/area' && method === 'DELETE') {
    savedArea = undefined
    return route.fulfill({ status: 204 })
  }
  return json([])
}

async function check(label, fn) {
  try {
    await fn()
  } catch (error) {
    ng.push(`${label}: ${error instanceof Error ? error.message : String(error)}`)
  }
}

function numberInput(page, label) {
  return page.locator(`input[type="number"][aria-label="${label}"]`)
}

// 点 (x, y) の最前面の要素の computed cursor。透明オーバーレイが覆っていれば、それが見える。
async function cursorAt(page, point) {
  // 利用者に見えるカーソルはポインタがそこにあるときの値なので、実際に動かしてから読む。
  await page.mouse.move(point.x, point.y)
  return page.evaluate(
    ({ x, y }) => {
      const element = document.elementFromPoint(x, y)
      return element ? getComputedStyle(element).cursor : '(要素なし)'
    },
    point,
  )
}

async function centerOf(locator, name) {
  await locator.scrollIntoViewIfNeeded()
  const box = await locator.boundingBox()
  if (!box) throw new Error(`${name} の寸法が取れない`)
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 }
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ['CM ロゴ状態', ListCMLogosResponseItem, logo],
    ['録画 7', GetRecordingResponse, recording7],
    ['録画 7（一覧）', ListRecordingsResponseItem, recording7],
  ],
  ng,
)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
page.setDefaultTimeout(5000)
await installApiStubs(page, apiHandler)
await page.goto(`${URL_BASE}/cm-logos/32678/5168?recording=7`, {
  waitUntil: 'domcontentloaded',
})

log('\n=== ① SAR を掛けた表示比 ===')
await check('①', async () => {
  const image = page.getByTestId('cm-logo-frame-image')
  await image.waitFor({ state: 'visible', timeout: 15000 })
  await page.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-frame-image"]')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
  const box = await image.boundingBox()
  if (!box || box.width <= 0 || box.height <= 0) throw new Error('コマの描画寸法が取れない')
  const ratio = box.width / box.height
  if (Math.abs(ratio / (16 / 9) - 1) > 0.01) {
    throw new Error(`表示比 ${ratio.toFixed(4)}（期待 16:9）`)
  }
  // 画像が枠箱を埋める（帯が残らない）
  const outer = await page.getByTestId('cm-logo-frame').boundingBox()
  if (!outer || Math.abs(outer.width - box.width) > 1 || Math.abs(outer.height - box.height) > 1) {
    throw new Error(`画像 ${box.width}x${box.height} が枠箱 ${outer?.width}x${outer?.height} を埋めない`)
  }
})

log('\n=== ② スライダーの時刻を /frame?at= に渡す ===')
await check('②', async () => {
  const slider = page.getByTestId('cm-logo-time')
  await slider.waitFor({ state: 'visible', timeout: 5000 })
  const image = page.getByTestId('cm-logo-frame-image')
  const previousFrameSrc = await image.getAttribute('src')
  if (!previousFrameSrc) throw new Error('変更前のコマ画像が無い')
  const min = Number(await slider.getAttribute('min'))
  const max = Number(await slider.getAttribute('max'))
  if (!(max > min)) throw new Error(`スライダーの範囲が空（min=${min} max=${max}）`)
  const initial = await slider.inputValue()
  // 利用者の操作で動かす（fill は input/change だけで、離したら確定する実装を落とす）。
  const box = await slider.boundingBox()
  if (!box) throw new Error('スライダーの寸法が取れない')
  frameRequests.length = 0
  await page.mouse.click(box.x + box.width * 0.37, box.y + box.height / 2)
  const target = await slider.inputValue()
  if (target === initial) throw new Error(`トラックをクリックしても値が動かない（${initial}）`)
  for (let i = 0; i < 30 && !frameRequests.includes(target); i++) await page.waitForTimeout(100)
  if (!frameRequests.includes(target)) {
    throw new Error(`/frame?at=${target} が呼ばれない（実際 ${JSON.stringify(frameRequests)}）`)
  }
  // 要求の発行だけで進むと、React が frame=null を描画する前に③が旧画像の上でドラッグを始める。
  // 直後に frame が null になり onPointerMove が pointermove を捨てて最小枠が残る（⑤のハンドル重なりも同じ原因）。
  await page.waitForFunction(
    (oldSrc) => {
      const current = document.querySelector('[data-testid="cm-logo-frame-image"]')
      return current instanceof HTMLImageElement && current.src !== oldSrc && current.complete && current.naturalWidth > 0
    },
    previousFrameSrc,
    { timeout: 15000 },
  )
})

log('\n=== ③ 4 隅の変形を coded size へ変換する ===')
await check('③', async () => {
  // ① と同じ要素の描画寸法を分母にする。
  const frameBox = await page.getByTestId('cm-logo-frame-image').boundingBox()
  if (!frameBox || frameBox.width <= 0 || frameBox.height <= 0) throw new Error('コマの描画寸法が取れない')

  const start = { x: frameBox.x + frameBox.width * 0.2, y: frameBox.y + frameBox.height * 0.2 }
  const end = { x: frameBox.x + frameBox.width * 0.55, y: frameBox.y + frameBox.height * 0.5 }
  await page.mouse.move(start.x, start.y)
  await page.mouse.down()
  await page.mouse.move(end.x, end.y)
  await page.mouse.up()

  const rect = page.getByTestId('cm-logo-rect')
  await rect.waitFor({ state: 'visible', timeout: 5000 })
  const before = await rect.boundingBox()
  const handleCenter = await centerOf(page.getByTestId('cm-logo-handle-se'), '右下ハンドル')
  if (!before) throw new Error('枠が表示されない')

  const dx = 23
  const dy = 17
  const expected = {
    w: Math.round(((before.width + dx) * 1440) / frameBox.width),
    h: Math.round(((before.height + dy) * 1080) / frameBox.height),
  }
  await page.mouse.move(handleCenter.x, handleCenter.y)
  await page.mouse.down()
  await page.mouse.move(handleCenter.x + dx, handleCenter.y + dy)
  await page.mouse.up()

  await page.getByRole('button', { name: 'ロゴを解析' }).click()
  for (let i = 0; i < 30 && savedArea === undefined; i++) await page.waitForTimeout(100)
  if (savedArea === undefined) throw new Error('PUT /area が届かない')
  if (savedArea.recordingId !== 7) throw new Error(`PUT body の recordingId が ${savedArea.recordingId}（期待 7）`)
  if ('atMs' in savedArea) throw new Error('全編解析なのに PUT body に atMs が載っている')
  const { w, h } = savedArea
  if (!Number.isFinite(w) || !Number.isFinite(h)) throw new Error(`PUT body の w/h が有限数でない（${JSON.stringify(savedArea)}）`)
  if (Math.abs(w - expected.w) > 1 || Math.abs(h - expected.h) > 1) {
    throw new Error(`保存 w/h が ${w}×${h}（期待 ${expected.w}×${expected.h} ±1）`)
  }
})

log('\n=== ④ 数値入力との往復 ===')
await check('④', async () => {
  const image = await page.getByTestId('cm-logo-frame-image').boundingBox()
  const rect = page.getByTestId('cm-logo-rect')
  if (!image) throw new Error('コマの描画寸法が取れない')
  const fill = async (label, value) => {
    const input = numberInput(page, label)
    await input.fill(String(value))
    await input.press('Tab')
    await page.waitForTimeout(50)
  }
  await fill('X', 120)
  await fill('幅', 200)
  const box = await rect.boundingBox()
  if (!box) throw new Error('枠の寸法が取れない')
  const want = { x: image.x + (120 * image.width) / 1440, w: (200 * image.width) / 1440 }
  if (Math.abs(box.x - want.x) > 1 || Math.abs(box.width - want.w) > 1) {
    throw new Error(
      `X=120 幅=200 の枠が x=${box.x.toFixed(1)} w=${box.width.toFixed(1)}（期待 x=${want.x.toFixed(1)} w=${want.w.toFixed(1)} ±1）`,
    )
  }

  const x = numberInput(page, 'X')
  const beforeX = Number(await x.inputValue())
  const center = await centerOf(rect, '枠')
  const move = 12
  await page.mouse.move(center.x, center.y)
  await page.mouse.down()
  await page.mouse.move(center.x + move, center.y)
  await page.mouse.up()
  const afterX = Number(await x.inputValue())
  const wantX = beforeX + (move * 1440) / image.width
  if (Math.abs(afterX - wantX) > 1) {
    throw new Error(`${move}px ドラッグ後の X が ${afterX}（期待 ${wantX.toFixed(1)} ±1）`)
  }
})

log('\n=== ⑤ カーソル（各点の最前面の要素で見る）===')
await check('⑤', async () => {
  const image = page.getByTestId('cm-logo-frame-image')
  await image.scrollIntoViewIfNeeded()
  const box = await image.boundingBox()
  if (!box) throw new Error('コマの描画寸法が取れない')
  const points = [
    ['スライダー', await centerOf(page.getByTestId('cm-logo-time'), 'スライダー'), 'pointer'],
    ['枠の外のコマ', { x: box.x + box.width * 0.95, y: box.y + box.height * 0.95 }, 'crosshair'],
  ]
  // 上のスクロールでコマの座標が動かないよう、コマはスライダーの後で測り直す。
  const box2 = await image.boundingBox()
  if (box2) points[1][1] = { x: box2.x + box2.width * 0.95, y: box2.y + box2.height * 0.95 }
  points.push(['枠の中心', await centerOf(page.getByTestId('cm-logo-rect'), '枠'), 'move'])
  for (const [corner, cursor] of [['nw', 'nwse-resize'], ['ne', 'nesw-resize'], ['sw', 'nesw-resize'], ['se', 'nwse-resize']]) {
    points.push([`ハンドル ${corner}`, await centerOf(page.getByTestId(`cm-logo-handle-${corner}`), corner), cursor])
  }
  const bad = []
  for (const [name, point, want] of points) {
    const got = await cursorAt(page, point)
    if (got !== want) bad.push(`${name}=${got}（期待 ${want}）`)
  }
  if (bad.length > 0) throw new Error(bad.join(' / '))
})

log('\n=== ⑥ 局の画面に生ログを出さない ===')
await check('⑥', async () => {
  // 局の画面が描かれていなければ「出ない」は空虚に通るので、先に局名を待つ。
  await page.getByText(logo.serviceName).first().waitFor({ state: 'attached', timeout: 10000 })
  // 閉じた <details> の中の生ログも「画面に出さない」に反するので textContent で見る。
  const text = await page.locator('body').evaluate((element) => element.textContent ?? '')
  if (text.includes(longError)) throw new Error('4KB の生ログが DOM に現れる')
})

log('\n=== ⑧ 数値入力へ打鍵できる（入力中に丸めない）===')
await check('⑧', async () => {
  const w = numberInput(page, '幅')
  await w.click()
  await page.keyboard.press('ControlOrMeta+A')
  await page.keyboard.type('400')
  await page.keyboard.press('Enter')
  if ((await w.inputValue()) !== '400') throw new Error(`幅に 400 を打つと ${await w.inputValue()}`)
  const x = numberInput(page, 'X')
  await x.click()
  await page.keyboard.press('ControlOrMeta+A')
  await page.keyboard.type('500')
  await page.keyboard.press('Tab')
  await x.click()
  await page.keyboard.press('End')
  await page.keyboard.press('Backspace')
  if ((await x.inputValue()) !== '50') throw new Error(`X の 500 から Backspace すると ${await x.inputValue()}（期待 50）`)
  await page.keyboard.press('Tab')
  if ((await x.inputValue()) !== '50') throw new Error(`X を 50 で確定すると ${await x.inputValue()}`)
})

log('\n=== ⑨ 角から離れた位置でもハンドルで変形する ===')
await check('⑨', async () => {
  const rect = page.getByTestId('cm-logo-rect')
  const before = await rect.boundingBox()
  const handleBox = await page.getByTestId('cm-logo-handle-se').boundingBox()
  const xBefore = await numberInput(page, 'X').inputValue()
  const yBefore = await numberInput(page, 'Y').inputValue()
  const wBefore = Number(await numberInput(page, '幅').inputValue())
  if (!before || !handleBox) throw new Error('枠またはハンドルが無い')
  // ハンドルは 44px 角。中心から斜めに 13px ずらすと角から約 18px 離れる
  const sx = handleBox.x + handleBox.width / 2 + 13
  const sy = handleBox.y + handleBox.height / 2 + 13
  await page.mouse.move(sx, sy)
  const hit = await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.closest('[data-testid^="cm-logo-handle-"]')?.getAttribute('data-testid')?.slice('cm-logo-handle-'.length) ?? null, [sx, sy])
  if (hit !== 'se') throw new Error(`角から 18px の elementFromPoint が se ハンドルでない: ${hit}`)
  await page.mouse.down()
  await page.mouse.move(sx + 20, sy)
  await page.mouse.up()
  const xAfter = await numberInput(page, 'X').inputValue()
  const yAfter = await numberInput(page, 'Y').inputValue()
  const wAfter = Number(await numberInput(page, '幅').inputValue())
  if (xAfter !== xBefore || yAfter !== yBefore) throw new Error(`新しい枠を描いた: X,Y ${xBefore},${yBefore} → ${xAfter},${yAfter}`)
  if (wAfter <= wBefore) throw new Error(`幅が増えない ${wBefore} → ${wAfter}`)
  log(`  幅 ${wBefore} → ${wAfter}（X,Y は ${xAfter},${yAfter} のまま）`)
})

log('\n=== ⑩ 枠に寄ったまま枠を動かすと、枠はポインタに付いてくる ===')
await check('⑩', async () => {
  await page.getByRole('button', { name: '枠に寄る' }).click()
  await page.waitForTimeout(100)
  const rect = page.getByTestId('cm-logo-rect')
  const before = await rect.boundingBox()
  if (!before) throw new Error('枠が無い')
  const cx = before.x + before.width / 2
  const cy = before.y + before.height / 2
  await page.mouse.move(cx, cy)
  await page.mouse.down()
  await page.mouse.move(cx + 10, cy, { steps: 2 })
  await page.mouse.move(cx + 20, cy, { steps: 2 })
  await page.mouse.up()
  const after = await rect.boundingBox()
  if (!after || Math.abs(after.x - before.x - 20) > 2) {
    throw new Error(`ポインタを 20px 動かして枠が ${after ? after.x - before.x : NaN}px 動いた`)
  }
  log(`  ポインタ 20px → 枠 ${(after.x - before.x).toFixed(1)}px`)
  await page.getByRole('button', { name: '全体' }).click()
})

log('\n=== ⑪ 整数に丸めて保存する ===')
await check('⑪', async () => {
  const rect = page.getByTestId('cm-logo-rect')
  const box = await rect.boundingBox()
  if (!box) throw new Error('枠が無い')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
  await page.mouse.down()
  await page.mouse.move(box.x + box.width / 2 + 7.3, box.y + box.height / 2 + 3.7)
  await page.mouse.up()
  for (const label of ['X', 'Y', '幅', '高さ']) {
    const value = await numberInput(page, label).inputValue()
    if (!/^\d+$/.test(value)) throw new Error(`${label}=${value} が整数でない`)
  }
})

log('\n=== ⑫ 720x480 SAR 8:9 の表示枠は 4:3 で、画像が埋める ===')
await check('⑫', async () => {
  const other = await context.newPage()
  await installApiStubs(other, apiHandler)
  await other.goto(`${URL_BASE}/cm-logos/32678/5168?recording=8`, { waitUntil: 'domcontentloaded' })
  await other.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-frame-image"]')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
  const frame = await other.getByTestId('cm-logo-frame').boundingBox()
  const image = await other.getByTestId('cm-logo-frame-image').boundingBox()
  if (!frame || !image) throw new Error('寸法が取れない')
  // 720 * 8/9 : 480 = 640 : 480 = 4:3
  const ratio = frame.width / frame.height
  log(`  枠箱の比 ${ratio.toFixed(4)}`)
  if (Math.abs(ratio / (4 / 3) - 1) > 0.01) throw new Error(`枠箱の比 ${ratio.toFixed(4)}（期待 4:3）`)
  if (Math.abs(image.width - frame.width) > 1 || Math.abs(image.height - frame.height) > 1) {
    throw new Error(`画像 ${image.width}x${image.height} が枠箱 ${frame.width}x${frame.height} を埋めない`)
  }
  // 録画 8 を表示して解析すると、PUT body の recordingId が 8 になる（表示中の録画が載る）。
  for (const [label, value] of Object.entries({ X: 10, Y: 20, 幅: 100, 高さ: 80 })) {
    const input = numberInput(other, label)
    await input.click()
    await other.keyboard.press('ControlOrMeta+A')
    await other.keyboard.type(String(value))
  }
  savedArea = undefined
  await other.getByRole('button', { name: 'ロゴを解析' }).click()
  for (let i = 0; i < 30 && savedArea === undefined; i++) await other.waitForTimeout(100)
  if (savedArea === undefined) throw new Error('録画 8 の PUT /area が届かない')
  if (savedArea.recordingId !== 8) throw new Error(`録画 8 を表示して解析したのに recordingId が ${savedArea.recordingId}`)
  await other.close()
})

log('\n=== ⑬ 400px 幅で数値入力だけで保存できる ===')
await check('⑬', async () => {
  const narrow = await context.newPage()
  await narrow.setViewportSize({ width: 400, height: 860 })
  await installApiStubs(narrow, apiHandler)
  await narrow.goto(`${URL_BASE}/cm-logos/32678/5168?recording=7`, { waitUntil: 'domcontentloaded' })
  await narrow.waitForFunction(
    () => (document.querySelector('[data-testid="cm-logo-frame-image"]')?.naturalWidth ?? 0) > 0,
    undefined,
    { timeout: 15000 },
  )
  const scrollWidth = await narrow.evaluate(() => document.documentElement.scrollWidth)
  if (scrollWidth > 400) throw new Error(`400px 幅で横スクロールがある（scrollWidth=${scrollWidth}）`)
  const want = { X: 100, Y: 200, 幅: 300, 高さ: 150 }
  for (const [label, value] of Object.entries(want)) {
    const input = numberInput(narrow, label)
    await input.scrollIntoViewIfNeeded()
    await input.click()
    await narrow.keyboard.press('ControlOrMeta+A')
    await narrow.keyboard.type(String(value))
  }
  // Tab も Enter も押さず、入力中のまま保存を押す（blur で確定してから保存される）
  savedArea = undefined
  await narrow.getByRole('button', { name: 'ロゴを解析' }).click()
  for (let i = 0; i < 30 && savedArea === undefined; i++) await narrow.waitForTimeout(100)
  if (savedArea === undefined) throw new Error('PUT /area が届かない')
  const got = { X: savedArea.x, Y: savedArea.y, 幅: savedArea.w, 高さ: savedArea.h }
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    throw new Error(`PUT body が ${JSON.stringify(got)}（期待 ${JSON.stringify(want)}）`)
  }
  if (savedArea.recordingId !== 7 || 'atMs' in savedArea) {
    throw new Error(`PUT body が表示中の録画 7 だけを指さない（${JSON.stringify(savedArea)}）`)
  }
  await narrow.close()
})

log('\n=== ⑭ 解析待ち（candidate の行が無い間）の表示とポーリング ===')
const candidateBase = {
  x: 400,
  y: 300,
  w: 400,
  h: 300,
  codedWidth: 1440,
  codedHeight: 1080,
  recordingId: 7,
  attemptedAt: '2026-01-02T00:00:00Z',
}
const candidatePage = await context.newPage()
await installApiStubs(candidatePage, apiHandler)
await check('⑭', async () => {
  // 枠は保存済みで candidate の行がまだ無い。メモリ state の無い開き直したページでも解析中を出す。
  savedArea = { recordingId: 7, ...candidateBase }
  candidate = undefined
  await candidatePage.goto(`${URL_BASE}/cm-logos/32678/5168?recording=7`, { waitUntil: 'domcontentloaded' })
  await candidatePage.getByTestId('cm-logo-candidate-running').waitFor({ timeout: 5000 })
  // candidate が無い間もポーリングして、worker が作った failed を拾う。
  candidate = { state: 'failed', stage: 'area', ...candidateBase }
  await candidatePage.getByTestId('cm-logo-candidate-failed').waitFor({ timeout: 8000 })
  const text = await candidatePage.getByTestId('cm-logo-candidate-failure-message').textContent()
  if (!text?.includes('教えた枠が録画の解像度と合わない')) throw new Error(`failed の工程文が違う（${text}）`)
})

log('\n=== ⑮ 候補と今のロゴのプレビュー寸法 ===')
await check('⑮', async () => {
  candidate = { state: 'ready', previewPng: CANDIDATE_LOGO_PNG, ...candidateBase }
  await candidatePage.reload({ waitUntil: 'domcontentloaded' })
  await candidatePage.getByTestId('cm-logo-candidate-ready').waitFor({ timeout: 5000 })
  // 今のロゴと候補の両方で「描画幅 = naturalWidth × SAR、描画高さ = naturalHeight」かつ
  // 各 img が自分の欄に収まっている（スクロールしないと見えない状態でない）ことを測る。
  for (const width of [1280, 1024]) {
    await candidatePage.setViewportSize({ width, height: 900 })
    for (const [testId, name, w, h] of [
      ['cm-logo-current-preview', '今のロゴ', 240, 120],
      ['cm-logo-candidate-preview', '候補', 200, 100],
    ]) {
      await candidatePage.waitForFunction(
        (id) => (document.querySelector(`[data-testid="${id}"] img`)?.naturalWidth ?? 0) > 0,
        testId,
      )
      const m = await candidatePage.getByTestId(testId).evaluate((element) => {
        const image = element.querySelector('img')
        const outer = element.getBoundingClientRect()
        const rect = image.getBoundingClientRect()
        return {
          natural: [image.naturalWidth, image.naturalHeight],
          width: rect.width,
          height: rect.height,
          left: rect.left - outer.left,
          right: outer.right - rect.right,
          scrolls: element.scrollWidth > element.clientWidth,
        }
      })
      log(`  ${width}px ${name}: ${JSON.stringify(m)}`)
      if (m.natural[0] !== w || m.natural[1] !== h) throw new Error(`${width}px ${name}の画像が読めていない（natural ${m.natural}）`)
      if (Math.abs(m.width - w * SAR) > 1 || Math.abs(m.height - h) > 1) {
        throw new Error(`${width}px ${name}が原寸×SAR でない（${m.width}x${m.height} / 期待 ${w * SAR}x${h}）`)
      }
      if (m.left < 0 || m.right < 0 || m.scrolls) {
        throw new Error(`${width}px ${name}が欄から見切れている（左 ${m.left} / 右 ${m.right} / scroll ${m.scrolls}）`)
      }
    }
  }
  await candidatePage.setViewportSize({ width: 1280, height: 900 })
})

log('\n=== ⑯ ready 候補を redetect=false で採用する ===')
await check('⑯', async () => {
  await candidatePage.getByTestId('cm-logo-candidate-redetect').uncheck()
  await candidatePage.getByTestId('cm-logo-candidate-adopt').click()
  await candidatePage.getByTestId('cm-logo-candidate-adopted').waitFor({ timeout: 5000 })
  if (JSON.stringify(adoptBody) !== JSON.stringify({ redetect: false })) {
    throw new Error(`採用 body が違う（実際 ${JSON.stringify(adoptBody)}）`)
  }
  const text = await candidatePage.getByTestId('cm-logo-candidate-adopted').textContent()
  if (!text?.includes('検出待ち 2 件')) throw new Error(`採用後の検出待ちメッセージが違う（${text}）`)
})
await candidatePage.close()

log('\n=== ⑦ 旧形式の URL は局のルートへ飛ぶ ===')
await check('⑦', async () => {
  await page.goto(`${URL_BASE}/cm-logos?network=32678&service=5168&recording=7`, {
    waitUntil: 'domcontentloaded',
  })
  await page.waitForURL((url) => url.pathname === '/cm-logos/32678/5168', { timeout: 5000 })
  const search = new URL(page.url()).searchParams
  if (search.get('recording') !== '7') throw new Error(`recording が引き継がれない（${page.url()}）`)
})

await finish(ng, browser)
