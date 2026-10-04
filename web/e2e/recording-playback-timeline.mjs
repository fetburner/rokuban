// 原本 HLS と非カット MP4 が同じ原本時間軸を使うか、表示中のフレームで測る。
// 期待値はチャプターの軸である非カット MP4（encoded.mp4）の各目印フレームの PTS を
// ffprobe で読んで求める。原本 TS の「目印 PTS - 最早 start_time」は参考値としてログにだけ出す。
// fixture は Go のテストが製品の BuildOriginalVODFFmpegArgs / BuildFFmpegArgs で作る。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-playback-timeline
//   E2E_TIMELINE_CHAPTER_SEEK_ONLY=1 E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
//
// 変異確認: E2E_TIMELINE_EXPECTED_SHIFT_FRAMES=1 を付けると、期待時刻を 1 フレーム
// ずらした判定が落ちる。原本 HLS の offset セッションでは、表示中フレームの境界が
// 「現在位置に合わせる」で保存した PUT の ms と一致するかも見る（live-player.tsx の
// getDisplayedFrameSeconds の起点を整数 offset に戻すと 33 ms ずれて落ちる）。
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, statSync } from 'node:fs'
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

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const RECORDING_ID = 1067
const LIVE_PROFILE = 'hd'
const ENCODE_PROFILE = 'h264'
const SOURCE_FRAME_RATE = 30_000 / 1_001
// 原本 HLS の offset セッションの起点。streamer は -ss を floor(N×30000/1001)×1001/30000 秒へ写す。
const sessionOrigin = (offset) => (offset > 0 ? Math.floor(offset * 30_000 / 1_001) * 1_001 / 30_000 : 0)
// 判定は格子の一致。浮動小数の誤差だけ許す（1 ms 未満。半フレームは 16.7 ms で、ずれを通してしまう）。
const GRID_TOLERANCE_SECONDS = 0.001
const INITIAL_HLS_SEGMENTS = 5
const CHAPTER_SEEK_ONLY = process.env.E2E_TIMELINE_CHAPTER_SEEK_ONLY === '1'
const EXPECTED_SHIFT_FRAMES = Number.parseInt(process.env.E2E_TIMELINE_EXPECTED_SHIFT_FRAMES ?? '0', 10) || 0
const ng = []
const chapterPuts = []
const now = Date.now()
const recordingSeconds = 22
const startedAt = new Date(now - 30_000).toISOString()
const endedAt = new Date(now - 8_000).toISOString()
const recording = {
  id: RECORDING_ID,
  site: SITE,
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '原本 HLS の再生時刻測定',
  description: '製品の引数ビルダーが作った fixture のフレーム時刻を測る。',
  startAt: startedAt,
  durationMs: recordingSeconds * 1000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt,
  endedAt,
  createdAt: '2026-10-03T00:00:00Z',
  encodeProfiles: [ENCODE_PROFILE],
  sizeBytes: 2_000_000,
  encodedAssets: [],
}
const encodedRecording = {
  ...recording,
  encodedAssets: [{ profile: ENCODE_PROFILE, sizeBytes: 1_000_000 }],
}

await validateFixturesOrExit([
  ['original-only recording', ListRecordingsResponseItem, recording],
  ['encoded recording', ListRecordingsResponseItem, encodedRecording],
], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const fixtureDir = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-playback-timeline-'))
const repoRoot = path.resolve(process.cwd(), '..')
log(`\n=== 製品ビルダーで時間軸 fixture を生成 (${fixtureDir}) ===`)
try {
  execFileSync('ffprobe', ['-version'], { stdio: 'ignore' })
  execFileSync('go', ['test', './internal/worker', '-run', '^TestWritePlaybackTimelineFixture$', '-count=1'], {
    cwd: repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ROKUBAN_PLAYBACK_TIMELINE_FIXTURE_DIR: fixtureDir },
  })
} catch (err) {
  ng.push(`Go の製品ビルダーから playback fixture を生成できない: ${err.message}`)
  await finish(ng)
}

const manifest = JSON.parse(readFileSync(path.join(fixtureDir, 'manifest.json'), 'utf8'))
const OFFSET_SECONDS = manifest.offsetSeconds
if (OFFSET_SECONDS !== 10) ng.push(`fixture offset が10秒ではない (${OFFSET_SECONDS})`)
const sourcePath = path.join(fixtureDir, manifest.source)
const encodedPath = path.join(fixtureDir, manifest.encoded)

function runFFprobe(args) {
  return JSON.parse(execFileSync('ffprobe', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }))
}

const probe = runFFprobe([
  '-v', 'error', '-show_streams', '-show_frames',
  '-show_entries', 'format=start_time:stream=codec_type,codec_name,start_time,r_frame_rate:frame=pts_time,best_effort_timestamp_time',
  '-select_streams', 'v:0', '-of', 'json', sourcePath,
])
const allStreams = runFFprobe([
  '-v', 'error', '-show_streams',
  '-show_entries', 'format=start_time:stream=codec_type,codec_name,start_time,r_frame_rate',
  '-of', 'json', sourcePath,
])
const videoStream = allStreams.streams.find((stream) => stream.codec_type === 'video')
const audioStream = allStreams.streams.find((stream) => stream.codec_type === 'audio')
const earliestStart = Math.min(...allStreams.streams.map((stream) => Number(stream.start_time)))
const videoStart = Number(videoStream?.start_time)
const audioStart = Number(audioStream?.start_time)
if (videoStream?.codec_name !== 'mpeg2video') ng.push(`fixture の映像 codec が MPEG-2 ではない (${videoStream?.codec_name})`)
if (videoStream?.r_frame_rate !== '30000/1001') ng.push(`fixture の frame rate が 30000/1001 ではない (${videoStream?.r_frame_rate})`)
if (!(earliestStart > 1)) ng.push(`fixture の先頭 PTS が 0 ではない (${earliestStart})`)
if (!(audioStart < videoStart) || Math.abs((videoStart - audioStart) - 0.7) > 0.06) {
  ng.push(`fixture の音声が映像より約 700 ms 先行していない (audio=${audioStart}, video=${videoStart})`)
}

const decodedFrames = probe.frames ?? []
const encodedTimes = runFFprobe([
  '-v', 'error', '-select_streams', 'v:0',
  '-show_entries', 'frame=pts_time,best_effort_timestamp_time', '-of', 'json', encodedPath,
]).frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time)).sort((a, b) => a - b)
if (encodedTimes.length !== decodedFrames.length) {
  ng.push(`encoded の frame 数が原本と違う (encoded=${encodedTimes.length}, source=${decodedFrames.length})`)
}
const markerTimes = manifest.markerFrames.map((frameIndex, markerSlot) => {
  const frame = decodedFrames[frameIndex]
  const pts = Number(frame?.best_effort_timestamp_time ?? frame?.pts_time)
  if (!Number.isFinite(pts)) {
    ng.push(`ffprobe で目印 frame ${frameIndex} の PTS を得られない`)
    return { frame: frameIndex, markerSlot, pts: Number.NaN, expectedSeconds: Number.NaN }
  }
  // 目印の i 番目の frame は、encoded でも表示順の i 番目（frame 数の一致は下で検査する）。
  const encodedPts = encodedTimes[frameIndex]
  if (!Number.isFinite(encodedPts)) ng.push(`encoded の目印 frame ${frameIndex} の PTS を得られない`)
  return { frame: frameIndex, markerSlot, pts, legacySeconds: pts - earliestStart, expectedSeconds: encodedPts }
})
log(`  ffprobe: fps=${videoStream?.r_frame_rate}, earliest=${earliestStart.toFixed(6)}s, audio lead=${(videoStart - audioStart).toFixed(6)}s`)
log(`  目印 encoded PTS (主判定の基準): ${markerTimes.map((item) => `${item.frame}:${item.expectedSeconds.toFixed(6)}`).join(', ')}`)
log(`  目印 原本 PTS - earliest start_time (参考): ${markerTimes.map((item) => `${item.frame}:${item.legacySeconds.toFixed(6)}`).join(', ')}`)

recording.encodedAssets = [{ profile: ENCODE_PROFILE, sizeBytes: statSync(encodedPath).size }]
const liveRecording = { ...recording, encodedAssets: [] }
let activeRecording = liveRecording
let chaptersForHls = false // 原本 HLS のページでも章を返す（offset セッションの「現在位置に合わせる」判定用）
await validateFixturesOrExit([
  ['recording for original HLS', ListRecordingsResponseItem, liveRecording],
  ['recording for encoded MP4', ListRecordingsResponseItem, recording],
], ng)

const engine = process.env.E2E_BROWSER ?? 'chrome'
log(`\n=== 実ブラウザ: ${engine} ===`)
const browser = engine === 'chrome'
  ? await launchBrowser('chromium', { channel: 'chrome' })
  : await launchBrowser(engine)
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
const playlistRequests = []
const mp4Requests = []

function growingPlaylist(text) {
  const out = []
  let segments = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-ENDLIST')) continue
    if (line.startsWith('#EXTINF:')) segments += 1
    if (segments > INITIAL_HLS_SEGMENTS) break
    out.push(line)
  }
  return out.join('\n') + '\n'
}

await context.addInitScript((markerPixels) => {
  window.__displayedMarkerSlot = () => {
    const video = document.querySelector('video')
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    if (!video || !ctx) return -2
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
    return markerPixels.findIndex(([x, y]) => {
      const pixel = ctx.getImageData(x, y, 1, 1).data
      return pixel[0] > 210 && pixel[1] > 210 && pixel[2] > 210
    })
  }
  window.__timelineMarks = []
  window.__timelineCaptureError = null
  window.__timelineCapture = null
  window.__startTimelineCapture = (sessionOffsetSeconds) => {
    const video = document.querySelector('video')
    if (!video || typeof video.requestVideoFrameCallback !== 'function') {
      window.__timelineCaptureError = 'video or requestVideoFrameCallback is unavailable'
      return false
    }
    const previous = window.__timelineCapture
    if (previous?.video && previous.callbackId !== undefined) {
      previous.video.cancelVideoFrameCallback?.(previous.callbackId)
    }
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    const capture = { video, callbackId: undefined }
    window.__timelineCaptureComplete = false
    window.__timelineCaptureError = null
    const onFrame = (_now, metadata) => {
      try {
        if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.videoWidth > 0 && ctx) {
          ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
          const markerSlot = markerPixels.findIndex(([x, y]) => {
            const pixel = ctx.getImageData(x, y, 1, 1).data
            return pixel[0] > 210 && pixel[1] > 210 && pixel[2] > 210
          })
          if (markerSlot >= 0) {
            window.__timelineMarks.push({
              markerSlot,
              sessionOffsetSeconds,
              mediaTime: metadata.mediaTime,
              presentedFrames: metadata.presentedFrames,
            })
            window.__timelineCaptureComplete = true
            video.pause()
          }
        }
      } catch (err) {
        window.__timelineCaptureError = String(err)
      }
      if (window.__timelineCapture === capture && !window.__timelineCaptureComplete && !window.__timelineCaptureError) {
        capture.callbackId = video.requestVideoFrameCallback(onFrame)
      }
    }
    window.__timelineCapture = capture
    video.muted = true
    capture.callbackId = video.requestVideoFrameCallback(onFrame)
    void video.play().catch((err) => { window.__timelineCaptureError = String(err) })
    return true
  }
}, [
  [16, 9], [32, 9], [48, 9],
  [16, 18], [32, 18], [48, 18],
  [16, 27], [32, 27], [48, 27],
])

async function captureExpectedMarkers(page, sessionOffsetSeconds, expectedMarkers, label) {
  const observed = []
  for (const marker of expectedMarkers) {
    const localTime = marker.expectedSeconds - sessionOrigin(sessionOffsetSeconds)
    const seekTime = Math.max(0, localTime - 0.25)
    try {
      await page.waitForFunction((target) => {
        const video = document.querySelector('video')
        if (!video || video.seekable.length === 0) return false
        for (let i = 0; i < video.seekable.length; i += 1) {
          if (video.seekable.start(i) <= target && video.seekable.end(i) >= target) return true
        }
        return false
      }, seekTime, { timeout: 10_000 })
      await page.locator('video').evaluate(async (video, target) => {
        video.pause()
        video.playbackRate = 0.5
        if (Math.abs(video.currentTime - target) < 0.02) return
        await new Promise((resolve) => {
          let settled = false
          const finish = () => {
            if (settled) return
            settled = true
            clearTimeout(timer)
            resolve()
          }
          const timer = setTimeout(finish, 2000)
          video.addEventListener('seeked', finish, { once: true })
          try {
            video.currentTime = target
          } catch {
            finish()
          }
        })
      }, seekTime)
      const previousCount = await page.evaluate(() => window.__timelineMarks.length)
      await page.evaluate((offset) => window.__startTimelineCapture(offset), sessionOffsetSeconds)
      await page.waitForFunction((count) => (
        window.__timelineMarks.length > count || window.__timelineCaptureError !== null
      ), previousCount, { timeout: 5000 })
      const capture = await page.evaluate((index) => window.__timelineMarks[index], previousCount)
      if (capture) {
        const frame = manifest.markerFrames[capture.markerSlot]
        if (!Number.isInteger(frame)) {
          ng.push(`${label}: unknown marker slot ${capture.markerSlot}`)
        } else {
          observed.push({ ...capture, frame })
        }
      }
      else ng.push(`${label}: frame ${marker.frame} の目印を取得できない (${await page.evaluate(() => window.__timelineCaptureError)})`)
    } catch (err) {
      ng.push(`${label}: frame ${marker.frame} が表示されない (${err.message})`)
    }
  }
  return observed
}

async function seekThroughChapterCards(page) {
  log('\n=== 非カット MP4: 境界カードのシーク ===')
  await page.getByTestId('recording-player-shell').hover()
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.getByRole('menuitem', { name: 'チャプターを直す' }).click()
  const rows = page.getByTestId('chapter-span-row')
  const rowCount = await rows.count()
  if (rowCount !== markerTimes.length) {
    ng.push(`チャプター境界カード数が目印数と違う (got=${rowCount}, want=${markerTimes.length})`)
    return
  }

  for (let index = 0; index < markerTimes.length; index += 1) {
    const marker = markerTimes[index]
    const boundaryMs = Math.round(marker.expectedSeconds * 1000)
    // 押す前は境界の 0.5 秒手前（目印でないフレーム）に置く。整数 ms の境界へ置くと、
    // 切り上がる境界では押す前から目印が映っており、判定が空振りする。
    const beforeSeconds = boundaryMs / 1000 - 0.5
    try {
      await page.locator('video').evaluate((video, target) => {
        video.pause()
        video.currentTime = target
      }, beforeSeconds)
      await page.waitForFunction((target) => {
        const video = document.querySelector('video')
        return video && !video.seeking && Math.abs(video.currentTime - target) < 0.05
      }, beforeSeconds, { timeout: 5000 })
      const slotBefore = await page.evaluate(() => window.__displayedMarkerSlot())
      if (slotBefore === marker.markerSlot) {
        ng.push(`境界カード ${index + 1}: 押す前から frame ${marker.frame} が映っている（判定が空振りする）`)
      }
      await rows.nth(index).locator('button').first().click()
      await page.waitForFunction((slot) => {
        const video = document.querySelector('video')
        return video && !video.seeking && window.__displayedMarkerSlot() === slot
      }, marker.markerSlot, { timeout: 5000 })
      log(`  card ${index + 1}: boundary=${boundaryMs}ms frame=${marker.frame} displayed slot=${marker.markerSlot}`)
    } catch (err) {
      const slot = await page.evaluate(() => window.__displayedMarkerSlot()).catch(() => -2)
      ng.push(`境界カード ${index + 1} のシークで frame ${marker.frame} を表示できない (slot=${slot}, ${err.message})`)
    }
  }
}

// 目印のフレームが表示された瞬間（requestVideoFrameCallback）に一時停止し、「再生位置に合わせる」を
// 押して保存した PUT の ms が、そのフレームの境界になることを見る。行 j の終了境界を目印 j+1 へ
// 合わせる（i を大きい方から回し、1 つの下書きに積む。区間は [目印 j, 目印 j+1] の正当な形）。
// 再生して目印を捉える判定とは別の周回で行う（混ぜると WebKit でタイムアウトした）。
// 原本 HLS の offset セッションでは video の時刻がセッション起点ぶん小さい。first は合わせ始める
// 行 j の下限、offset は video に入れる時刻の補正（セッション起点）。
async function alignBoundariesToPausedFrames(page, label = '非カット MP4', first = 0, offset = 0) {
  log(`\n=== ${label}: 現在位置に合わせる ===`)
  const rows = page.getByTestId('chapter-span-row')
  const expectedStarts = markerTimes.map((marker) => Math.round(marker.expectedSeconds * 1000))
  const alignedFrames = new Map()
  for (let i = markerTimes.length - 1; i >= first + 1; i -= 1) {
    const j = i - 1
    const target = markerTimes[i]
    try {
      await page.locator('video').evaluate((video, seconds) => {
        video.pause()
        video.currentTime = seconds
      }, expectedStarts[j] / 1000 + 0.5 - offset)
      await page.waitForFunction(() => !document.querySelector('video')?.seeking, undefined, { timeout: 5000 })
      await rows.nth(j).locator('button').first().click()
      const paused = await page.locator('video').evaluate((video, [seconds, slot]) => new Promise((resolve) => {
        const timer = setTimeout(() => resolve(false), 8000)
        const onFrame = () => {
          if (window.__displayedMarkerSlot() === slot) {
            video.pause()
            clearTimeout(timer)
            resolve(true)
            return
          }
          video.requestVideoFrameCallback(onFrame)
        }
        video.currentTime = seconds
        video.addEventListener('seeked', () => {
          video.requestVideoFrameCallback(onFrame)
          void video.play()
        }, { once: true })
      }), [target.expectedSeconds - 0.3 - offset, target.markerSlot])
      if (!paused) {
        ng.push(`${label} 現在位置に合わせる: frame ${target.frame} で一時停止できない`)
        continue
      }
      await page.waitForTimeout(300)
      const slot = await page.evaluate(() => window.__displayedMarkerSlot())
      if (slot !== target.markerSlot) {
        ng.push(`${label} 現在位置に合わせる: 一時停止後に frame ${target.frame} が映っていない (slot=${slot})`)
        continue
      }
      await page.getByRole('button', { name: '選択中の境界を現在の再生位置に合わせる' }).click()
      alignedFrames.set(j, target)
    } catch (err) {
      ng.push(`${label} 現在位置に合わせる: frame ${target.frame} の操作に失敗 (${err.message})`)
    }
  }
  const putCountBefore = chapterPuts.length
  await page.getByRole('button', { name: '保存' }).click()
  await page.waitForFunction(() => document.querySelector('[data-testid="chapter-span-row"]') === null, undefined, { timeout: 5000 }).catch(() => {})
  if (chapterPuts.length !== putCountBefore + 1) {
    ng.push(`${label} 現在位置に合わせる: 保存の PUT が 1 回ではない (${chapterPuts.length - putCountBefore})`)
    return
  }
  const spans = chapterPuts.at(-1).spans
  for (const [j, target] of alignedFrames) {
    // encoded の表示順の番号 k（目印 PTS ÷ 1 フレーム）。フィクスチャの原本 frame 番号とは音ずれ補正ぶん違う。
    const k = Math.round(target.expectedSeconds * 30_000 / 1_001)
    const want = Math.round(k * 1001 / 30)
    const got = spans[j]?.endMs
    log(`  span ${j + 1}: PUT endMs=${got} want=${want} (k=${k})`)
    if (got !== want) ng.push(`${label} 現在位置に合わせる: span ${j + 1} の PUT endMs が目印 frame ${target.frame} の境界ではない (got=${got}, want=${want})`)
  }
}

async function timelineHandler({ path: requestPath, url, json, route }) {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers' || requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/live-profiles') return json([{ name: LIVE_PROFILE, height: 720 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([activeRecording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') return json(activeRecording)
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapter-edits` && method === 'PUT') {
    const body = route.request().postDataJSON()
    chapterPuts.push(body)
    return json({ version: 'timeline-v2', detectionPending: false, source: 'user', spans: body.spans })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    const spans = activeRecording.encodedAssets.length > 0 || chaptersForHls
      ? markerTimes.map((marker, index) => ({
        startMs: Math.round(marker.expectedSeconds * 1000),
        endMs: Math.round((marker.expectedSeconds + 1 / SOURCE_FRAME_RATE) * 1000),
        label: `目印 ${index + 1}`,
        cut: false,
      }))
      : []
    return json({ version: 'timeline-v1', detectionPending: false, source: 'auto', spans })
  }
  if (requestPath.startsWith(`/api/recordings/${RECORDING_ID}/`) && method !== 'GET') {
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/media/recordings/${RECORDING_ID}/seek-tiles`) return route.fulfill({ status: 404 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) return route.fulfill({ status: 404 })

  const originalPrefix = `/api/sites/${SITE}/recordings/${RECORDING_ID}/original-vod/`
  if (requestPath.startsWith(originalPrefix)) {
    const relative = requestPath.slice(originalPrefix.length)
    const offsetMatch = relative.match(/^offset\/(\d+)\//)
    const offset = offsetMatch ? Number(offsetMatch[1]) : 0
    const resource = offsetMatch ? relative.slice(offsetMatch[0].length) : relative
    if (resource === 'leave') return route.fulfill({ status: 204 })
    const manifestPath = offset === 0
      ? manifest.hls.offset0
      : offset === OFFSET_SECONDS
        ? manifest.hls.offset10
        : null
    if (!manifestPath) return route.fulfill({ status: 404, body: `fixture missing for offset ${offset}` })
    const outputDir = path.dirname(path.join(fixtureDir, manifestPath))
    if (resource === 'playlist.m3u8') {
      playlistRequests.push({ offset, resource })
      const master = path.join(fixtureDir, manifestPath)
      if (!existsSync(master)) return route.fulfill({ status: 404, body: `fixture missing for offset ${offset}` })
      return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: readFileSync(master) })
    }
    const file = path.join(outputDir, resource)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: `fixture missing: ${resource}` })
    let body = readFileSync(file)
    if (offset === 0 && /^hd\.0\.m3u8$/.test(resource)) body = Buffer.from(growingPlaylist(body.toString('utf8')))
    if (resource.endsWith('.m3u8')) playlistRequests.push({ offset, resource })
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }

  if (requestPath === `/api/media/recordings/${RECORDING_ID}/file`) {
    if (activeRecording.encodedAssets.length === 0) return route.fulfill({ status: 404 })
    const bytes = readFileSync(encodedPath)
    mp4Requests.push(url.href)
    const range = /^bytes=(\d+)-(\d*)$/.exec(route.request().headers().range ?? '')
    if (!range) {
      return route.fulfill({
        status: 200,
        contentType: 'video/mp4',
        body: method === 'HEAD' ? undefined : bytes,
        headers: { 'Accept-Ranges': 'bytes', 'Content-Length': String(bytes.length) },
      })
    }
    const start = Number(range[1])
    const end = Math.min(range[2] ? Number(range[2]) : bytes.length - 1, bytes.length - 1)
    if (start >= bytes.length || end < start) {
      return route.fulfill({ status: 416, headers: { 'Content-Range': `bytes */${bytes.length}` } })
    }
    return route.fulfill({
      status: 206,
      contentType: 'video/mp4',
      body: method === 'HEAD' ? undefined : bytes.subarray(start, end + 1),
      headers: {
        'Accept-Ranges': 'bytes',
        'Content-Range': `bytes ${start}-${end}/${bytes.length}`,
        'Content-Length': String(end - start + 1),
      },
    })
  }
  return json([])
}

function compareMarkers(label, observed, expected, sessionOffset) {
  if (observed.length !== expected.length) {
    ng.push(`${label}: 目印フレーム数が違う (got=${observed.length}, want=${expected.length})`)
  }
  const count = Math.min(observed.length, expected.length)
  for (let i = 0; i < count; i += 1) {
    if (observed[i].markerSlot !== expected[i].markerSlot || observed[i].frame !== expected[i].frame) {
      ng.push(`${label}: 目印フレームの順序が違う (got=${observed[i].frame}, want=${expected[i].frame})`)
    }
    const actual = sessionOrigin(sessionOffset) + observed[i].mediaTime
    const expectedSeconds = expected[i].expectedSeconds + EXPECTED_SHIFT_FRAMES / SOURCE_FRAME_RATE
    const diff = actual - expectedSeconds
    const legacyDiff = actual - expected[i].legacySeconds
    log(`  frame=${observed[i].frame} want=${expected[i].frame} encodedPts=${expectedSeconds.toFixed(6)} actual=${actual.toFixed(6)} diff=${diff >= 0 ? '+' : ''}${(diff * 1000).toFixed(2)}ms (参考: 原本PTS基準 ${legacyDiff >= 0 ? '+' : ''}${(legacyDiff * 1000).toFixed(2)}ms) presentedFrames=${observed[i].presentedFrames}`)
    if (!Number.isFinite(actual) || Math.abs(diff) > GRID_TOLERANCE_SECONDS) {
      ng.push(`${label}: frame ${expected[i].frame} の差が格子の一致（1ms 未満）を外れる (${(diff * 1000).toFixed(2)}ms)`)
    }
  }
}

if (!CHAPTER_SEEK_ONLY) {
  chaptersForHls = true
  const hlsPage = await context.newPage()
  await installApiStubs(hlsPage, timelineHandler)
  await hlsPage.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  await hlsPage.getByTestId('recording-playback-start').click()
  await hlsPage.locator('video').waitFor({ timeout: 15000 })
  await hlsPage.waitForFunction(() => {
    const video = document.querySelector('video')
    return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA
  }, undefined, { timeout: 15000 })

  log('\n=== 原本 HLS offset 0 ===')
  const rootMarkers = markerTimes.slice(0, 4)
  const rootObserved = await captureExpectedMarkers(hlsPage, 0, rootMarkers, '原本 HLS offset 0')
  compareMarkers('原本 HLS offset 0', rootObserved, rootMarkers, 0)

  await hlsPage.locator('video').evaluate((video) => video.pause())
  const seekbar = hlsPage.getByTestId('seek-scrub')
  const seekbarBox = await seekbar.boundingBox()
  if (!seekbarBox) {
    ng.push('offset セッション測定用のシークバーを取得できない')
  } else {
    const targetSeconds = OFFSET_SECONDS + 0.5
    const x = seekbarBox.x + seekbarBox.width * targetSeconds / recordingSeconds
    const y = seekbarBox.y + seekbarBox.height / 2
    await hlsPage.mouse.move(seekbarBox.x + seekbarBox.width * 0.2, y)
    await hlsPage.mouse.down()
    await hlsPage.mouse.move(x, y)
    await hlsPage.mouse.up()
  }

  const offsetRequestDeadline = Date.now() + 15_000
  while (
    !playlistRequests.some((request) => request.offset === OFFSET_SECONDS && request.resource === 'playlist.m3u8') &&
    Date.now() < offsetRequestDeadline
  ) {
    await hlsPage.waitForTimeout(50)
  }
  if (!playlistRequests.some((request) => request.offset === OFFSET_SECONDS && request.resource === 'playlist.m3u8')) {
    ng.push(`seek で原本 HLS offset/${OFFSET_SECONDS} セッションを要求しない (${JSON.stringify(playlistRequests)})`)
  } else {
    const hlsOffsetDeadline = Date.now() + 15_000
    while (Date.now() < hlsOffsetDeadline) {
      const state = await hlsPage.evaluate(() => {
        const video = document.querySelector('video')
        return { videoWidth: video?.videoWidth ?? 0, currentTime: video?.currentTime ?? -1 }
      })
      if (state.videoWidth > 0 && state.currentTime >= 0.25) break
      await hlsPage.waitForTimeout(50)
    }
    log(`\n=== 原本 HLS offset ${OFFSET_SECONDS} ===`)
    const offsetMarkers = markerTimes.slice(4)
    const offsetObserved = await captureExpectedMarkers(hlsPage, OFFSET_SECONDS, offsetMarkers, `原本 HLS offset ${OFFSET_SECONDS}`)
    compareMarkers(`原本 HLS offset ${OFFSET_SECONDS}`, offsetObserved, offsetMarkers, OFFSET_SECONDS)
    // 製品の整数 offset セッション上で、表示フレームの境界が PUT の ms と一致するか。
    // 表示位置 = セッション起点 + mediaTime の起点を壊すとここが落ちる。
    await hlsPage.getByTestId('recording-player-shell').hover()
    await hlsPage.getByRole('button', { name: '再生設定' }).click()
    await hlsPage.getByRole('menuitem', { name: 'チャプターを直す' }).click()
    await alignBoundariesToPausedFrames(hlsPage, `原本 HLS offset ${OFFSET_SECONDS}`, 4, sessionOrigin(OFFSET_SECONDS))
  }

  await hlsPage.evaluate(() => localStorage.clear())
  await hlsPage.close()
  chaptersForHls = false
}
activeRecording = recording
const mp4Page = await context.newPage()
await installApiStubs(mp4Page, timelineHandler)
await mp4Page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
await mp4Page.locator('video').waitFor({ timeout: 15000 })
const mp4Ready = await mp4Page.waitForFunction(() => {
  const video = document.querySelector('video')
  return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_METADATA
}, undefined, { timeout: 15000 }).then(() => true).catch(() => false)
log('\n=== 非カット MP4 ===')
if (mp4Ready) {
  const mp4Observed = await captureExpectedMarkers(mp4Page, 0, markerTimes, '非カット MP4')
  compareMarkers('非カット MP4', mp4Observed, markerTimes, 0)
  // playback mediaTime の全判定を終えてから、UI 経由のシークを別の周回で行う。
  // WebKit の native HLS では、このシークを再生判定の間に挟むと後続が timeout した。
  await seekThroughChapterCards(mp4Page)
  await alignBoundariesToPausedFrames(mp4Page)
} else {
  const videoState = await mp4Page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    duration: video.duration,
    readyState: video.readyState,
    networkState: video.networkState,
    paused: video.paused,
    videoWidth: video.videoWidth,
    videoHeight: video.videoHeight,
    currentSrc: video.currentSrc,
    error: video.error ? { code: video.error.code, message: video.error.message } : null,
  }))
  ng.push(`非カット MP4 の metadata が読めない (video=${JSON.stringify(videoState)}, requests=${JSON.stringify(mp4Requests)})`)
}
if (mp4Requests.length === 0) ng.push('encoded MP4 の Range 配信を要求しない')
if (!CHAPTER_SEEK_ONLY && !playlistRequests.some((request) => request.offset === 0 && request.resource === 'playlist.m3u8')) {
  ng.push('原本 HLS offset 0 の master playlist を要求しない')
}
if (EXPECTED_SHIFT_FRAMES !== 0) log(`  変異: 期待時刻に ${EXPECTED_SHIFT_FRAMES} frame を加算`)
await finish(ng, browser)
