// チャプター（CM の目盛り・自動スキップ・境界の前後再生、issue #884）の実ブラウザ判定。
//
// **jsdom では原理的に測れないものだけを見る。** 目盛りの位置は帯の実寸に対する
// 割合で決まり（jsdom の `getBoundingClientRect()` は常に 0）、自動スキップと
// 前後再生は `<video>` の実再生と `currentTime` の推移でしか観測できない。
// CLAUDE.md §テスト規律のとおり、実装より先にここで判定手段を作る。
//
// 見るのは 5 点:
//   ① 目盛りの位置が区間の割合と一致し、cut と ラベル付きが別の見た目で出る
//   ② 通常の再生で cut 区間の先頭に差し掛かると、その終端へ飛ぶ
//   ③ 手動のシークで区間の中に入ったときは飛ばない（cut / ラベル付きの両方）
//   ④ 境界の「前後 3 秒」が境界の手前から始まり、境界を跨いでも飛ばされない
//      （前後再生の間は自動スキップを止める）
//   ⑤ 2 倍速でも前後 3 秒が境界の 3 秒後で止まる（実時間ではなく再生位置で止める）
//
// フィクスチャは ffmpeg で作る（再生位置の推移が判定に要る）。無い環境では
// この判定だけを skip として終了する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:chapters
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
const ng = []

// 判定対象の区間。**実装から import しない**（実装がどう変わっても同じ値で比較する
// 循環になり、何も主張しなくなる）。
//   [30, 40) 切る CM、[60, 70) 切らない OP（目盛りには出るが飛ばさない）
const CM_SPAN = { startMs: 30_000, endMs: 40_000, label: 'CM', cut: true }
const OP_SPAN = { startMs: 60_000, endMs: 70_000, label: 'OP', cut: false }
const CHAPTERS = { source: 'auto', version: 'auto:detected:1', detectionPending: false, spans: [CM_SPAN, OP_SPAN] }

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
  title: 'チャプター確認用',
  startAt: '2026-01-01T12:00:00.000Z',
  durationMs: 120_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'detected' },
  sizeBytes: 500_000_000,
  // 画質の下の階層でサイズ付きの選択肢を見るため 2 つ置く（同じ動画を配る）。
  encodedAssets: [
    { profile: 'h264', sizeBytes: 400_000_000 },
    { profile: 'h265', sizeBytes: 300_000_000 },
  ],
  createdAt: '2026-01-02T12:30:00Z',
}

/**
 * ensureFixture は判定に使う動画を一度だけ作る。長さ（duration）が要るので実在の
 * 動画を配る。**VP8/WebM を使う** --- Playwright の Chromium は H.264 を持たない
 * 構成があり、コーデックの有無で落ちると「実装が壊れている」と区別できない。
 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-chapters')
  const videoPath = path.join(fixtureDir, 'clip.webm')
  if (existsSync(videoPath) && statSync(videoPath).size > 0) return videoPath

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(fixtureDir, { recursive: true })
  log(`判定用の動画フィクスチャを生成中... (${videoPath})`)
  execFileSync(
    'ffmpeg',
    [
      '-y', '-f', 'lavfi', '-i', 'testsrc=size=160x90:rate=2',
      '-t', '120', '-c:v', 'libvpx', '-b:v', '30k', '-pix_fmt', 'yuv420p',
      videoPath,
    ],
    { stdio: 'ignore' },
  )
  return existsSync(videoPath) ? videoPath : undefined
}

/** rangeResponse は Range に応じる（応じないと Chromium は seekable にしない）。 */
function rangeResponse(route, bytes, contentType) {
  const range = /bytes=(\d+)-(\d*)/.exec(route.request().headers().range ?? '')
  if (!range) {
    return route.fulfill({ status: 200, contentType, body: bytes, headers: { 'Accept-Ranges': 'bytes' } })
  }
  const start = Number(range[1])
  const end = range[2] ? Number(range[2]) : bytes.length - 1
  return route.fulfill({
    status: 206,
    contentType,
    body: bytes.subarray(start, end + 1),
    headers: { 'Accept-Ranges': 'bytes', 'Content-Range': `bytes ${start}-${end}/${bytes.length}` },
  })
}

let videoBytes = Buffer.alloc(0)

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
  if (/^\/api\/recordings\/1\/chapters$/.test(apiPath) && method === 'GET') return json(CHAPTERS)
  if (/^\/api\/media\/recordings\/1\/file$/.test(apiPath)) {
    return rangeResponse(route, videoBytes, 'video/webm')
  }
  if (/^\/api\/media\/recordings\/\d+\/(thumbnail|seek-tiles)$/.test(apiPath)) {
    return route.fulfill({ status: 404 })
  }
  return json([])
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const videoPath = ensureFixture()
if (videoPath === undefined) {
  log('  ffmpeg が無いため、チャプターの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}
videoBytes = readFileSync(videoPath)

// 音声トラックの無いフィクスチャでも Chromium の自動再生ポリシーは掛かる。判定は
// 「実際に再生が進むこと」に依存するので、ジェスチャ無しの再生を許す。
const browser = await launchBrowser('chromium', {
  args: ['--autoplay-policy=no-user-gesture-required'],
})
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const page = await context.newPage()
await installApiStubs(page, apiHandler)

const video = page.locator('video')
await page.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
await video.waitFor({ timeout: 15000 })
await page.waitForFunction(
  () => Number.isFinite(document.querySelector('video')?.duration) && document.querySelector('video').duration > 0,
  undefined,
  { timeout: 15000 },
)
const duration = await video.evaluate((v) => v.duration)
if (Math.abs(duration - 120) > 5) {
  ng.push(`フィクスチャの長さが想定と違う（duration=${duration}）--- 判定の前提が崩れている`)
}
// 再生位置は localStorage に残る。前回の実行の続きから始まると判定が揺れるので消す。
await page.evaluate(() => localStorage.clear())

/** seek は再生位置を動かし、seek の完了を待つ。 */
async function seek(seconds) {
  await video.evaluate((v, s) => {
    v.currentTime = s
  }, seconds)
  await page
    .waitForFunction(() => !document.querySelector('video')?.seeking, undefined, { timeout: 5000 })
    .catch(() => {})
  // seeked の後に実装が直前位置を更新するのを待つ（timeupdate を 1 回跨がせる）。
  await page.waitForTimeout(300)
}

/** playFor は ms ミリ秒ぶん再生して、その間の再生位置の推移を返す。 */
async function playFor(ms) {
  await video.evaluate((v) => {
    v.muted = true
    return v.play()
  })
  await page.waitForTimeout(ms)
  const current = await video.evaluate((v) => v.currentTime)
  await video.evaluate((v) => v.pause())
  return current
}

log('\n=== ① 目盛りの位置 ===')
const scrub = page.locator('[data-testid="seek-scrub"]')
const scrubBox = await scrub.boundingBox()
const markers = page.locator('[data-testid="chapter-marker"]')
const markerCount = await markers.count()
if (markerCount !== CHAPTERS.spans.length) {
  ng.push(`① 目盛りの数が ${markerCount}（期待 ${CHAPTERS.spans.length}）`)
} else if (!scrubBox || scrubBox.width <= 0) {
  ng.push('① 帯の矩形が取れない（レイアウトが想定と違う）')
} else {
  for (const [index, span] of CHAPTERS.spans.entries()) {
    const box = await markers.nth(index).boundingBox()
    const wantX = scrubBox.x + scrubBox.width * (span.startMs / 1000 / duration)
    const wantWidth = scrubBox.width * ((span.endMs - span.startMs) / 1000 / duration)
    if (!box) {
      ng.push(`① 目盛り #${index} の実寸が取れない`)
      continue
    }
    // 端の丸め（rounded-full）で 1px 程度ずれるので許容する。
    if (Math.abs(box.x - wantX) > 2 || Math.abs(box.width - wantWidth) > 2) {
      ng.push(
        `① 目盛り #${index} の位置が違う（実際 x=${box.x.toFixed(1)} w=${box.width.toFixed(1)}` +
          ` / 期待 x=${wantX.toFixed(1)} w=${wantWidth.toFixed(1)}）`,
      )
    }
    const cut = await markers.nth(index).getAttribute('data-cut')
    if (cut !== String(span.cut)) {
      ng.push(`① 目盛り #${index} の data-cut が ${cut}（期待 ${span.cut}）`)
    }
  }
}

log('\n=== ①-c 目盛りはトラック上にあり、再生済みの塗りと thumb は映像上で読める固定色 ===')
for (const scheme of ['light', 'dark']) {
  await page.emulateMedia({ colorScheme: scheme })
  const geometry = await page.evaluate(() => {
    const track = document.querySelector('[data-testid="seek-scrub"] .bg-white\\/30')
    const fill = track?.firstElementChild
    const thumb = document.querySelector('[data-testid="seek-thumb"]')
    const marker = document.querySelector('[data-testid="chapter-marker"]')
    if (!track || !fill || !thumb || !marker) return null
    const t = track.getBoundingClientRect()
    const m = marker.getBoundingClientRect()
    const th = thumb.getBoundingClientRect()
    return {
      fill: getComputedStyle(fill).backgroundColor,
      markerCoversTrack: m.top <= t.top + 0.5 && m.bottom >= t.bottom - 0.5,
      thumbCenterOnTrack: Math.abs(th.top + th.height / 2 - (t.top + t.height / 2)) < 1,
      volume: getComputedStyle(document.querySelector('input[aria-label="音量"]')).accentColor,
    }
  })
  if (geometry === null) {
    ng.push(`①-c (${scheme}) トラック・thumb・目盛りのいずれかが見つからない`)
    continue
  }
  if (geometry.fill !== 'rgb(255, 255, 255)') ng.push(`①-c (${scheme}) 再生済みの塗りが白でない（${geometry.fill}）`)
  if (!geometry.markerCoversTrack) ng.push(`①-c (${scheme}) 目盛りがトラック上に重なっていない`)
  if (!geometry.thumbCenterOnTrack) ng.push(`①-c (${scheme}) thumb の中心がトラック上にない`)
  if (geometry.volume !== 'rgb(255, 255, 255)') ng.push(`①-c (${scheme}) 音量スライダーが白でない（${geometry.volume}）`)
}
await page.emulateMedia({ colorScheme: null })

log('\n=== ①-a native controls が無く、単一バーとチャプターナビが同じプレイヤーにある ===')
if ((await video.evaluate((node) => node.hasAttribute('controls'))) !== false) {
  ng.push('①-a encoded video に native controls が残っている')
}
if ((await page.locator('[data-testid="seek-scrub"]').count()) !== 1) {
  ng.push('①-a encoded player の seekbar が 1 本ではない')
}
if ((await page.locator('[data-testid="player-controls"] [data-testid="chapter-navigation"]').count()) !== 1) {
  ng.push('①-a 前後チャプターの操作が player toolbar にない')
}

log('\n=== ①-b 全画面要素に操作バーとシークバーが含まれる ===')
try {
  await page.getByRole('button', { name: '全画面表示' }).click()
  await page.waitForFunction(() => document.fullscreenElement !== null, undefined, { timeout: 5000 })
  const fullscreenContainsPlayerControls = await page.evaluate(() => {
    const fullscreen = document.fullscreenElement
    return Boolean(
      fullscreen?.matches('[data-testid="recording-player-frame"]') &&
      fullscreen.querySelector('[data-testid="player-controls"] [data-testid="seek-scrub"]'),
    )
  })
  if (!fullscreenContainsPlayerControls) {
    ng.push('①-b fullscreenElement が player frame ではないか、操作バー/seekbar を含まない')
  }
  // 全画面でも設定メニューが全画面要素の中に描かれ、画面内に見える。
  await page.getByRole('button', { name: '再生設定' }).click()
  const settingsInFullscreen = await page.evaluate(() => {
    const fullscreen = document.fullscreenElement
    const settings = document.querySelector('[data-testid="playback-settings"]')
    if (!fullscreen || !settings) return { exists: Boolean(settings), inside: false, visible: false }
    const r = settings.getBoundingClientRect()
    return {
      exists: true,
      inside: fullscreen.contains(settings),
      visible: r.width > 0 && r.height > 0 && r.bottom <= window.innerHeight && r.right <= window.innerWidth,
    }
  })
  if (!settingsInFullscreen.inside || !settingsInFullscreen.visible) {
    ng.push(`①-b 全画面で設定メニューが全画面要素の中に見えない（${JSON.stringify(settingsInFullscreen)}）`)
  }
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.getByRole('button', { name: '全画面を終了' }).click()
  await page.waitForFunction(() => document.fullscreenElement === null, undefined, { timeout: 5000 })
} catch (error) {
  ng.push(`①-b 全画面遷移の実ブラウザ確認に失敗: ${String(error)}`)
  await page.keyboard.press('Escape').catch(() => {})
}

log('\n=== ② 通常の再生で cut 区間の先頭に差し掛かると終端へ飛ぶ ===')
// 区間の手前 2 秒から再生する。飛ばなければ 4 秒後は 32 秒付近にとどまる。
await seek(28)
const afterPlayback = await playFor(4000)
if (afterPlayback < 39) {
  ng.push(`② Cut 区間に入っても飛ばない（4 秒後の位置 ${afterPlayback.toFixed(1)} 秒。期待 40 秒以降）`)
}

log('\n=== ③ 手動のシークで区間の中に入ったときは飛ばない ===')
// cut 区間の中（35 秒）へシークして再生する。飛ばすなら 4 秒後は 40 秒以降になる。
await seek(35)
const insideCut = await playFor(3000)
if (insideCut >= 39.5) {
  ng.push(`③ シークで入った cut 区間から追い出された（位置 ${insideCut.toFixed(1)} 秒）`)
} else if (insideCut < 36) {
  ng.push(`③ シーク後に再生が進んでいない（位置 ${insideCut.toFixed(1)} 秒）--- 判定が空虚に通っている`)
}
// **再生したまま**（pause しない）区間の手前から中へシークしても追い出されない。
// シークの処理順は seeking → timeupdate → seeked なので、seeked で直前位置を更新する
// 実装だと、最初の timeupdate がシーク前の位置（25 秒付近）のまま「30 秒を跨いだ」と
// 判定して 40 秒へ飛ばす。
await video.evaluate((v) => {
  v.muted = true
  return v.play()
})
await video.evaluate((v) => {
  v.currentTime = 25
})
await page.waitForTimeout(1500)
const beforeLiveSeek = await video.evaluate((v) => v.currentTime)
await video.evaluate((v) => {
  v.currentTime = 35
})
await page.waitForTimeout(3000)
const afterLiveSeek = await video.evaluate((v) => ({ t: v.currentTime, paused: v.paused }))
await video.evaluate((v) => v.pause())
if (beforeLiveSeek < 25 || beforeLiveSeek >= 30) {
  ng.push(`③ 再生中シークの前提が崩れている（シーク前の位置 ${beforeLiveSeek.toFixed(1)} 秒。期待 25〜30 秒）`)
} else if (afterLiveSeek.t >= 40) {
  ng.push(`③ 再生したままのシークで cut 区間から追い出された（3 秒後の位置 ${afterLiveSeek.t.toFixed(1)} 秒。期待 40 秒未満）`)
} else if (afterLiveSeek.t < 36 || afterLiveSeek.paused) {
  ng.push(`③ 再生したままのシーク後に再生が進んでいない（位置 ${afterLiveSeek.t.toFixed(1)} 秒）--- 判定が空虚に通っている`)
}
// ラベル付き（cut でない）区間は、通常の再生で入っても飛ばない。
await seek(58)
const throughOp = await playFor(4000)
if (throughOp < 61 || throughOp >= 70) {
  ng.push(`③ Cut でない区間（OP）で再生位置が飛んだ（4 秒後の位置 ${throughOp.toFixed(1)} 秒。期待 62 秒付近）`)
}

log('\n=== ④ 境界の「前後 3 秒」 ===')
// チャプター編集は閉じた <details> に入っているので、境界行を見る前に開く。
await page.locator('[data-testid="chapter-editor-details"] > summary').click()
const firstBoundary = page.locator('[data-testid="chapter-boundary"]').first()
const label = await firstBoundary.textContent()
if (!label?.includes('0:00:30')) {
  ng.push(`④ 先頭の境界が 30 秒ではない（${label?.trim()}）--- 判定の前提が崩れている`)
}
await seek(100)
await firstBoundary.getByRole('button', { name: '前後3秒' }).click()
// まず開始位置を見る。境界の 3 秒手前（27 秒）から始まり、1 秒後に 28 秒付近に居る。
// 境界そのもの（30 秒）から始めていれば 31 秒付近になるので、ここで区別できる。
const afterOneSecond = await playFor(1000)
if (afterOneSecond < 27 || afterOneSecond > 29.5) {
  ng.push(
    `④ 前後 3 秒が 30 秒の手前 3 秒から始まっていない（1 秒後の位置 ${afterOneSecond.toFixed(2)} 秒。期待 27〜29.5 秒）`,
  )
}
// そのまま境界を跨ぐ。**前後再生の間は自動スキップを止める**ので、跨いでも
// 40 秒へ飛ばされない（3.5 秒後に 31 秒付近）。
const afterBoundary = await playFor(3500)
if (afterBoundary >= 35) {
  ng.push(`④ 前後 3 秒の再生中に境界で自動スキップが働いた（位置 ${afterBoundary.toFixed(2)} 秒）`)
} else if (afterBoundary < 29) {
  ng.push(`④ 前後 3 秒の再生が進んでいない（位置 ${afterBoundary.toFixed(2)} 秒）--- 判定が空虚に通っている`)
}

log('\n=== ⑤ 前後 3 秒は再生速度に追従して境界の 3 秒後で止まる ===')
// 2 倍速では境界の 3 秒後（33 秒）まで実時間 3 秒で届く。実時間 6 秒のタイマーで
// 止める実装だと 4.5 秒後もまだ再生中で 36 秒付近まで進む。
await video.evaluate((v) => {
  v.playbackRate = 2
})
await seek(100)
await firstBoundary.getByRole('button', { name: '前後3秒' }).click()
await page.waitForTimeout(4500)
const fast = await video.evaluate((v) => ({ t: v.currentTime, paused: v.paused }))
await video.evaluate((v) => {
  v.pause()
  v.playbackRate = 1
})
if (!fast.paused || fast.t < 32 || fast.t > 34.5) {
  ng.push(
    `⑤ 2 倍速の前後 3 秒が境界の 3 秒後（33 秒）で止まっていない（位置 ${fast.t.toFixed(2)} 秒 paused=${fast.paused}）`,
  )
}

log('\n=== ⑥ 再生中のバー: マウスで押した後は隠れ、キーボードの Tab で届く ===')
const controls = page.locator('[data-testid="player-controls"]')
const controlsOpacity = () => controls.evaluate((el) => getComputedStyle(el).opacity)
await seek(80)
// 6-a: バーの ▶ をマウスで押す。フォーカスが残っても 3 秒後にバーは隠れる。
await page.getByRole('button', { name: '再生', exact: true }).click()
await page.mouse.move(5, 5)
await page.waitForTimeout(4500)
const hiddenAfterClick = await controlsOpacity()
await video.evaluate((v) => v.pause())
if (hiddenAfterClick !== '0') {
  ng.push(`⑥-a バーのボタンをマウスで押して再生した後もバーが隠れない（opacity=${hiddenAfterClick}）`)
}
// 6-b: 映像クリックで再生 → バーが隠れた後に Tab でバーへ届く（隠れたバーは inert）。
await page.locator('video').click()
await page.mouse.move(5, 5)
await page.waitForTimeout(4500)
const hiddenBeforeTab = await controlsOpacity()
await page.keyboard.press('Tab')
await page.waitForTimeout(400)
const afterTab = await page.evaluate(() => ({
  inBar: Boolean(document.activeElement?.closest('[data-testid="player-controls"]')),
  active: document.activeElement?.getAttribute('aria-label') ?? document.activeElement?.tagName,
}))
const shownAfterTab = await controlsOpacity()
await video.evaluate((v) => v.pause())
if (hiddenBeforeTab !== '0') ng.push(`⑥-b 前提: 映像クリックで再生した後にバーが隠れていない（opacity=${hiddenBeforeTab}）`)
if (!afterTab.inBar || shownAfterTab !== '1') {
  ng.push(`⑥-b 再生中に Tab を押してもバーに届かない（focus=${afterTab.active} opacity=${shownAfterTab}）`)
}

log('\n=== ⑦ デスクトップの設定メニュー: 行リストを歯車の真上に開き、› で中身を差し替える ===')
await page.mouse.move(640, 300)
const gear = page.getByRole('button', { name: '再生設定' })
const settingsMenu = page.locator('[data-testid="playback-settings"]')
/** menuShape は設定メニューの形（行の役割・フォーム部品の有無・歯車との位置）を返す。 */
const menuShape = () =>
  page.evaluate(() => {
    const menu = document.querySelector('[data-testid="playback-settings"]')
    const gearButton = document.querySelector('button[aria-label="再生設定"]')
    const frame = document.querySelector('[data-testid="recording-player-frame"]')
    if (!menu || !gearButton || !frame) return null
    const m = menu.getBoundingClientRect()
    const g = gearButton.getBoundingClientRect()
    const f = frame.getBoundingClientRect()
    const items = Array.from(menu.querySelectorAll('[role^="menuitem"]')).filter((el) => el.getClientRects().length > 0)
    return {
      role: menu.getAttribute('role'),
      formControls: menu.querySelectorAll('select, input, a[download]').length,
      text: menu.textContent ?? '',
      items: items.map((el) => ({
        role: el.getAttribute('role'),
        name: el.getAttribute('aria-label') ?? el.textContent?.trim(),
        checked: el.getAttribute('aria-checked'),
      })),
      background: getComputedStyle(menu).backgroundColor,
      aboveGear: m.bottom <= g.top + 1 && m.left <= g.left + g.width / 2 && m.right >= g.left + g.width / 2,
      insideFrame: frame.contains(menu) && m.top >= f.top - 1 && m.bottom <= f.bottom + 1,
    }
  })
await gear.click()
await settingsMenu.waitFor({ timeout: 5000 })
const desktopMenu = await menuShape()
if (desktopMenu === null) {
  ng.push('⑦ 設定メニューが開かない')
} else {
  if (desktopMenu.role !== 'menu') ng.push(`⑦ 設定メニューが role="menu" でない（${desktopMenu.role}）`)
  if (desktopMenu.formControls !== 0) {
    ng.push(`⑦ 設定メニューにプルダウン・チェックボックス・ダウンロード等のフォーム部品が ${desktopMenu.formControls} 個ある`)
  }
  const names = desktopMenu.items.map((item) => `${item.role}:${item.name}`)
  const wantRows = ['menuitemcheckbox:CM を飛ばす', 'menuitemcheckbox:字幕', 'menuitem:再生速度', 'menuitem:画質']
  if (JSON.stringify(names) !== JSON.stringify(wantRows)) {
    ng.push(`⑦ 行リストが「CM を飛ばす / 字幕 / 再生速度 / 画質」でない（${JSON.stringify(names)}）`)
  }
  if (!/標準/.test(desktopMenu.text) || !/h264/.test(desktopMenu.text)) {
    ng.push(`⑦ 行に現在値（標準 / h264）が出ていない（${desktopMenu.text}）`)
  }
  if (/ダウンロード/.test(desktopMenu.text)) ng.push('⑦ 設定メニューにダウンロードが残っている')
  const alpha = /rgba\(\s*(\d+),\s*(\d+),\s*(\d+),\s*([\d.]+)\)/.exec(desktopMenu.background)
  if (!alpha || Number(alpha[4]) >= 1 || Number(alpha[1]) > 60) {
    ng.push(`⑦ メニューが半透明の黒い小窓でない（background=${desktopMenu.background}）`)
  }
  if (!desktopMenu.aboveGear) ng.push('⑦ メニューが歯車の真上に開いていない')
  if (!desktopMenu.insideFrame) ng.push('⑦ メニューが映像の枠の中に収まっていない')
}
// › で下の階層へ: 同じ枠の中身が「‹ 画質」と選択肢（✓・サイズ）に替わる。
await settingsMenu.evaluate((node) => {
  node.dataset.e2eIdentity = 'settings-before-submenu'
}).catch(() => {})
await page.getByRole('menuitem', { name: '画質' }).click({ timeout: 3000 }).catch(() => {})
const qualityMenu = await page.evaluate(() => {
  const menu = document.querySelector('[data-testid="playback-settings"]')
  if (!menu) return null
  const options = Array.from(menu.querySelectorAll('[role="menuitemradio"]'))
  return {
    sameElement: menu.dataset.e2eIdentity === 'settings-before-submenu',
    back: Array.from(menu.querySelectorAll('[role="menuitem"]')).some((el) =>
      (el.getAttribute('aria-label') ?? el.textContent ?? '').includes('画質'),
    ),
    mainRowsGone: !Array.from(menu.querySelectorAll('[role^="menuitem"]')).some((el) =>
      (el.textContent ?? '').includes('再生速度'),
    ),
    options: options.map((el) => ({ text: el.textContent?.trim() ?? '', checked: el.getAttribute('aria-checked') })),
  }
})
if (qualityMenu === null) {
  ng.push('⑦ 画質を押したらメニューが閉じた')
} else {
  if (!qualityMenu.sameElement) ng.push('⑦ 画質の下の階層が同じ枠の中身の差し替えでない（別の要素になった）')
  if (!qualityMenu.back || !qualityMenu.mainRowsGone) {
    ng.push(`⑦ 画質の下の階層が「‹ 画質」+ 選択肢に差し替わっていない（back=${qualityMenu.back} mainRowsGone=${qualityMenu.mainRowsGone}）`)
  }
  const sized = qualityMenu.options.filter((option) => /\d+(\.\d+)?\s?(B|KB|MB|GB)/.test(option.text))
  if (qualityMenu.options.length !== 2 || sized.length !== 2) {
    ng.push(`⑦ 画質の選択肢 2 つにサイズが付いていない（${JSON.stringify(qualityMenu.options)}）`)
  }
  if (qualityMenu.options.filter((option) => option.checked === 'true').length !== 1) {
    ng.push(`⑦ 画質の選択中が 1 つだけ ✓（aria-checked）になっていない（${JSON.stringify(qualityMenu.options)}）`)
  }
}
// Esc で 1 段戻り、矢印キーで行を移り、もう一度 Esc で閉じて歯車にフォーカスが戻る。
await page.keyboard.press('Escape')
const afterFirstEscape = await page.evaluate(() => ({
  open: Boolean(document.querySelector('[data-testid="playback-settings"]')),
  speedRow: Array.from(document.querySelectorAll('[data-testid="playback-settings"] [role="menuitem"]')).some((el) =>
    (el.textContent ?? '').includes('再生速度'),
  ),
}))
if (!afterFirstEscape.open || !afterFirstEscape.speedRow) {
  ng.push(`⑦ 下の階層で Esc を押しても元の行リストに戻らない（${JSON.stringify(afterFirstEscape)}）`)
}
await page.keyboard.press('ArrowUp')
const afterArrow = await page.evaluate(() => document.activeElement?.getAttribute('role') ?? document.activeElement?.tagName)
if (!String(afterArrow).startsWith('menuitem')) ng.push(`⑦ 矢印キーで行を移れない（focus=${afterArrow}）`)
await page.keyboard.press('Escape')
const afterSecondEscape = await page.evaluate(() => ({
  open: Boolean(document.querySelector('[data-testid="playback-settings"]')),
  focus: document.activeElement?.getAttribute('aria-label'),
}))
if (afterSecondEscape.open || afterSecondEscape.focus !== '再生設定') {
  ng.push(`⑦ Esc でメニューが閉じて歯車にフォーカスが戻らない（${JSON.stringify(afterSecondEscape)}）`)
}
if (afterSecondEscape.open) await gear.click()
// バーの CC はメニューの字幕スイッチと同じ状態を持つ。
const ccButton = page.locator('[data-testid="player-controls"] button[aria-label="字幕"]')
if ((await ccButton.count()) !== 1) {
  ng.push('⑦ バーに字幕（CC）ボタンが無い')
} else {
  await ccButton.click()
  const ccPressed = await ccButton.getAttribute('aria-pressed')
  await gear.click()
  const subtitleSwitch = await page
    .getByRole('menuitemcheckbox', { name: '字幕' })
    .getAttribute('aria-checked', { timeout: 3000 })
    .catch(() => null)
  if (ccPressed !== 'true' || subtitleSwitch !== 'true') {
    ng.push(`⑦ CC を押してもバーとメニューの字幕が揃ってオンにならない（cc=${ccPressed} menu=${subtitleSwitch}）`)
  }
  await page.keyboard.press('Escape')
  await ccButton.click()
}
// 時刻の横にいまのチャプター名。
await seek(65)
const chapterName = await page
  .locator('[data-testid="playback-chapter"]')
  .textContent({ timeout: 3000 })
  .catch(() => null)
if (!chapterName?.includes('OP')) ng.push(`⑦ 時刻の横にいまのチャプター名（OP）が出ない（${chapterName}）`)
// 全画面でも歯車の真上に、全画面要素の中で開く。
try {
  await page.getByRole('button', { name: '全画面表示' }).click()
  await page.waitForFunction(() => document.fullscreenElement !== null, undefined, { timeout: 5000 })
  await gear.click()
  const fullscreenMenu = await menuShape()
  if (!fullscreenMenu?.aboveGear || !fullscreenMenu.insideFrame || fullscreenMenu.formControls !== 0) {
    ng.push(`⑦ 全画面で設定メニューが歯車の真上の行リストにならない（${JSON.stringify(fullscreenMenu)}）`)
  }
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: '全画面を終了' }).click()
  await page.waitForFunction(() => document.fullscreenElement === null, undefined, { timeout: 5000 })
} catch (error) {
  ng.push(`⑦ 全画面の設定メニューを確かめられない: ${String(error)}`)
  await page.keyboard.press('Escape').catch(() => {})
}

log('\n=== ⑧ スマホ: 中央に前後チャプター、右上に CC と歯車、歯車は画面下からのシート ===')
const phoneContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  hasTouch: true,
  isMobile: true,
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
const phone = await phoneContext.newPage()
await installApiStubs(phone, apiHandler)
await phone.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
await phone.waitForFunction(
  () => Number.isFinite(document.querySelector('video')?.duration) && document.querySelector('video').duration > 0,
  undefined,
  { timeout: 15000 },
)
const phoneLayout = await phone.evaluate(() => {
  const frame = document.querySelector('[data-testid="recording-player-frame"]')?.getBoundingClientRect()
  const visible = (selector) =>
    Array.from(document.querySelectorAll(selector)).filter((el) => {
      const r = el.getBoundingClientRect()
      return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden'
    })
  const box = (selector) => {
    const r = visible(selector)[0]?.getBoundingClientRect()
    return r ? { top: r.top, left: r.left, width: r.width, height: r.height } : null
  }
  return {
    frame: frame ? { top: frame.top, bottom: frame.bottom } : null,
    prev: box('button[aria-label="前のチャプター"]'),
    play: box('[data-testid="player-controls"] button[aria-label="再生"]'),
    next: box('button[aria-label="次のチャプター"]'),
    cc: box('[data-testid="player-controls"] button[aria-label="字幕"]'),
    gear: box('button[aria-label="再生設定"]'),
    volume: visible('input[aria-label="音量"], button[aria-label="ミュート"], button[aria-label="ミュート解除"]').length,
  }
})
const middle = (r) => r.top + r.height / 2
if (!phoneLayout.frame || !phoneLayout.prev || !phoneLayout.play || !phoneLayout.next) {
  ng.push(`⑧ スマホに前後チャプター・再生が見えない（${JSON.stringify(phoneLayout)}）`)
} else {
  const f = phoneLayout.frame
  const third = (f.bottom - f.top) / 3
  const centered = [phoneLayout.prev, phoneLayout.play, phoneLayout.next].every(
    (r) => middle(r) > f.top + third && middle(r) < f.bottom - third,
  )
  if (!centered || !(phoneLayout.prev.left < phoneLayout.play.left && phoneLayout.play.left < phoneLayout.next.left)) {
    ng.push(`⑧ 前のチャプター / 再生 / 次のチャプターが映像の中央に並んでいない（${JSON.stringify(phoneLayout)}）`)
  }
  if (!phoneLayout.cc || !phoneLayout.gear || middle(phoneLayout.gear) > f.top + third || middle(phoneLayout.cc) > f.top + third) {
    ng.push(`⑧ CC と歯車が映像の右上にない（cc=${JSON.stringify(phoneLayout.cc)} gear=${JSON.stringify(phoneLayout.gear)}）`)
  }
}
if (phoneLayout.volume !== 0) ng.push(`⑧ スマホにミュート / 音量が ${phoneLayout.volume} 個見えている`)
// 中央の次のチャプター / 前のチャプターが実際に動く（0 秒 → CM の先頭 30 秒 → 先頭）。
const tapVisible = async (selector) => {
  const box = await phone.evaluate((sel) => {
    const el = Array.from(document.querySelectorAll(sel)).find((node) => node.getBoundingClientRect().width > 0)
    const r = el?.getBoundingClientRect()
    return r ? { x: r.left + r.width / 2, y: r.top + r.height / 2 } : null
  }, selector)
  if (box) await phone.touchscreen.tap(box.x, box.y)
  await phone.waitForTimeout(400)
}
await tapVisible('button[aria-label="次のチャプター"]')
const phoneJump = await phone.locator('video').evaluate((v) => v.currentTime)
if (Math.abs(phoneJump - 30) > 1) ng.push(`⑧ 中央の次のチャプターで 30 秒へ飛ばない（currentTime=${phoneJump.toFixed(1)}）`)
await tapVisible('button[aria-label="前のチャプター"]')
const phoneBack = await phone.locator('video').evaluate((v) => v.currentTime)
if (phoneBack > 1) ng.push(`⑧ 中央の前のチャプターで先頭へ戻らない（currentTime=${phoneBack.toFixed(1)}）`)
// 歯車 → 画面の下からモーダルのシート。背後を暗くし、シートは下半分に収まる。
await tapVisible('button[aria-label="再生設定"]')
const sheet = await phone.evaluate(() => {
  const menu = document.querySelector('[data-testid="playback-settings"]')
  if (!menu) return null
  const scrim = document.querySelector('[data-testid="playback-settings-scrim"]')
  const r = menu.getBoundingClientRect()
  const s = scrim?.getBoundingClientRect()
  const items = Array.from(menu.querySelectorAll('[role^="menuitem"]')).filter((el) => el.getClientRects().length > 0)
  return {
    top: r.top,
    bottom: r.bottom,
    left: r.left,
    right: r.right,
    innerHeight: window.innerHeight,
    innerWidth: window.innerWidth,
    formControls: menu.querySelectorAll('select, input, a[download]').length,
    volume: menu.querySelectorAll('[aria-label="音量"], [aria-label="ミュート"], [aria-label="ミュート解除"]').length,
    items: items
      .slice()
      .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
      .map((el) => (el.getAttribute('aria-label') ?? el.textContent?.trim() ?? '').slice(0, 20)),
    scrimCovers: Boolean(s && s.top <= 0 && s.bottom >= window.innerHeight && s.width >= window.innerWidth),
    scrimBackground: scrim ? getComputedStyle(scrim).backgroundColor : null,
  }
})
if (sheet === null) {
  ng.push('⑧ 歯車を押しても設定が開かない')
} else {
  if (Math.abs(sheet.bottom - sheet.innerHeight) > 2 || sheet.left > 1 || sheet.right < sheet.innerWidth - 1) {
    ng.push(`⑧ 設定が画面の下端に幅いっぱいのシートで開かない（${JSON.stringify(sheet)}）`)
  }
  if (sheet.top < sheet.innerHeight / 2 - 2) {
    ng.push(`⑧ シートが画面の下半分に収まっていない（top=${sheet.top} / ${sheet.innerHeight}）`)
  }
  if (!sheet.scrimCovers || !/rgba\(0, 0, 0, 0\.[1-9]/.test(sheet.scrimBackground ?? '')) {
    ng.push(`⑧ シートの背後が暗くならない（scrim=${sheet.scrimBackground} covers=${sheet.scrimCovers}）`)
  }
  if (sheet.formControls !== 0) ng.push(`⑧ シートにプルダウン・チェックボックス等が ${sheet.formControls} 個ある`)
  if (sheet.volume !== 0) ng.push('⑧ シートにミュート / 音量がある')
  if (!/画質/.test(sheet.items[0] ?? '') || !sheet.items.some((name) => /ピクチャー/.test(name))) {
    ng.push(`⑧ シートの行が画質から始まり PiP を含む行リストでない（${JSON.stringify(sheet.items)}）`)
  }
  await tapVisible('[data-testid="playback-settings"] [role="menuitem"][aria-label="画質"]')
  const phoneQuality = await phone.evaluate(() =>
    Array.from(document.querySelectorAll('[data-testid="playback-settings"] [role="menuitemradio"]')).map(
      (el) => el.textContent?.trim() ?? '',
    ),
  )
  if (phoneQuality.length !== 2 || !phoneQuality.every((text) => /\d+(\.\d+)?\s?(B|KB|MB|GB)/.test(text))) {
    ng.push(`⑧ シートの画質の下の階層にサイズ付きの選択肢が出ない（${JSON.stringify(phoneQuality)}）`)
  }
}
await phoneContext.close()

await finish(ng, browser)
