// チャプター（CM の目盛り・自動スキップ・境界の前後再生、issue #884）の実ブラウザ判定。
//
// **jsdom では原理的に測れないものだけを見る。** 目盛りの位置は帯の実寸に対する
// 割合で決まり（jsdom の `getBoundingClientRect()` は常に 0）、自動スキップと
// 前後再生は `<video>` の実再生と `currentTime` の推移でしか観測できない。
// CLAUDE.md §テスト規律のとおり、実装より先にここで判定手段を作る。
//
// 見るのは 4 点:
//   ① 目盛りの位置が区間の割合と一致し、cut と ラベル付きが別の見た目で出る
//   ② 通常の再生で cut 区間の先頭に差し掛かると、その終端へ飛ぶ
//   ③ 手動のシークで区間の中に入ったときは飛ばない（cut / ラベル付きの両方）
//   ④ 境界の「前後 3 秒」が境界の手前から始まり、境界を跨いでも飛ばされない
//      （前後再生の間は自動スキップを止める）
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
const CHAPTERS = { source: 'auto', spans: [CM_SPAN, OP_SPAN] }

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
  encodedAssets: [{ profile: 'h264', sizeBytes: 400_000_000 }],
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
// ラベル付き（cut でない）区間は、通常の再生で入っても飛ばない。
await seek(58)
const throughOp = await playFor(4000)
if (throughOp < 61 || throughOp >= 70) {
  ng.push(`③ Cut でない区間（OP）で再生位置が飛んだ（4 秒後の位置 ${throughOp.toFixed(1)} 秒。期待 62 秒付近）`)
}

log('\n=== ④ 境界の「前後 3 秒」 ===')
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

await finish(ng, browser)
