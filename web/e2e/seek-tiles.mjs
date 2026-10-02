// シークバーのプレビュー（タイル画像、issue #873）の実ブラウザ判定。
//
// **jsdom では原理的に測れないものだけを見る。** プレビューの位置は
// 「ポインタの x → 動画内の割合 → 再生位置 → タイルの格子位置」で決まるが、
// jsdom の `getBoundingClientRect()` は常に 0 を返すので、この経路は
// 単体テストでは 1 歩も進まない（`recording-player.test.tsx` が見るのは
// 「問い合わせが始まること」だけ）。CLAUDE.md §テスト規律のとおり、
// 実装より先にここで判定手段を作る。
//
// 実装後の判定契約: `<video>` に native controls はなく、プレイヤー内の
// `seek-scrub` 1 本が role=slider を持ち、動画下端に重なる。そのシークバー上で
// プレビュー・クリック位置が一致する。旧実装（native controls + 別帯）では
// 最初の検査が失敗する。
//
// 見るのは 7 点:
//   ① 帯の上のホバー位置に対応するタイルが出る（列の折り返しと行送りを別々の位置で固定）
//   ② プレビューが帯の幅に収まり、帯そのものを覆わない
//   ③ ポインタが帯から離れると消え、動画の映像の上では出ない（両方向）
//   ④ タイルが無い録画（404）ではプレビューが出ず、再生面は従来のまま
//   ⑤ 帯をクリックすると、その位置でプレビューに出ていたタイルの時刻へ飛ぶ
//   ⑥ 帯が 1 枚ぶん（320px）より狭い画面でも、プレビューが帯の幅に収まる
//   ⑦ タッチではタップで飛ぶだけで、プレビューは出ない（pointerleave が来ないので居座る）
//
// フィクスチャは ffmpeg で作る（動画の長さが判定に要る）。無い環境では
// この判定だけを skip として終了する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:seek-tiles
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, statSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { ListRecordingsResponseItem } from '../src/api/zod.ts'
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
const EVIDENCE_DIR = process.env.E2E_SHOT_DIR
const browserEngine = process.env.E2E_BROWSER ?? 'chromium'
const videoContentType = browserEngine === 'webkit' ? 'video/mp4' : 'video/webm'
if (EVIDENCE_DIR) mkdirSync(EVIDENCE_DIR, { recursive: true })
const ng = []

// タイルの形。**web/src/lib/seek-tiles.ts と同じ値をここにも書く**のは意図的で、
// 判定側が実装から値を import すると「実装がどう変わっても同じ値で比較する」
// 循環になり何も主張しなくなる。
const TILE_INTERVAL_SECONDS = 10
const TILE_DISPLAY_WIDTH = 320
const TILE_DISPLAY_HEIGHT = 180
const TILE_COLUMNS = 10

const recording = {
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'プレビュー確認用',
  startAt: '2026-01-01T12:00:00.000Z',
  durationMs: 120_000,
  status: 'finished',
  keepOriginal: 'always', cmDetection: { state: 'disabled' },
  sizeBytes: 500_000_000,
  encodedAssets: [{ profile: 'h264', sizeBytes: 400_000_000 }],
  createdAt: '2026-01-02T12:30:00Z',
}

/** serveTiles が false の間はタイル配信だけ 404 を返す（④の判定用）。 */
let serveTiles = false

/** タイルの中身ではなく、背景位置が指す格子と実ブラウザでの描画を判定する。 */
let tileSprite

/**
 * 判定に使う動画を一度だけ作る。長さ（duration）が要るので実在の動画を配る。
 * Chromium / Firefox はビルドによって H.264 を持たないことがあるため VP8/WebM、
 * WebKit は実際の MP4 再生経路に合わせて H.264/MP4 を使う。
 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-seek-tiles')
  const webkit = browserEngine === 'webkit'
  const videoPath = path.join(fixtureDir, webkit ? 'clip.mp4' : 'clip.webm')
  if (existsSync(videoPath) && statSync(videoPath).size > 0) return videoPath

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(fixtureDir, { recursive: true })
  log(`判定用の動画フィクスチャを生成中... (${videoPath})`)
  const codecArgs = webkit
    ? ['-c:v', 'libx264', '-preset', 'ultrafast', '-movflags', '+faststart']
    : ['-c:v', 'libvpx', '-b:v', '30k']
  execFileSync(
    'ffmpeg',
    [
      '-y',
      '-f',
      'lavfi',
      '-i',
      'testsrc=size=160x90:rate=2',
      '-t',
      '120',
      ...codecArgs,
      '-pix_fmt',
      'yuv420p',
      videoPath,
    ],
    { stdio: 'ignore' },
  )
  return existsSync(videoPath) ? videoPath : undefined
}

async function apiHandler({ path: apiPath, url, json, route }) {
  const method = route.request().method()
  if (apiPath === '/api/sites') return json(['default'])
  if (apiPath === '/api/capabilities') return json({ live: false })
  if (apiPath === '/api/breakers') return json([])
  if (apiPath === '/api/encode-profiles' || apiPath === '/api/rules') return json([])
  if (apiPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (apiPath === '/api/capacity/overages') return json([])
  if (apiPath === '/api/events') return sseKeepAlive(route)
  if (apiPath === '/api/recordings' && method === 'GET') {
    return json(url.searchParams.get('trash') === 'true' ? [] : [recording])
  }
  if (/^\/api\/recordings\/1$/.test(apiPath) && method === 'GET') return json(recording)
  if (/^\/api\/media\/recordings\/1\/file$/.test(apiPath)) {
    // Range に応じる（実物の streamer と同じ）。応じないと Chromium は動画を
    // seekable にせず、⑤のクリックが 0 秒から動かない。
    const range = /bytes=(\d+)-(\d*)/.exec(route.request().headers().range ?? '')
    if (!range) return route.fulfill({ status: 200, contentType: videoContentType, body: videoBytes, headers: { 'Accept-Ranges': 'bytes' } })
    const start = Number(range[1])
    const end = range[2] ? Number(range[2]) : videoBytes.length - 1
    return route.fulfill({
      status: 206,
      contentType: videoContentType,
      body: videoBytes.subarray(start, end + 1),
      headers: { 'Accept-Ranges': 'bytes', 'Content-Range': `bytes ${start}-${end}/${videoBytes.length}` },
    })
  }
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(apiPath)) {
    return route.fulfill({ status: 404 })
  }
  if (/^\/api\/media\/recordings\/1\/seek-tiles$/.test(apiPath)) {
    if (!serveTiles) return route.fulfill({ status: 404 })
    return route.fulfill({ status: 200, contentType: 'image/png', body: tileSprite })
  }
  if (/^\/api\/recordings\/1\/chapters$/.test(apiPath)) {
    return json({
      source: 'auto',
      version: 'auto:detected:1',
      detectionPending: false,
      spans: [{ startMs: 30_000, endMs: 40_000, label: 'CM', cut: true }],
    })
  }
  return json([])
}

/** scrubPoint は帯の上で seconds に対応する座標を返す。 */
function scrubPoint(scrubBox, duration, seconds) {
  return { x: scrubBox.x + scrubBox.width * (seconds / duration), y: scrubBox.y + scrubBox.height / 2 }
}

/** moveToSeconds は「対応するタイルが出るはずの位置」へポインタを動かす。 */
async function moveToSeconds(page, scrubBox, duration, seconds) {
  const p = scrubPoint(scrubBox, duration, seconds)
  await page.mouse.move(p.x, p.y)
}

/** expectedTile は位置から期待する格子オフセットを独立に計算する。 */
function expectedTile(seconds) {
  const index = Math.floor(seconds / TILE_INTERVAL_SECONDS)
  return {
    index,
    x: -(index % TILE_COLUMNS) * TILE_DISPLAY_WIDTH,
    y: -Math.floor(index / TILE_COLUMNS) * TILE_DISPLAY_HEIGHT,
  }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const videoPath = ensureFixture()
if (videoPath === undefined) {
  log('  ffmpeg が無いため、シークプレビューの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}
const videoBytes = readFileSync(videoPath)
// スクリーンショットに 1×1 の単色タイルを拡大して写さないよう、動画フィクスチャ
// からプロダクトと同じ 320×180・10 列の sprite を作る。判定自体は tile sheet の
// 画素ではなく background-position を見るため、10×2 の大きさだけ合わせる。
tileSprite = execFileSync('ffmpeg', [
  '-hide_banner', '-loglevel', 'error', '-y', '-i', videoPath,
  '-vf', 'fps=1/10,scale=320:180,tile=10x2:padding=0:margin=0',
  '-frames:v', '1', '-f', 'image2pipe', '-vcodec', 'png', 'pipe:1',
])

const browser = await launchBrowser(browserEngine)
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

const openPlayer = async () => {
  await page.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
  const video = page.locator('video')
  await video.waitFor({ timeout: 15000 })
  // duration が確定するまで待つ（未確定だと実装は位置を出さない）。
  await page.waitForFunction(
    () => {
      const el = document.querySelector('video')
      return Number.isFinite(el?.duration) && el.duration > 0
    },
    undefined,
    { timeout: 15000 },
  )
  return video
}

log('\n=== ④ タイルが無い録画ではプレビューを出さず、再生面は従来のまま ===')
const noTilesVideo = await openPlayer()
const noTilesBox = await page.locator('[data-testid="seek-scrub"]').boundingBox()
const noTilesNativeControls = await noTilesVideo.evaluate((el) => el.hasAttribute('controls'))
if (noTilesNativeControls) {
  ng.push('プレイヤーの video に native controls が残っている')
}
const seekbar = page.locator('[data-testid="seek-scrub"]')
if ((await seekbar.count()) !== 1) {
  ng.push(`プレイヤー内の seek-scrub が 1 本ではない（count=${await seekbar.count()}）`)
} else {
  if ((await seekbar.getAttribute('role')) !== 'slider') {
    ng.push('seek-scrub に role="slider" がない')
  }
  const videoBox = await noTilesVideo.boundingBox()
  if (videoBox && noTilesBox) {
    const overlapsVideo =
      noTilesBox.x < videoBox.x + videoBox.width &&
      noTilesBox.x + noTilesBox.width > videoBox.x &&
      noTilesBox.y >= videoBox.y - 1 &&
      noTilesBox.y + noTilesBox.height <= videoBox.y + videoBox.height + 1
    if (!overlapsVideo) {
      ng.push('seek-scrub が映像下端に重なっていない')
    }
  }
  const playerSliders = page.locator('[data-testid="recording-player-frame"] [role="slider"]')
  if ((await playerSliders.count()) !== 1) {
    ng.push(`プレイヤー内の role="slider" が 1 本ではない（count=${await playerSliders.count()}）`)
  }
}
// タイルは 404 なので読み込まれない。duration は分かっているので、実装が
// 「タイルの有無を見ずに出す」ならここで出てしまう。
await moveToSeconds(page, noTilesBox, 120, 35)
await page.waitForTimeout(200)
await moveToSeconds(page, noTilesBox, 120, 36)
await page.waitForTimeout(500)
if ((await page.locator('[data-testid="seek-tile-preview"]').count()) !== 0) {
  ng.push('④ タイルが 404 なのにプレビューが出ている')
}
const srcWithoutTiles = await noTilesVideo.getAttribute('src')
if (srcWithoutTiles !== '/api/media/recordings/1/file?profile=h264') {
  ng.push(`④ タイルが無いだけで再生面が変わっている（src=${srcWithoutTiles}）`)
}
if ((await noTilesVideo.evaluate((v) => v.duration)) <= 0) {
  ng.push('④ タイルが無いと再生面が壊れている（duration が取れない）')
}

log('\n=== ①② タイルがあるときのホバー位置 → タイル ===')
serveTiles = true
const video = await openPlayer()
const duration = await video.evaluate((v) => v.duration)
if (Math.abs(duration - 120) > 5) {
  ng.push(`フィクスチャの長さが想定と違う（duration=${duration}）--- 判定の前提が崩れている`)
}
const videoBox = await video.boundingBox()
let scrubBox = await page.locator('[data-testid="seek-scrub"]').boundingBox()
if (!videoBox || videoBox.width <= 0 || !scrubBox || scrubBox.width <= 0) {
  ng.push('動画かスクラブ帯の矩形が取れない（レイアウトが想定と違う）')
  await finish(ng, browser)
}

/** hoverTile は 1 枚ぶんの位置へポインタを送り、プレビューが出るまで待つ。 */
async function hoverTile(seconds) {
  const want = expectedTile(seconds)
  // タイルの中ほどを狙う（端に寄せると duration の丸めで隣のタイルになる）。
  const target = want.index * TILE_INTERVAL_SECONDS + 5
  await moveToSeconds(page, scrubBox, duration, target)
  // 1 度目はタイルの取得が始まるだけなので、届くまで待ってからもう一度動かす。
  await page
    .waitForFunction(() => (document.querySelector('img[src*="/seek-tiles"]')?.naturalWidth ?? 0) > 0, undefined, {
      timeout: 15000,
    })
    .catch(() => {})
  await moveToSeconds(page, scrubBox, duration, target)
  return want
}

// 列の折り返し（3 → 9）と行送り（10）を別々の位置で押さえる。
for (const seconds of [35, 95, 105]) {
  const want = await hoverTile(seconds)
  const preview = page.locator('[data-testid="seek-tile-preview"]')
  try {
    await preview.waitFor({ timeout: 5000 })
  } catch {
    ng.push(`① ${seconds}s 相当（タイル #${want.index}）の位置でプレビューが出ない`)
    continue
  }

  const { position, image } = await preview.locator('> div').evaluate((el) => {
    const style = getComputedStyle(el)
    return { position: style.backgroundPosition, image: style.backgroundImage }
  })
  const wantPosition = `${want.x}px ${want.y}px`
  if (position !== wantPosition) {
    ng.push(`① タイル #${want.index} の background-position が ${position}（期待 ${wantPosition}）`)
  }
  if (!image.includes('/api/media/recordings/1/seek-tiles')) {
    ng.push(`① プレビューの背景がタイル配信を指していない（${image}）`)
  }

  // ② 帯の幅に収まり、帯そのものを覆わない（覆うとクリック先が見えない）。
  const previewBox = await preview.boundingBox()
  if (!previewBox) {
    ng.push(`② タイル #${want.index} のプレビューに実寸が無い`)
    continue
  }
  const scrubRight = scrubBox.x + scrubBox.width
  if (previewBox.x < scrubBox.x - 1 || previewBox.x + previewBox.width > scrubRight + 1) {
    ng.push(
      `② タイル #${want.index} のプレビューが横にはみ出している` +
        `（preview ${previewBox.x}..${previewBox.x + previewBox.width} / scrub ${scrubBox.x}..${scrubRight}）`,
    )
  }
  if (previewBox.y + previewBox.height > scrubBox.y + 1) {
    ng.push(`② タイル #${want.index} のプレビューが帯に重なっている`)
  }
  // タイルの下に「時刻 · チャプター名」（ラフの「15:27 · CM」）。CM の中だけ名前が付く。
  const labelBox = await page.locator('[data-testid="seek-tile-label"]').boundingBox()
  const labelText = (await page.locator('[data-testid="seek-tile-label"]').textContent())?.trim()
  const wantLabel = { 35: /^0:3[4-6] · CM$/, 95: /^1:3[4-6]$/, 105: /^1:4[4-6]$/ }[seconds]
  if (!wantLabel.test(labelText ?? '')) {
    ng.push(`② ${seconds}s 相当のタイル下ラベルが想定と違う（${labelText}）`)
  }
  if (!labelBox || labelBox.y < previewBox.y + previewBox.height - 1 || labelBox.y + labelBox.height > scrubBox.y + 1) {
    ng.push('② タイル下ラベルがタイルの下・帯の上に収まっていない')
  }
  if (EVIDENCE_DIR && seconds === 35) {
    await page.locator('[data-testid="recording-player-shell"]').screenshot({
      path: path.join(EVIDENCE_DIR, 'recording-detail-desktop.png'),
    })
  }
}

log('\n=== ③ ポインタが離れると消える ===')
await hoverTile(35)
const backOnVideo = await page
  .locator('[data-testid="seek-tile-preview"]')
  .waitFor({ timeout: 5000 })
  .then(() => true)
  .catch(() => false)
if (!backOnVideo) {
  ng.push('③ 帯の上に戻してもプレビューが出ない（①②の待ちが失敗している疑い）')
}
// 映像の上（帯の外）へ動かす。映像を見ている間にプレビューが居座らないこと。
await page.mouse.move(videoBox.x + videoBox.width * 0.3, videoBox.y + videoBox.height / 2)
await page.waitForTimeout(100)
await page.mouse.move(videoBox.x + videoBox.width * 0.4, videoBox.y + videoBox.height / 2)
await page.waitForTimeout(300)
if ((await page.locator('[data-testid="seek-tile-preview"]').count()) !== 0) {
  ng.push('③ ポインタが映像の上にあるのにプレビューが出ている')
}

log('\n=== ⑤ クリック先 = プレビューに出ていたタイル ===')
for (const seconds of [35, 95]) {
  const want = await hoverTile(seconds)
  const previewTile = page.locator('[data-testid="seek-tile-preview"] > div')
  const shown = await previewTile
    .waitFor({ timeout: 5000 })
    .then(() => previewTile.evaluate((el) => getComputedStyle(el).backgroundPosition))
    .catch(() => null)
  const targetSeconds = want.index * TILE_INTERVAL_SECONDS + 5
  const p = scrubPoint(scrubBox, duration, targetSeconds)
  const seekable = await page.waitForFunction(
    (target) => {
      const video = document.querySelector('video')
      if (!video || video.readyState < HTMLMediaElement.HAVE_METADATA) return false
      for (let index = 0; index < video.seekable.length; index += 1) {
        if (target >= video.seekable.start(index) && target <= video.seekable.end(index)) return true
      }
      return false
    },
    targetSeconds,
    { timeout: 10000 },
  ).then(() => true).catch(() => false)
  if (!seekable) {
    const state = await video.evaluate((el) => ({
      readyState: el.readyState,
      duration: el.duration,
      currentTime: el.currentTime,
      seekable: Array.from({ length: el.seekable.length }, (_, index) => [el.seekable.start(index), el.seekable.end(index)]),
    }))
    ng.push(`⑤ ${targetSeconds}s をクリックする前に metadata と seekable range が揃わない（${JSON.stringify(state)}）`)
    continue
  }
  await page.locator('[data-testid="seek-scrub"]').click({
    position: { x: p.x - scrubBox.x, y: p.y - scrubBox.y },
  })
  // WebKit may expose seeking=false for a tick before the asynchronous seek request
  // starts. Wait for the requested time itself before checking the resolved tile.
  await page.waitForFunction(
    (target) => {
      const video = document.querySelector('video')
      return video !== null && Math.abs(video.currentTime - target) < 0.5
    },
    targetSeconds,
    { timeout: 5000 },
  ).catch(() => {})
  await page.waitForFunction(() => !document.querySelector('video')?.seeking, undefined, { timeout: 5000 }).catch(() => {})
  const currentTime = await video.evaluate((v) => v.currentTime)
  const landed = expectedTile(currentTime)
  // 見えていたタイルそのものと、飛んだ先のタイルを比べる。
  if (shown !== `${landed.x}px ${landed.y}px`) {
    ng.push(`⑤ ${seconds}s で ${shown ?? 'プレビュー無し'} を見てクリックしたが ${currentTime.toFixed(1)}s（タイル #${landed.index}、期待 ${targetSeconds}s）へ飛んだ`)
  }
}

log('\n=== ⑥ 400px の設定パネルと狭い画面のプレビュー ===')
if (EVIDENCE_DIR) {
  await page.setViewportSize({ width: 400, height: 844 })
  scrubBox = await page.locator('[data-testid="seek-scrub"]').boundingBox()
  if (!scrubBox || scrubBox.width <= 0) {
    ng.push('⑥ 400px のスクラブ帯の矩形が取れない（レイアウトが想定と違う）')
  } else {
    await hoverTile(35)
    const mobilePreview = page.locator('[data-testid="seek-tile-preview"]')
    const previewShown = await mobilePreview
      .waitFor({ timeout: 5000 })
      .then(() => true)
      .catch(() => false)
    if (!previewShown) ng.push('⑥ 400px で 35s 相当のタイルプレビューが出ない')
  }
  await page.locator('[data-testid="recording-player-shell"]').screenshot({
    path: path.join(EVIDENCE_DIR, 'recording-detail-400px.png'),
  })
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.locator('[data-testid="playback-settings"]').waitFor({ timeout: 5000 })
  await page.locator('[data-testid="recording-player-shell"]').screenshot({
    path: path.join(EVIDENCE_DIR, 'recording-detail-400px-settings.png'),
  })
  await page.keyboard.press('Escape')
}
await page.setViewportSize({ width: 340, height: 800 })
await page.waitForTimeout(200)
scrubBox = await page.locator('[data-testid="seek-scrub"]').boundingBox()
if (!scrubBox || scrubBox.width >= TILE_DISPLAY_WIDTH) {
  ng.push(`⑥ 帯が狭くなっていない（width=${scrubBox?.width}）--- 判定の前提が崩れている`)
} else {
  for (const seconds of [5, 55, 115]) {
    await hoverTile(seconds)
    const preview = page.locator('[data-testid="seek-tile-preview"]')
    const box = await preview
      .waitFor({ timeout: 5000 })
      .then(() => preview.boundingBox())
      .catch(() => null)
    if (!box) {
      ng.push(`⑥ ${seconds}s の位置でプレビューが出ない`)
    } else if (box.x < scrubBox.x - 1 || box.x + box.width > scrubBox.x + scrubBox.width + 1) {
      ng.push(
        `⑥ ${seconds}s のプレビューが帯からはみ出している` +
          `（preview ${box.x.toFixed(0)}..${(box.x + box.width).toFixed(0)} / scrub ${scrubBox.x.toFixed(0)}..${(scrubBox.x + scrubBox.width).toFixed(0)}）`,
      )
    }
  }
}

log('\n=== ⑦ タッチではプレビューを出さない ===')
const touchContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  hasTouch: true,
  isMobile: true,
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const touchPage = await touchContext.newPage()
await installApiStubs(touchPage, apiHandler)
await touchPage.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
await touchPage.waitForFunction(
  () => {
    const el = document.querySelector('video')
    return Number.isFinite(el?.duration) && el.duration > 0
  },
  undefined,
  { timeout: 15000 },
)
const touchScrub = await touchPage.locator('[data-testid="seek-scrub"]').boundingBox()
for (const seconds of [35, 95, 65]) {
  const p = scrubPoint(touchScrub, duration, seconds)
  await touchPage.touchscreen.tap(p.x, p.y)
  await touchPage.waitForTimeout(300)
}
// タップが帯に届いていること（届いていなければ「出ない」は空虚に通る）。
const touchTime = await touchPage.locator('video').evaluate((v) => v.currentTime)
if (Math.abs(touchTime - 65) > 5) {
  ng.push(`⑦ タップで帯の位置へ飛んでいない（currentTime=${touchTime.toFixed(1)}、期待 65 付近）`)
}
if ((await touchPage.locator('[data-testid="seek-tile-preview"]').count()) !== 0) {
  ng.push('⑦ タップの後にプレビューが残っている')
}
await touchContext.close()

await finish(ng, browser)
