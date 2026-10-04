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
//   ⑥ 再生中のバー: マウスで押した後は隠れ、キーボードの Tab で届く
//   ⑦ デスクトップの設定メニューが歯車の真上の行リストで、「›」で中身を差し替える
//   ⑧ スマホの操作表示（中央の前後チャプター・右上の CC と歯車）と画面下からのシート
//
// フィクスチャは ffmpeg で作る（再生位置の推移が判定に要る）。無い環境では
// この判定だけを skip として終了する。
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 corepack pnpm e2e:chapters
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
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
let currentChapters = CHAPTERS
const chapterEditBodies = []
const seekTileRequests = []
let tileResponseStatus = 200
let chapterResetCount = 0

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

/**
 * tileColor は秒数 `seconds` のタイルの単色（R, G, B）。**時刻ごとに違う色**にして、画素を読めば
 * 「どの時刻のタイルがそこに出ているか」が分かるようにする（色が同じ・似た絵だと、タイルが 1 枚
 * ずれても位置の判定が通ってしまう）。実装から import しない（循環になる）。
 */
function tileColor(seconds) {
  const index = Math.floor(seconds / 10)
  return [20 + index * 18, 240 - index * 18, 100]
}

/** QUAD_BLUE は 1 枚のタイルの 4 象限（左上・右上・左下・右下）の青。象限ごとに違う値にして、切り抜きのずれを読めるようにする。 */
const QUAD_BLUE = [40, 120, 200, 255]

/**
 * ensureSeekTilesFixture は 10 秒ごとの 10 列格子（1600x180、12 枚 + 黒）を作る。1 枚は 4 象限で、
 * 赤・緑が時刻（タイルの番号）、青が象限を表す。タイルの左上だけを写す切り抜きは、右・下の象限の青が違うので分かる。
 */
function ensureSeekTilesFixture(videoPath) {
  const fixturePath = path.join(path.dirname(videoPath), 'seek-tiles-quads.jpg')
  if (existsSync(fixturePath) && statSync(fixturePath).size > 0) return fixturePath
  const width = 1600
  const height = 180
  const pixels = Buffer.alloc(width * height * 3)
  for (let index = 0; index < 12; index += 1) {
    const [r, g] = tileColor(index * 10)
    const x0 = (index % 10) * 160
    const y0 = Math.floor(index / 10) * 90
    for (let y = 0; y < 90; y += 1) {
      for (let x = 0; x < 160; x += 1) {
        const offset = ((y0 + y) * width + x0 + x) * 3
        pixels[offset] = r
        pixels[offset + 1] = g
        pixels[offset + 2] = QUAD_BLUE[(y >= 45 ? 2 : 0) + (x >= 80 ? 1 : 0)]
      }
    }
  }
  const ppmPath = path.join(path.dirname(videoPath), 'seek-tiles-quads.ppm')
  writeFileSync(ppmPath, Buffer.concat([Buffer.from(`P6\n${width} ${height}\n255\n`), pixels]))
  execFileSync('ffmpeg', ['-y', '-i', ppmPath, '-q:v', '2', fixturePath], { stdio: 'ignore' })
  return existsSync(fixturePath) ? fixturePath : undefined
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
let tileBytes = Buffer.alloc(0)

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
  if (/^\/api\/recordings\/1\/chapters$/.test(apiPath) && method === 'GET') return json(currentChapters)
  if (/^\/api\/recordings\/1\/chapter-edits$/.test(apiPath) && method === 'PUT') {
    const body = route.request().postDataJSON()
    chapterEditBodies.push(body)
    currentChapters = { ...currentChapters, source: 'user', version: 'user:edited:1', spans: body.spans }
    return route.fulfill({ status: 204 })
  }
  if (/^\/api\/recordings\/1\/chapter-edits$/.test(apiPath) && method === 'DELETE') {
    chapterResetCount += 1
    currentChapters = { ...CHAPTERS, version: `auto:detected:${chapterResetCount + 1}` }
    return route.fulfill({ status: 204 })
  }
  if (/^\/api\/media\/recordings\/1\/file$/.test(apiPath)) {
    return rangeResponse(route, videoBytes, 'video/webm')
  }
  if (/^\/api\/media\/recordings\/\d+\/seek-tiles$/.test(apiPath)) {
    seekTileRequests.push(apiPath)
    return route.fulfill({ status: tileResponseStatus, contentType: 'image/jpeg', body: tileResponseStatus === 200 ? tileBytes : '' })
  }
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(apiPath)) {
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
const tilesPath = ensureSeekTilesFixture(videoPath)
if (tilesPath === undefined) {
  ng.push('filmstrip 判定用の seek-tile fixture を生成できない')
  await finish(ng)
}
tileBytes = readFileSync(tilesPath)

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
  // `.dark` は matchMedia の change で付く。付く前に測ると dark の回が light を測ってしまう。
  const themed = await page
    .waitForFunction((dark) => document.documentElement.classList.contains('dark') === dark, scheme === 'dark', {
      timeout: 5000,
    })
    .then(() => true)
    .catch(() => false)
  if (!themed) ng.push(`①-c (${scheme}) テーマのクラスが切り替わらない --- 判定の前提が崩れている`)
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
// デスクトップの Tab 順が見た目（左から再生 → 前 → 次）と一致する（WCAG 2.4.3）。
await page.getByRole('button', { name: '再生', exact: true }).focus()
const tabOrder = []
for (let i = 0; i < 3; i += 1) {
  tabOrder.push(
    await page.evaluate(() => {
      const el = document.activeElement
      return { label: el?.getAttribute('aria-label'), left: el?.getBoundingClientRect().left ?? -1 }
    }),
  )
  await page.keyboard.press('Tab')
}
const tabLabels = tabOrder.map((item) => item.label)
const leftToRight = tabOrder.every((item, index) => index === 0 || item.left > tabOrder[index - 1].left)
if (JSON.stringify(tabLabels) !== JSON.stringify(['再生', '前のチャプター', '次のチャプター']) || !leftToRight) {
  ng.push(`①-a デスクトップの Tab 順が見た目（再生 → 前 → 次）と一致しない（${JSON.stringify(tabOrder)}）`)
}
await page.evaluate(() => document.activeElement?.blur())

log('\n=== ①-b 全画面要素に操作バーとシークバーが含まれる ===')
try {
  await page.getByRole('button', { name: '全画面表示' }).click()
  await page.waitForFunction(() => document.fullscreenElement !== null, undefined, { timeout: 5000 })
  const fullscreenContainsPlayerControls = await page.evaluate(() => {
    const fullscreen = document.fullscreenElement
    return Boolean(
      fullscreen?.matches('[data-testid="recording-playback-group"]') &&
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
  // 失敗したまま全画面に残ると後続の判定が全画面の映像に遮られて崩れるので、確実に抜ける。
  await page.evaluate(() => document.fullscreenElement && document.exitFullscreen()).catch(() => {})
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
// #1019 の専用編集画面で境界を選んで前後再生する。
await page.getByRole('button', { name: '再生設定' }).click()
await page.getByRole('menuitem', { name: 'チャプターを直す', exact: true }).click()
await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })
const firstBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][data-time-ms="30000"]')
if ((await firstBoundary.count()) !== 1) ng.push('④ 30 秒の境界が filmstrip に無い --- 判定の前提が崩れている')
else await firstBoundary.click()
await seek(100)
await page.getByRole('button', { name: '選択中の境界の前後3秒を再生' }).click()
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
await firstBoundary.click()
await page.getByRole('button', { name: '選択中の境界の前後3秒を再生' }).click()
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

// 編集に変更は加えていないので「やめる」で通常再生へ戻る。
await page.getByRole('button', { name: 'やめる', exact: true }).click()
await page.waitForSelector('[data-testid="chapter-edit-layout"]', { state: 'detached' })

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
/**
 * defineRgbaOf はページに `rgbaOf(color)`（[r, g, b, a]）を生やす。Tailwind v4 の不透明度
 * 修飾は `oklab(... / a)` で返るので、canvas に塗って RGBA に揃えてから比べる。
 */
const defineRgbaOf = () => {
  window.rgbaOf = (color) => {
    const ctx = document.createElement('canvas').getContext('2d')
    ctx.fillStyle = color
    ctx.fillRect(0, 0, 1, 1)
    const [r, g, b, a] = ctx.getImageData(0, 0, 1, 1).data
    return [r, g, b, a / 255]
  }
}
await page.evaluate(defineRgbaOf)
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
      background: rgbaOf(getComputedStyle(menu).backgroundColor),
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
  const wantRows = [
    'menuitemcheckbox:CM を飛ばす',
    'menuitemcheckbox:字幕',
    'menuitem:再生速度',
    'menuitem:画質',
    'menuitem:チャプターを直す',
  ]
  if (JSON.stringify(names) !== JSON.stringify(wantRows)) {
    ng.push(`⑦ 行リストが「CM を飛ばす / 字幕 / 再生速度 / 画質」でない（${JSON.stringify(names)}）`)
  }
  if (!/標準/.test(desktopMenu.text) || !/h264/.test(desktopMenu.text)) {
    ng.push(`⑦ 行に現在値（標準 / h264）が出ていない（${desktopMenu.text}）`)
  }
  if (/ダウンロード/.test(desktopMenu.text)) ng.push('⑦ 設定メニューにダウンロードが残っている')
  const [red, , , alpha] = desktopMenu.background
  if (alpha >= 1 || alpha === 0 || red > 60) {
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
  // Esc はブラウザが全画面の解除に使うので、歯車で閉じる。
  await gear.click()
  // 時刻の横のチャプター名: 全画面のまま、枠の中に見るためのチャプター一覧を出し、行で飛ぶ。
  // ④ で閉じた編集モードへ誤って入らないことも確かめる。
  await page.locator('[data-testid="playback-chapter"]').click({ timeout: 3000 })
  await page.waitForTimeout(300)
  const chapterList = await page.evaluate(() => {
    const frame = document.querySelector('[data-testid="recording-player-frame"]')
    const fullscreenContainer = document.querySelector('[data-testid="recording-playback-group"]')
    const list = document.querySelector('[data-testid="chapter-list"]')
    const editorOpened = Boolean(document.querySelector('[data-testid="chapter-edit-layout"]'))
    if (!list) return { fullscreen: document.fullscreenElement === fullscreenContainer, exists: false, editorOpened }
    const r = list.getBoundingClientRect()
    return {
      editorOpened,
      fullscreen: document.fullscreenElement === fullscreenContainer,
      exists: true,
      inside: frame.contains(list),
      visible: r.width > 0 && r.height > 0 && r.bottom <= window.innerHeight,
      role: list.getAttribute('role'),
      rows: Array.from(list.querySelectorAll('[role^="menuitem"]')).map((el) => ({
        text: el.textContent?.trim() ?? '',
        checked: el.getAttribute('aria-checked'),
      })),
      editControls: list.querySelectorAll('input, select, textarea, details, [data-testid="chapter-boundary"]').length,
      editText: /保存|変更を破棄|削除|前後3秒|秒/.test(
        Array.from(list.querySelectorAll('button')).map((el) => el.textContent ?? '').join(' ').replace(/\d+:\d+/g, ''),
      ),
    }
  })
  if (!chapterList.fullscreen) ng.push('⑦ チャプター名を押すと全画面が解除された')
  if (chapterList.editorOpened) ng.push('⑦ チャプター名を押すとチャプター編集モードが開いた')
  if (!chapterList.exists || !chapterList.inside || !chapterList.visible || chapterList.role !== 'menu') {
    ng.push(`⑦ 全画面でチャプター名を押しても枠の中にチャプター一覧（role="menu"）が出ない（${JSON.stringify(chapterList)}）`)
  } else {
    const labels = chapterList.rows.map((row) => row.text)
    if (!labels.some((text) => /0:30.*CM/.test(text)) || !labels.some((text) => /1:00.*OP/.test(text))) {
      ng.push(`⑦ チャプター一覧に「時刻・チャプター名」が並んでいない（${JSON.stringify(labels)}）`)
    }
    const current = chapterList.rows.filter((row) => row.checked === 'true')
    if (current.length !== 1 || !current[0].text.includes('OP')) {
      ng.push(`⑦ いまのチャプター（OP、65 秒）に印が付いていない（${JSON.stringify(chapterList.rows)}）`)
    }
    if (chapterList.editControls !== 0 || chapterList.editText) {
      ng.push(`⑦ チャプター一覧に編集の操作がある（controls=${chapterList.editControls} text=${chapterList.editText}）`)
    }
    await page.locator('[data-testid="chapter-list"] [role^="menuitem"]', { hasText: 'CM' }).first().click()
    await page.waitForTimeout(400)
    const afterJump = await page.evaluate(() => ({
      time: document.querySelector('video').currentTime,
      fullscreen: document.fullscreenElement !== null,
      listOpen: Boolean(document.querySelector('[data-testid="chapter-list"]')),
    }))
    if (Math.abs(afterJump.time - 30) > 1 || !afterJump.fullscreen || afterJump.listOpen) {
      ng.push(`⑦ 一覧の CM の行を押しても全画面のまま 30 秒へ飛んで閉じない（${JSON.stringify(afterJump)}）`)
    }
  }
  await page.getByRole('button', { name: '全画面を終了' }).click()
  await page.waitForFunction(() => document.fullscreenElement === null, undefined, { timeout: 5000 })
} catch (error) {
  ng.push(`⑦ 全画面の設定メニューを確かめられない: ${String(error)}`)
  await page.keyboard.press('Escape').catch(() => {})
  // 失敗したまま全画面に残ると後続の判定が全画面の映像に遮られて崩れるので、確実に抜ける。
  await page.evaluate(() => document.fullscreenElement && document.exitFullscreen()).catch(() => {})
}

log('\n=== ⑧ スマホ: 中央に前後チャプター、右上に CC と歯車、歯車は画面下からのシート ===')
const phoneContext = await browser.newContext({
  viewport: { width: 390, height: 844 },
  hasTouch: true,
  isMobile: true,
  locale: 'ja-JP',
  timezoneId: 'Asia/Tokyo',
})
await phoneContext.addInitScript(defineRgbaOf)
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
    frame: frame ? { top: frame.top, bottom: frame.bottom, left: frame.left, right: frame.right } : null,
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
  // 縦は枠の中段、横は再生ボタンが枠の中心に来る（下端の行に並んでいるだけだと左に寄る）。
  const playCenterX = phoneLayout.play.left + phoneLayout.play.width / 2
  const centered =
    [phoneLayout.prev, phoneLayout.play, phoneLayout.next].every(
      (r) => middle(r) > f.top + third && middle(r) < f.bottom - third,
    ) && Math.abs(playCenterX - (f.left + f.right) / 2) < 4
  if (!centered || !(phoneLayout.prev.left < phoneLayout.play.left && phoneLayout.play.left < phoneLayout.next.left)) {
    ng.push(`⑧ 前のチャプター / 再生 / 次のチャプターが映像の中央に並んでいない（${JSON.stringify(phoneLayout)}）`)
  }
  if (!phoneLayout.cc || !phoneLayout.gear || middle(phoneLayout.gear) > f.top + third || middle(phoneLayout.cc) > f.top + third) {
    ng.push(`⑧ CC と歯車が映像の右上にない（cc=${JSON.stringify(phoneLayout.cc)} gear=${JSON.stringify(phoneLayout.gear)}）`)
  }
}
if (phoneLayout.volume !== 0) ng.push(`⑧ スマホにミュート / 音量が ${phoneLayout.volume} 個見えている`)
// 中央の次のチャプター / 前のチャプターが実際に動く。
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
// 境界は区間の端だけ（先頭 0 秒は境界でない）。CM の終端 40 秒まで進めてから 30 秒へ戻す。
await tapVisible('button[aria-label="次のチャプター"]')
await tapVisible('button[aria-label="前のチャプター"]')
const phoneBack = await phone.locator('video').evaluate((v) => v.currentTime)
if (Math.abs(phoneBack - 30) > 1) ng.push(`⑧ 中央の前のチャプターで 30 秒へ戻らない（currentTime=${phoneBack.toFixed(1)}）`)
// 再生中: 映像のタップで操作が出て、暗い幕のタップで隠れる。どちらのタップでも再生は止まらない。
// 幕が pointerup で消えると、続く click が下の <video> に落ちて再生 / 一時停止してしまう。
await phone.locator('video').evaluate((v) => {
  v.muted = true
  return v.play()
})
await phone.waitForTimeout(3800)
const phoneControlsOpacity = () =>
  phone.locator('[data-testid="player-controls"]').evaluate((el) => getComputedStyle(el).opacity)
const hiddenWhilePlaying = await phoneControlsOpacity()
// 左上（CC・歯車・中央のボタンが無いところ）を叩く。
const frameBox = await phone.locator('[data-testid="recording-player-frame"]').boundingBox()
const emptySpot = { x: frameBox.x + 30, y: frameBox.y + 30 }
await phone.touchscreen.tap(emptySpot.x, emptySpot.y)
await phone.waitForTimeout(400)
const afterVideoTap = {
  opacity: await phoneControlsOpacity(),
  paused: await phone.locator('video').evaluate((v) => v.paused),
}
await phone.touchscreen.tap(emptySpot.x, emptySpot.y)
await phone.waitForTimeout(400)
const afterScrimTap = {
  opacity: await phoneControlsOpacity(),
  paused: await phone.locator('video').evaluate((v) => v.paused),
}
await phone.locator('video').evaluate((v) => v.pause())
if (hiddenWhilePlaying !== '0') {
  ng.push(`⑧ 前提: 再生中に操作が隠れていない（opacity=${hiddenWhilePlaying}）`)
} else {
  if (afterVideoTap.opacity !== '1' || afterVideoTap.paused) {
    ng.push(`⑧ 再生中に映像をタップしても操作が出ないか、再生が止まった（${JSON.stringify(afterVideoTap)}）`)
  }
  if (afterScrimTap.opacity !== '0' || afterScrimTap.paused) {
    ng.push(`⑧ 再生中に暗い幕をタップしても操作が隠れないか、再生が止まった（${JSON.stringify(afterScrimTap)}）`)
  }
}
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
    scrimBackground: scrim ? rgbaOf(getComputedStyle(scrim).backgroundColor) : null,
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
  if (!sheet.scrimCovers || !sheet.scrimBackground || sheet.scrimBackground[0] > 40 || sheet.scrimBackground[3] < 0.1) {
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
// スマホのチャプター一覧も画面下からのシートで出る。背後の幕を叩くと設定のシートは閉じる。
await phone.touchscreen.tap(195, 40)
await phone.waitForTimeout(300)
if (await phone.locator('[data-testid="playback-settings"]').count()) ng.push('⑧ 背後の幕を叩いても設定のシートが閉じない')
await tapVisible('[data-testid="playback-chapter"]')
const phoneChapters = await phone.evaluate(() => {
  const list = document.querySelector('[data-testid="chapter-list"]')
  if (!list) return null
  const r = list.getBoundingClientRect()
  return { bottom: r.bottom, top: r.top, innerHeight: window.innerHeight, rows: list.querySelectorAll('[role^="menuitem"]').length }
})
if (!phoneChapters || Math.abs(phoneChapters.bottom - phoneChapters.innerHeight) > 2 || phoneChapters.top < phoneChapters.innerHeight / 2 - 2 || phoneChapters.rows === 0) {
  ng.push(`⑧ スマホでチャプター名を押しても画面下からのシートでチャプター一覧が出ない（${JSON.stringify(phoneChapters)}）`)
}
await phoneContext.close()

log('\n=== ⑨ md 未満の幅でもマウスで映像（操作の幕）を押すと再生 / 一時停止する ===')
await page.setViewportSize({ width: 600, height: 900 })
await page.waitForTimeout(300)
await video.evaluate((v) => v.pause())
const narrowFrame = await page.locator('[data-testid="recording-player-frame"]').boundingBox()
await page.mouse.click(narrowFrame.x + 30, narrowFrame.y + 30)
await page.waitForTimeout(400)
const narrowPaused = await video.evaluate((v) => v.paused)
await video.evaluate((v) => v.pause())
if (narrowPaused) ng.push('⑨ md 未満の幅でマウスで映像を押しても再生が始まらない')
await page.setViewportSize({ width: 1280, height: 900 })

log('\n=== #1019 編集モード: 入り口・<video> を作り直さず再生位置と再生状態を保つ ===')
await page.setViewportSize({ width: 1280, height: 800 })
// 通常速度を 1.5x にしてから編集へ入り、編集用 2x と保存先が分かれていることも見る。
await video.evaluate((v) => {
  v.defaultPlaybackRate = 1.5
  v.playbackRate = 1.5
})
await page.waitForFunction(() => localStorage.getItem('rokuban:playback-rate') === '1.5', undefined, { timeout: 3000 })
await seek(51.5)
await video.evaluate((v) => {
  v.muted = true
  window.__editVideo = v
  return v.play()
})
await page.waitForTimeout(300)
await page.getByRole('button', { name: '再生設定' }).click()
const enterEdit = page.getByRole('menuitem', { name: 'チャプターを直す', exact: true })
let editModeAvailable = (await enterEdit.count()) === 1
if (!editModeAvailable) {
  ng.push('#1019: 設定メニューに「チャプターを直す」が無い')
} else {
  await enterEdit.click()
  await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })
}
const afterEnter = await page.evaluate(() => {
  const v = document.querySelector('video')
  return { same: v === window.__editVideo, time: v?.currentTime ?? -1, paused: v?.paused ?? true }
})
log(`  入った直後: ${JSON.stringify(afterEnter)}`)
if (!afterEnter.same) ng.push('#1019: 編集モードに入ると <video> が作り直される')
if (afterEnter.time < 51.5) ng.push(`#1019: 編集モードに入ると再生位置が戻る（currentTime=${afterEnter.time}）`)
if (afterEnter.paused) ng.push('#1019: 編集モードに入ると再生が止まる')
await video.evaluate((v) => v.pause())
log('\n=== #1123 編集速度: 2x 再生、通常速度の保護、400px 幅 ===')
const editRateButton = page.getByTestId('chapter-edit-playback-rate')
for (let index = 0; index < 7 && (await editRateButton.textContent())?.trim() !== '2x'; index += 1) {
  await editRateButton.click()
}
const editRate = await video.evaluate((v) => v.playbackRate)
await page.waitForFunction(
  () => localStorage.getItem('rokuban:chapter-edit-playback-rate') === '2',
  undefined,
  { timeout: 3000 },
)
const ratesWhileEditing = await page.evaluate(() => ({
  normal: localStorage.getItem('rokuban:playback-rate'),
  editing: localStorage.getItem('rokuban:chapter-edit-playback-rate'),
}))
if (editRate !== 2) ng.push(`#1123: 編集帯の速度操作で video が 2x にならない（${editRate}x）`)
if (ratesWhileEditing.normal !== '1.5' || ratesWhileEditing.editing !== '2') {
  ng.push(`#1123: 編集中の速度保存先が分離されていない（${JSON.stringify(ratesWhileEditing)}）`)
}
await seek(15)
const editRateProgress = await playFor(1200)
if (editRateProgress < 16.6 || editRateProgress > 19.2) {
  ng.push(`#1123: 編集中の 2x で再生位置が 2 倍に進まない（1.2 秒後 ${editRateProgress.toFixed(2)} 秒、開始 15 秒）`)
}
await seek(51.5)
const editHeader = page.locator('header h1').last()
const resetButton = page.getByRole('button', { name: '自動に戻す', exact: true })
const initialHeader = await editHeader.textContent()
if (!initialHeader?.includes('チャプターを直す') || !initialHeader.includes('チャプター確認用') || !initialHeader.includes('自動検出（未確認）')) {
  ng.push(`#1019: 編集ヘッダーに編集名/番組名/未確認状態が無い（${initialHeader}）`)
}
if (!(await page.getByRole('button', { name: '編集をやめる' }).isVisible())) {
  ng.push('#1019 desktop: roughにある編集用の戻る矢印がない')
}

/** shot は E2E_SHOT_DIR があるときだけ、いまの画面を保存する。 */
async function shot(name) {
  const dir = process.env.E2E_SHOT_DIR
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  await page.screenshot({ path: path.join(dir, `${name}.png`) })
}

/** shotBoth は同じ状態をデスクトップ (1280x800) とスマホ (400x800) の両方で保存する。 */
async function shotBoth(name) {
  if (!process.env.E2E_SHOT_DIR) return
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.waitForTimeout(150)
  await shot(`${name}-desktop`)
  await page.setViewportSize({ width: 400, height: 800 })
  await page.waitForTimeout(150)
  await shot(`${name}-phone`)
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.waitForTimeout(150)
}

/**
 * readPixels は PNG (base64) を別ページの canvas に描いて、指定点の画素を返す。
 * **ブラウザが実際に描いた画素を読む**（DOM の矩形ではなく画像の中身を見る）。
 */
const auxPage = await context.newPage()
async function readPixels(png, points) {
  return auxPage.evaluate(
    async ({ b64, points }) => {
      const img = new Image()
      img.src = `data:image/png;base64,${b64}`
      await img.decode()
      const canvas = document.createElement('canvas')
      canvas.width = img.width
      canvas.height = img.height
      const ctx = canvas.getContext('2d')
      ctx.drawImage(img, 0, 0)
      return points.map(([x, y]) => Array.from(ctx.getImageData(x, y, 1, 1).data.slice(0, 3)))
    },
    { b64: png.toString('base64'), points },
  )
}

/**
 * checkTilePixels は表示中の各マスの 3 点（左寄り・中央・右寄り）の画素が、その時刻のタイルの色か
 * を見る。時刻→x だけでは「そのマスにどのフレームが出ているか」は分からない。
 */
async function checkTilePixels(label) {
  const trackBox = await page.locator('[data-testid="chapter-filmstrip-track"]').boundingBox()
  const cells = await page.evaluate(() =>
    Array.from(document.querySelectorAll('[data-testid="chapter-filmstrip-tile"]')).map((el) => {
      const r = el.getBoundingClientRect()
      return { seconds: Number(el.getAttribute('data-time-seconds')), x: r.x, y: r.y, width: r.width, height: r.height }
    }),
  )
  if (!trackBox || cells.length === 0) {
    ng.push(`${label}: タイルのマスが取れない`)
    return
  }
  const png = await page.screenshot({ clip: { x: trackBox.x, y: trackBox.y, width: trackBox.width, height: trackBox.height } })
  let checked = 0
  for (const cell of cells) {
    // 切る区間（CM）のマスは橙の幕が重なって画素が混ざる。幕のないマスで見る。
    const covered = cell.seconds >= CM_SPAN.startMs / 1000 && cell.seconds < CM_SPAN.endMs / 1000
    // 四隅を読むので、帯に全体が入っているマスだけを見る。
    const inside = cell.x >= trackBox.x - 0.5 && cell.x + cell.width <= trackBox.x + trackBox.width + 0.5
    if (!inside || cell.seconds >= 120 || covered) continue
    // 四隅寄り（横・縦とも 20% / 80%）の 4 点。象限ごとに青が違うので、タイル全体が縮んで入っているかが分かる。
    const points = []
    for (const fy of [0.2, 0.8]) {
      for (const fx of [0.2, 0.8]) {
        points.push([Math.round(cell.x + cell.width * fx - trackBox.x), Math.round(cell.y + cell.height * fy - trackBox.y)])
      }
    }
    const pixels = await readPixels(png, points)
    const [wantR, wantG] = tileColor(cell.seconds)
    for (const [i, px] of pixels.entries()) {
      const want = [wantR, wantG, QUAD_BLUE[i]]
      if (px.some((value, channel) => Math.abs(value - want[channel]) > 14)) {
        ng.push(`${label}: ${cell.seconds}秒のマスの画素が違う（隅${i} 実際 rgb(${px}) / 期待 rgb(${want})）`)
        break
      }
    }
    // 1 枚が収まっている: マスの縦横比は 16:9。
    if (Math.abs(cell.width / cell.height - 16 / 9) > 0.05) {
      ng.push(`${label}: ${cell.seconds}秒のマスが 16:9 でない（${cell.width.toFixed(1)}x${cell.height.toFixed(1)}）`)
    }
    checked += 1
  }
  log(`  ${label}: ${checked} マスの画素を確認`)
  if (checked < 3) ng.push(`${label}: 画素を確認できたマスが ${checked} 個しかない`)
}

async function editLayoutMetrics(viewport) {
  await page.setViewportSize(viewport)
  await page.waitForTimeout(200)
  return page.evaluate(() => {
    const box = (selector) => {
      const element = document.querySelector(selector)
      if (!element) return null
      const rect = element.getBoundingClientRect()
      return { x: rect.x, y: rect.y, width: rect.width, height: rect.height, bottom: rect.bottom }
    }
    const list = document.querySelector('[data-testid="chapter-span-list"]')
    const nav = document.querySelector('[data-testid="bottom-nav"]')
    const navRect = nav && getComputedStyle(nav).display !== 'none' ? nav.getBoundingClientRect() : null
    return {
      viewport: { width: window.innerWidth, height: window.innerHeight },
      body: box('[data-testid="recording-detail-body"]'),
      editing: box('[data-testid="chapter-edit-layout"]'),
      player: box('[data-testid="chapter-edit-player"]'),
      spans: box('[data-testid="chapter-span-list"]'),
      strip: box('[data-testid="chapter-filmstrip"]'),
      tuning: box('[data-testid="chapter-tuning-controls"]'),
      editBar: box('[data-testid="chapter-edit-playback-controls"]'),
      editBarOverflow: (() => {
        const bar = document.querySelector('[data-testid="chapter-edit-playback-controls"]')
        if (!bar) return true
        const bounds = bar.getBoundingClientRect()
        return Array.from(bar.children).filter((child) => getComputedStyle(child).display !== 'none').some((child) => {
          const rect = child.getBoundingClientRect()
          return rect.left < bounds.left - 0.5 || rect.right > bounds.right + 0.5
        })
      })(),
      navTop: navRect ? navRect.top : null,
      documentOverflow: document.documentElement.scrollHeight - window.innerHeight,
      listOverflowY: list ? getComputedStyle(list).overflowY : null,
      listClientHeight: list?.clientHeight ?? 0,
      listScrollHeight: list?.scrollHeight ?? 0,
    }
  })
}

for (const viewport of [{ width: 1280, height: 800 }, { width: 400, height: 800 }, { width: 400, height: 667 }]) {
  const phone = viewport.width <= 400
  const metrics = await editLayoutMetrics(viewport)
  log(`  ${viewport.width}×${viewport.height}: ${JSON.stringify(metrics)}`)
  if (!metrics.editing || !metrics.player || !metrics.spans || !metrics.strip || !metrics.tuning) {
    ng.push(`#1019 ${viewport.width}px: player・区間一覧・filmstrip・境界調整を同じ編集画面に出していない`)
  } else {
    const floor = metrics.navTop ?? viewport.height
    for (const [name, rect] of [['player', metrics.player], ['spans', metrics.spans], ['strip', metrics.strip], ['tuning', metrics.tuning]]) {
      if (rect.y < 0 || rect.bottom > floor + 0.5) {
        ng.push(`#1019 ${viewport.width}x${viewport.height}: ${name} が最初の画面（下部ナビの上端 ${floor}）に収まらない（bottom=${rect.bottom}）`)
      }
    }
    if (!phone) {
      if (!(metrics.player.x < metrics.spans.x && metrics.strip.width > metrics.spans.width)) {
        ng.push(`#1019 desktop: player左・区間一覧右・全幅filmstripの配置がroughと異なる（${JSON.stringify(metrics)}）`)
      }
      if (metrics.strip.y - metrics.player.bottom > 40) {
        ng.push(`#1019 desktop: プレイヤーの下に ${(metrics.strip.y - metrics.player.bottom).toFixed(0)}px の空きがある`)
      }
    } else {
      if (!metrics.editBar || metrics.editBarOverflow) {
        ng.push(`#1123: 編集帯の要素が ${viewport.width}px 幅に収まらない（${JSON.stringify(metrics.editBar)} overflow=${metrics.editBarOverflow}）`)
      }
      if (!(metrics.player.y < metrics.strip.y && metrics.strip.y < metrics.tuning.y && metrics.tuning.y < metrics.spans.y)) {
        ng.push(`#1019 mobile: player→filmstrip→tuning→区間一覧の順がroughと異なる（${JSON.stringify(metrics)}）`)
      }
      if (metrics.listOverflowY !== 'auto' && metrics.listOverflowY !== 'scroll') {
        ng.push('#1019 mobile: 区間一覧以外を固定し、一覧だけスクロールできる構造になっていない')
      }
      if (metrics.documentOverflow > 1) {
        ng.push(`#1019 ${viewport.width}x${viewport.height}: ページ自体が ${metrics.documentOverflow}px スクロールできる（一覧だけがスクロールするはず）`)
      }
      if (metrics.tuning.height > 56) {
        ng.push(`#1019 mobile: 調整欄が 1 行に収まらない（height=${metrics.tuning.height}）`)
      }
      if (await resetButton.isVisible()) ng.push('#1019 mobile: 自動に戻すが編集ヘッダーに出ている')
      // 一覧の末尾まで送ると、最後の要素（「ここから区間を足す」）が下部ナビの上に出る。
      const listEnd = await page.evaluate(() => {
        const list = document.querySelector('[data-testid="chapter-span-list"]')
        list.scrollTop = list.scrollHeight
        const add = Array.from(list.querySelectorAll('button')).find((b) => b.textContent?.includes('ここから区間を足す'))
        const nav = document.querySelector('[data-testid="bottom-nav"]')
        return {
          addBottom: add?.getBoundingClientRect().bottom ?? null,
          addTop: add?.getBoundingClientRect().top ?? null,
          navTop: nav?.getBoundingClientRect().top ?? null,
          listBottom: list.getBoundingClientRect().bottom,
        }
      })
      if (listEnd.addBottom === null || listEnd.navTop === null || listEnd.addBottom > listEnd.navTop - 2) {
        ng.push(`#1019 ${viewport.width}x${viewport.height}: 一覧の末尾が下部ナビに隠れる（${JSON.stringify(listEnd)}）`)
      }
      if (viewport.height === 800) {
        await shot('list-end-phone')
        await page.evaluate(() => {
          document.querySelector('[data-testid="chapter-span-list"]').scrollTop = 0
        })
      }
    }
  }
  if (!phone) {
    const initialRange = await page.locator('[data-testid="chapter-filmstrip-track"]').evaluate((el) => ({
      start: Number(el.dataset.visibleStartSeconds),
      end: Number(el.dataset.visibleEndSeconds),
    }))
    if (initialRange.start > 51.5 || initialRange.end < 51.5) {
      ng.push(`#1019 desktop: ストリップの初期範囲が入る前の位置 51.5 秒を含まない（${JSON.stringify(initialRange)}）`)
    }
  } else if (viewport.height === 800) {
    // 400px では範囲が動画より短くなる。入る前の位置 (51.5 秒) の周りで開く。
    const initialRange = await page.locator('[data-testid="chapter-filmstrip-track"]').evaluate((el) => ({
      start: Number(el.dataset.visibleStartSeconds),
      end: Number(el.dataset.visibleEndSeconds),
    }))
    const center = (initialRange.start + initialRange.end) / 2
    if (Math.abs(center - 51.5) > 6 || initialRange.end - initialRange.start > 100) {
      ng.push(`#1019 mobile: ストリップの初期範囲が入る前の位置の周りでない（${JSON.stringify(initialRange)}）`)
    }
    log(`  400px の初期範囲: ${JSON.stringify(initialRange)}`)
  }
  if (viewport.height === 800) {
    await checkTilePixels(`#1019 タイルの画素 ${viewport.width}px`)
    await shot(`entered-${phone ? 'phone' : 'desktop'}`)
  }
}

log('\n=== 編集モード: 本文幅が通常表示の映像高さ上限で絞られない ===')
// 上限 (100dvh - 18rem) * 16/9 は映像が本文全幅を占める通常表示の式。編集中は映像の横に区間一覧があるので掛けない。
{
  const metrics720 = await editLayoutMetrics({ width: 1280, height: 720 })
  const cap = ((720 - 288) * 16) / 9
  log(`  1280×720 本文幅=${metrics720.body?.width} 上限=${cap}`)
  if (metrics720.body === null || !(metrics720.body.width > cap + 1)) {
    ng.push(`編集モード 1280×720: 本文幅が通常表示の上限 ${cap}px で絞られている（body=${metrics720.body?.width}）`)
  }
  await page.setViewportSize({ width: 1280, height: 800 })
}

log('\n=== #1019 zoom limit: 1 マスがタイルの実画素幅 160px を超えて引き伸ばされない ===')
await page.setViewportSize({ width: 1280, height: 800 })
const zoomIn = page.getByRole('button', { name: 'フィルムストリップを拡大' })
for (let index = 0; index < 12 && (await zoomIn.isEnabled()); index += 1) await zoomIn.click()
const maxZoom = await page.evaluate(() => {
  const track = document.querySelector('[data-testid="chapter-filmstrip-track"]')
  const tile = document.querySelector('[data-testid="chapter-filmstrip-tile"]')
  return {
    trackWidth: track.getBoundingClientRect().width,
    cellWidth: tile.getBoundingClientRect().width,
    cellHeight: tile.getBoundingClientRect().height,
    range: Number(track.dataset.visibleEndSeconds) - Number(track.dataset.visibleStartSeconds),
  }
})
log(`  最大拡大: ${JSON.stringify(maxZoom)}`)
if (!(await zoomIn.isDisabled())) ng.push('#1019: 拡大の上限で「＋」が無効にならない')
if (Math.abs(maxZoom.cellWidth - 160) > 1.5) {
  ng.push(`#1019: 最大拡大で 1 マスの幅が実画素幅 160px でない（${maxZoom.cellWidth.toFixed(1)}px）`)
}
await checkTilePixels('#1019 最大拡大のタイルの画素')
await shotBoth('max-zoom')
await page.getByRole('button', { name: '全体', exact: true }).click()

log('\n=== #1019 編集中の自動スキップ抑止 ===')
await page.setViewportSize({ width: 1280, height: 800 })
await seek(28)
const editPlayback = await playFor(4000)
if (editPlayback >= 40 || editPlayback < 31) {
  ng.push(`#1019: 編集画面の再生で自動スキップが抑止されない、または再生が進まない（${editPlayback.toFixed(1)}秒）`)
}

log('\n=== #1019 filmstrip: timestamp → x ===')
const tilePosition = await page.evaluate(() => {
  const strip = document.querySelector('[data-testid="chapter-filmstrip-track"]')
  const tile = document.querySelector('[data-testid="chapter-filmstrip-tile"][data-time-seconds="30"]')
  if (!strip || !tile) return null
  const s = strip.getBoundingClientRect()
  const t = tile.getBoundingClientRect()
  const duration = Number(strip.getAttribute('data-duration-seconds'))
  const start = Number(strip.getAttribute('data-visible-start-seconds'))
  const end = Number(strip.getAttribute('data-visible-end-seconds'))
  return {
    actualX: t.left,
    expectedX: s.left + ((30 - start) / (end - start)) * s.width,
    duration,
    start,
    end,
  }
})
log(`  30秒 tile: ${JSON.stringify(tilePosition)}`)
await checkTilePixels('#1019 全体表示のタイルの画素')
if (!tilePosition || Math.abs(tilePosition.actualX - tilePosition.expectedX) > 2) {
  ng.push(`#1019: 30秒tileのx位置が表示範囲の時刻→x計算と一致しない（${JSON.stringify(tilePosition)}）`)
}

log('\n=== #1019 filmstrip: 境界選択・1 frame 調整・PUT ===')
await page.setViewportSize({ width: 1280, height: 800 })
if (await resetButton.isVisible() && await resetButton.isEnabled()) {
  ng.push('#1019: 自動検出結果のまま「自動に戻す」が有効になっている')
}
const targetBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][data-time-ms="30000"]')
if ((await targetBoundary.count()) !== 1) {
  ng.push('#1019: 30秒境界がfilmstripに無い')
} else {
  await targetBoundary.click()
  if ((await targetBoundary.getAttribute('aria-pressed')) !== 'true') {
    ng.push('#1019: filmstrip境界を押しても選択状態にならない')
  }
  await shotBoth('selected')
  const save = page.getByRole('button', { name: /^(保存|このまま確認)$/ })
  const track = page.locator('[data-testid="chapter-filmstrip-track"]')
  const trackBox = await track.boundingBox()
  const boundaryBox = await targetBoundary.boundingBox()
    if (!trackBox || !boundaryBox) {
      ng.push('#1019: coarse drag の境界/track矩形を取得できない')
    } else {
    const rangeStart = Number(await track.getAttribute('data-visible-start-seconds'))
    const rangeEnd = Number(await track.getAttribute('data-visible-end-seconds'))
    const deltaX = (4 / (rangeEnd - rangeStart)) * trackBox.width
    const cutRange = page.locator('[data-testid="chapter-filmstrip-cut-range"]').first()
    const cutRangeBefore = await cutRange.getAttribute('style')
    const originalCenterX = boundaryBox.x + boundaryBox.width / 2
    const centerY = boundaryBox.y + boundaryBox.height / 2
    await page.mouse.move(originalCenterX, centerY)
    await page.mouse.down()
    await page.mouse.move(originalCenterX + deltaX, centerY, { steps: 3 })
    const previewBoundary = await page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]').getAttribute('data-time-ms')
    const previewCutRange = await cutRange.getAttribute('style')
    const previewStatus = await editHeader.textContent()
    if (Number(previewBoundary) <= 30_000 || previewStatus?.includes('未保存の変更があります')) {
      ng.push(`#1019: drag中にpreviewだけが動き、未releaseのdraft/保存は変わらない（preview=${previewBoundary}）`)
    }
    if (previewCutRange === cutRangeBefore) {
      ng.push('#1121: drag中の cut 区間がプレビュー形状へ更新されない')
    }
    await page.mouse.up()
    const movedBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]')
    const committedBoundary = await movedBoundary.getAttribute('data-time-ms')
    const committedCutRange = await page.locator('[data-testid="chapter-filmstrip-cut-range"]').first().getAttribute('style')
    if (committedBoundary !== previewBoundary) {
      ng.push(`#1019: pointer release がpreviewの境界を下書きへ commit しない（preview=${previewBoundary}, committed=${committedBoundary}）`)
    }
    if (committedCutRange !== previewCutRange) {
      ng.push(`#1121: drag中の cut 区間と離した後の形が違う（preview=${previewCutRange}, committed=${committedCutRange}）`)
    }
    const movedBox = await movedBoundary.boundingBox()
    const latestTrackBox = await track.boundingBox()
    if (!movedBox || !latestTrackBox) {
      ng.push('#1019: release後の境界位置を取得できない')
    } else {
      const movedCenterX = movedBox.x + movedBox.width / 2
      const currentRangeStart = Number(await track.getAttribute('data-visible-start-seconds'))
      const currentRangeEnd = Number(await track.getAttribute('data-visible-end-seconds'))
      const originalX = latestTrackBox.x + ((30 - currentRangeStart) / (currentRangeEnd - currentRangeStart)) * latestTrackBox.width
      await page.mouse.move(movedCenterX, movedBox.y + movedBox.height / 2)
      await page.mouse.down()
      await page.mouse.move(originalX, movedBox.y + movedBox.height / 2, { steps: 3 })
      const restorePreview = await page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]').getAttribute('data-time-ms')
      await page.mouse.up()
      await page.waitForTimeout(100)
      const restoredBoundary = await page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]').getAttribute('data-time-ms')
      const restoredStatus = await editHeader.textContent()
      log(`  drag ${previewBoundary} preview → ${committedBoundary} released → ${restorePreview} reverse preview → ${restoredBoundary} restored; dirty=${restoredStatus?.includes('未保存の変更があります')}`)
      if (restoredStatus?.includes('未保存の変更があります')) ng.push('#1019: coarse dragを元の境界に戻してもdirtyが解除されない')
    }
  }
  // #1121: 他の区間に重ねる。CM [30,40] の終端を OP [60,70] の内側 65 秒へ運ぶと、cut が優先して OP は [65,70] に削られる。
  // ドラッグ中のプレビューと離した後の cut 帯・境界の集合が一致することを判定する。
  {
    await page.getByRole('button', { name: '全体', exact: true }).click()
    const overlapShape = () => page.evaluate(() => ({
      cuts: [...document.querySelectorAll('[data-testid="chapter-filmstrip-cut-range"]')].map((el) => el.getAttribute('style')),
      times: [...document.querySelectorAll('[data-testid="chapter-filmstrip-boundary"]')].map((el) => el.getAttribute('data-time-ms')).sort(),
    }))
    const cmEnd = page.locator('[data-testid="chapter-filmstrip-boundary"][data-time-ms="40000"]')
    const overlapTrack = await track.boundingBox()
    const cmEndBox = await cmEnd.boundingBox()
    const start = Number(await track.getAttribute('data-visible-start-seconds'))
    const end = Number(await track.getAttribute('data-visible-end-seconds'))
    if (!overlapTrack || !cmEndBox || start > 40 || end < 70) {
      ng.push(`#1121: 重ねるドラッグの前提（40 秒境界・表示範囲 ${start}-${end}）が整わない`)
    } else {
      const y = cmEndBox.y + cmEndBox.height / 2
      await page.mouse.move(cmEndBox.x + cmEndBox.width / 2, y)
      await page.mouse.down()
      await page.mouse.move(overlapTrack.x + ((65 - start) / (end - start)) * overlapTrack.width, y, { steps: 5 })
      const overlapPreview = await overlapShape()
      await page.mouse.up()
      const overlapCommitted = await overlapShape()
      log(`  overlap drag: preview=${JSON.stringify(overlapPreview)} committed=${JSON.stringify(overlapCommitted)}`)
      if (JSON.stringify(overlapPreview) !== JSON.stringify(overlapCommitted)) {
        ng.push(`#1121: 重ねるドラッグ中の形と離した後の形が違う（preview=${JSON.stringify(overlapPreview)}, committed=${JSON.stringify(overlapCommitted)}）`)
      }
      const t = overlapCommitted.times.map(Number)
      if (overlapPreview.cuts.length !== 1 || !t.some((ms) => Math.abs(ms - 65_000) < 40) || t.includes(60_000) || t.includes(40_000)) {
        ng.push(`#1121: cut が OP を [65,70] へ削る形になっていない（${JSON.stringify(overlapCommitted)}）`)
      }
      await page.getByRole('button', { name: '元に戻す', exact: true }).click()
      const afterUndo = await overlapShape()
      if (!afterUndo.times.includes('40000') || !afterUndo.times.includes('60000')) {
        ng.push(`#1121: 重ねたドラッグを元に戻しても境界が戻らない（${JSON.stringify(afterUndo)}）`)
      }
    }
  }
  // 選択が何になるかは再生位置に最も近い境界へ落ちるので、重なりの検査後は 30 秒境界を選び直す。
  await targetBoundary.click()
  const nudge = page.getByRole('button', { name: '選択中の境界を 1 フレーム進める', exact: true })
  if ((await nudge.count()) !== 1) {
    ng.push('#1019: 選択境界ひとつだけの調整欄に +1 frame が無い')
  } else {
    await nudge.click()
    const selectedBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]')
    const adjusted = await selectedBoundary.getAttribute('data-time-ms')
  if (Number(adjusted) < 30_032 || Number(adjusted) > 30_034) {
    ng.push(`#1019: +1 frame が境界を約33ms動かさない（${adjusted}ms）`)
  }
  await page.waitForFunction(() => document.querySelector('header h1')?.textContent?.includes('未保存の変更があります'))
  // フレーム単位の調整が画面に出る（1 秒単位の表示では +1 フレームが見えない）。
  const shownBoundary = (await page.getByTestId('chapter-selected-boundary').textContent()) ?? ''
  const shownPlayhead = (await page.getByTestId('chapter-edit-playhead').textContent()) ?? ''
  if (!/^0:30\.03[2-4]$/.test(shownBoundary)) ng.push(`#1019: 選んだ境界の表示がミリ秒の 0:30.033 前後でない（${shownBoundary}）`)
  if (!/\d+:\d\d\.\d{3}/.test(shownPlayhead)) ng.push(`#1019: プレイヤーの時刻がミリ秒つきでない（${shownPlayhead}）`)
  await shotBoth('unsaved')
  // ← → は調整ボタンを押した後（フォーカスが境界ボタンにない）でも前後の境界へ移る。
  await nudge.focus()
  await page.keyboard.press('ArrowRight')
  const afterArrow = await page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]').getAttribute('data-time-ms')
  if (afterArrow !== '40000') ng.push(`#1019: 調整ボタンにフォーカスがあると ← → で次の境界へ移らない（選択=${afterArrow}）`)
  await page.keyboard.press('ArrowLeft')
  const afterBack = await page.locator('[data-testid="chapter-filmstrip-boundary"][aria-pressed="true"]').getAttribute('data-time-ms')
  if (Number(afterBack) < 30_032 || Number(afterBack) > 30_034) ng.push(`#1019: ← で元の境界へ戻らない（選択=${afterBack}）`)
}
  if ((await save.count()) !== 1) {
    ng.push('#1019: 編集ヘッダーに保存が無い')
  } else {
    const response = page.waitForResponse((res) => res.url().includes('/chapter-edits') && res.request().method() === 'PUT')
    await save.click()
    await response
  }
  await page.waitForSelector('[data-testid="chapter-edit-layout"]', { state: 'detached', timeout: 5000 })
  const ratesAfterSave = await page.evaluate((v) => ({
    current: v.playbackRate,
    default: v.defaultPlaybackRate,
    normal: localStorage.getItem('rokuban:playback-rate'),
    editing: localStorage.getItem('rokuban:chapter-edit-playback-rate'),
  }), await video.elementHandle())
  if (ratesAfterSave.current !== 1.5 || ratesAfterSave.default !== 1.5 || ratesAfterSave.normal !== '1.5' || ratesAfterSave.editing !== '2') {
    ng.push(`#1123: 保存後に通常速度へ戻らない / 編集速度を保たない（${JSON.stringify(ratesAfterSave)}）`)
  }
  const submitted = chapterEditBodies.at(-1)
  if (!submitted || submitted.version !== 'auto:detected:1' || submitted.spans?.[0]?.startMs !== 30_033) {
    ng.push(`#1019: PUT本文にframe調整済み境界と元versionが無い（${JSON.stringify(submitted)}）`)
  }
}
if (seekTileRequests.length !== 1) {
  ng.push(`#1019: seek-tile格子画像が一回のGETで共有されていない（GET数=${seekTileRequests.length}）`)
}

log('\n=== #1019 dirty cancel: 確認中は編集を続けられ、破棄で戻る ===')
// Save は保存成功後に編集モードを閉じるので、ここからもう一度入る。
await page.getByRole('button', { name: '再生設定' }).click()
await page.getByRole('menuitem', { name: 'チャプターを直す', exact: true }).click()
await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })
const resumedRate = await video.evaluate((v) => v.playbackRate)
if (resumedRate !== 2 || (await page.getByTestId('chapter-edit-playback-rate').textContent())?.trim() !== '2x') {
  ng.push(`#1123: 再編集時に保存した 2x が復元されない（${resumedRate}x）`)
}
const secondBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][data-time-ms="60000"]')
if ((await secondBoundary.count()) === 1) {
  await secondBoundary.click()
  const beforeUnloadPrevented = () =>
    page.evaluate(() => {
      const event = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(event)
      return event.defaultPrevented
    })
  if (await beforeUnloadPrevented()) ng.push('#1019: 下書きが無いのにリロード・タブを閉じる操作の確認(beforeunload)が登録されている')
  await page.getByRole('button', { name: '選択中の境界を 1 フレーム進める' }).click()
  await page.waitForFunction(() => {
    const save = [...document.querySelectorAll('button')].find((button) => button.textContent?.trim() === '保存')
    return save !== undefined && !save.disabled
  })
  await page.waitForTimeout(100)
  if (!(await beforeUnloadPrevented())) ng.push('#1019: 未保存の下書きがあるのにリロード・タブを閉じる操作でブラウザ標準の確認(beforeunload)が出ない')
  const stripTopBefore = (await page.locator('[data-testid="chapter-filmstrip"]').boundingBox())?.y
  await page.getByRole('button', { name: 'やめる', exact: true }).click()
  if ((await page.locator('[data-testid="chapter-exit-confirmation"]').count()) !== 1) {
    ng.push('#1019: dirty な「やめる」で独自の確認バーが出ない')
  }
  const stripTopWith = (await page.locator('[data-testid="chapter-filmstrip"]').boundingBox())?.y
  if (stripTopBefore === undefined || stripTopWith === undefined || Math.abs(stripTopBefore - stripTopWith) > 1) {
    ng.push(`#1019: 確認バーが出るとストリップが押し下げられる（${stripTopBefore} → ${stripTopWith}）`)
  }
  await page.getByRole('button', { name: '編集を続ける' }).click()
  if ((await page.locator('[data-testid="chapter-edit-layout"]').count()) !== 1) {
    ng.push('#1019: 「編集を続ける」でドラフトを保持していない')
  }
  await page.getByRole('button', { name: 'やめる', exact: true }).click()
  await page.getByRole('button', { name: '変更を捨てる' }).click()
  await page.waitForSelector('[data-testid="chapter-edit-layout"]', { state: 'detached' })
  const ratesAfterDiscard = await video.evaluate((v) => ({ current: v.playbackRate, default: v.defaultPlaybackRate }))
  if (ratesAfterDiscard.current !== 1.5 || ratesAfterDiscard.default !== 1.5) {
    ng.push(`#1123: 破棄後に通常速度へ戻らない（${JSON.stringify(ratesAfterDiscard)}）`)
  }
  if (await beforeUnloadPrevented()) ng.push('#1019: 下書きを捨てて編集を終えた後も beforeunload が残っている')
} else {
  ng.push('#1019: dirty cancel 確認に使う60秒境界が無い')
}

log('\n=== #1019 dirty route navigation: SPA link uses the same in-band guard ===')
await page.getByRole('button', { name: '再生設定' }).click()
await page.getByRole('menuitem', { name: 'チャプターを直す', exact: true }).click()
await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })
const routeBoundary = page.locator('[data-testid="chapter-filmstrip-boundary"][data-time-ms="60000"]')
if ((await routeBoundary.count()) === 1) {
  await routeBoundary.click()
  await page.getByRole('button', { name: '選択中の境界を 1 フレーム進める' }).click()
  await page.waitForFunction(() => {
    const save = [...document.querySelectorAll('button')].find((button) => button.textContent?.trim() === '保存')
    return save !== undefined && !save.disabled
  })
  let nativeDialogSeen = false
  page.on('dialog', async (dialog) => {
    nativeDialogSeen = true
    await dialog.dismiss()
  })
  const recordingsLink = page.locator('a[href="/recordings"]:visible').first()
  if ((await recordingsLink.count()) === 0) {
    ng.push('#1019: dirty route guard を起動する録画一覧リンクが見つからない')
  } else {
    await recordingsLink.click()
    await page.waitForSelector('[data-testid="chapter-exit-confirmation"]', { timeout: 5000 })
    await page.getByRole('button', { name: '編集を続ける' }).click()
    if (!page.url().includes('/recordings/1') || (await page.locator('[data-testid="chapter-edit-layout"]').count()) !== 1) {
      ng.push('#1019: route guard の「編集を続ける」で録画ページ・下書きを保持しない')
    }
    await recordingsLink.click()
    await page.waitForSelector('[data-testid="chapter-exit-confirmation"]', { timeout: 5000 })
    await page.getByRole('button', { name: '変更を捨てる' }).click()
    await page.waitForURL('**/recordings', { timeout: 5000 })
    if (nativeDialogSeen) ng.push('#1019: dirty SPA route navigation が native confirm を開いた')
  }
} else {
  ng.push('#1019: route guard 確認に使う60秒境界が無い')
}

log('\n=== #1019 tile 404: 画像なしでも境界調整を続けられる ===')
tileResponseStatus = 404
await page.goto(URL_BASE + '/recordings/1', { waitUntil: 'domcontentloaded' })
await video.waitFor({ timeout: 15000 })
await page.waitForFunction(
  () => Number.isFinite(document.querySelector('video')?.duration) && document.querySelector('video').duration > 0,
  undefined,
  { timeout: 15000 },
)
const missingTileResponse = page.waitForResponse((response) => response.url().includes('/seek-tiles') && response.status() === 404)
await page.getByRole('button', { name: '再生設定' }).click()
await page.getByRole('menuitem', { name: 'チャプターを直す', exact: true }).click()
await page.waitForSelector('[data-testid="chapter-filmstrip"] img', { timeout: 5000 })
await missingTileResponse
if ((await page.locator('[data-testid="chapter-filmstrip-boundary"]').count()) === 0) {
  ng.push('#1019: seek-tileが404だと境界が表示されない')
}
if (await page.getByRole('button', { name: '選択中の境界を 1 フレーム進める' }).isDisabled()) {
  ng.push('#1019: seek-tileが404だと境界調整が使えない')
}
if (seekTileRequests.length !== 2) {
  ng.push(`#1019: tile 404 を含む各編集画面が1回ずつタイル画像を要求していない（GET数=${seekTileRequests.length}）`)
}
if (await page.getByRole('button', { name: '自動に戻す', exact: true }).isVisible()) {
  if (await page.getByRole('button', { name: '自動に戻す', exact: true }).isDisabled()) {
    ng.push('#1019: 確認済み状態で自動に戻すが無効になっている')
  } else {
    const resetResponse = page.waitForResponse((response) => response.url().includes('/chapter-edits') && response.request().method() === 'DELETE')
    await page.getByRole('button', { name: '自動に戻す', exact: true }).click()
    await resetResponse
    await page.waitForFunction(() => document.querySelector('[data-testid="chapter-edit-layout"]') === null, undefined, { timeout: 5000 }).catch(() => undefined)
    const editLayoutCountAfterReset = await page.locator('[data-testid="chapter-edit-layout"]').count()
    log(`  reset DELETE count=${chapterResetCount}; editor layout count=${editLayoutCountAfterReset}`)
    if (chapterResetCount !== 1 || editLayoutCountAfterReset !== 0) ng.push('#1019: 自動に戻すが自動層へ戻して編集を終了しない')
  }
} else {
  ng.push('#1019: 確認済み状態のヘッダーに自動に戻すがない')
}



await finish(ng, browser)
