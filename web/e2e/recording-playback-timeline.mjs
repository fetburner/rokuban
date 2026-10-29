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
//   E2E_TIMELINE_API_HLS_ONLY=1 E2E_URL=http://localhost:4173 pnpm e2e:recording-playback-timeline
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
// 目印の 3x3 の位置を、64x36 に縮めたフレームの画素で読む（fixture の drawbox と同じ並び）。
const MARKER_PIXELS = [
  [16, 9], [32, 9], [48, 9],
  [16, 18], [32, 18], [48, 18],
  [16, 27], [32, 27], [48, 27],
]
const INITIAL_HLS_SEGMENTS = 5
const CHAPTER_SEEK_ONLY = process.env.E2E_TIMELINE_CHAPTER_SEEK_ONLY === '1'
const BOUNDARY_SEEK_ONLY = process.env.E2E_TIMELINE_BOUNDARY_SEEK_ONLY === '1'
const RAW_SEEK_ONLY = process.env.E2E_TIMELINE_RAW_SEEK_ONLY === '1'
const RAW_SEEK_DIAGNOSTICS = RAW_SEEK_ONLY || process.env.E2E_TIMELINE_RAW_SEEK_DIAGNOSTICS === '1'
const API_HLS_ONLY = process.env.E2E_TIMELINE_API_HLS_ONLY === '1'
const REAL_HLS_URL = process.env.E2E_TIMELINE_REAL_HLS_URL ?? ''
const REAL_HLS_BOUNDARY_MS = Math.round((Number(process.env.E2E_TIMELINE_REAL_HLS_BOUNDARY_SECONDS ?? '1') || 1) * 1000)
if (REAL_HLS_URL) {
  const realHlsAddress = new URL(REAL_HLS_URL)
  if (realHlsAddress.protocol !== 'http:' || !['127.0.0.1', 'localhost', '[::1]'].includes(realHlsAddress.hostname)) {
    throw new Error('実録画 HLS の診断 URL は localhost のみ指定できます')
  }
}
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
  hlsPlayback: [], cmDetection: { state: 'disabled' },
  startedAt,
  endedAt,
  createdAt: '2026-10-03T00:00:00Z',
  encodeProfiles: [ENCODE_PROFILE],
  sizeBytes: 2_000_000,
  encodedAssets: [],
}
const apiHlsStartedAt = new Date(Date.now() - 30_000).toISOString()
const apiHlsPath = (generation, profile = 'hd') =>
  `/api/media/stores/media/recordings/${RECORDING_ID}/hls/v${generation}/${profile}/master.m3u8`
const apiHlsRecording = {
  ...recording,
  startAt: apiHlsStartedAt,
  startedAt: apiHlsStartedAt,
  durationMs: 120_000,
  endedAt: undefined,
  status: 'recording',
  hlsPlayback: [
    { profile: 'hd', masterUrl: apiHlsPath(1, 'hd') },
    { profile: 'sd', masterUrl: apiHlsPath(1, 'sd') },
  ],
}
// 410 復旧の判定は、録画が終わって取り込み中の録画で、temp を捨てて世代を替えるサーバーの遷移を再現する。
const apiHlsRecoveryFinished = {
  ...apiHlsRecording,
  startAt: new Date(now - 120_000).toISOString(),
  startedAt: new Date(now - 120_000).toISOString(),
  endedAt: new Date(now - 10_000).toISOString(),
  durationMs: 110_000,
  status: 'finished',
  resumePositionMs: 0,
}
const apiHlsRecoveryV1 = {
  ...apiHlsRecoveryFinished,
  ingest: { state: 'transferring', writtenBytes: 1_000_000, observedAt: new Date(now).toISOString() },
}
const apiHlsRecoveryEmpty = { ...apiHlsRecoveryFinished, ingest: { state: 'pending' }, hlsPlayback: [] }
const apiHlsRecoveryV2 = {
  ...apiHlsRecoveryFinished,
  ingest: { state: 'transferring', writtenBytes: 100_000, observedAt: new Date(now).toISOString() },
  hlsPlayback: [
    { profile: 'hd', masterUrl: apiHlsPath(2, 'hd') },
    { profile: 'sd', masterUrl: apiHlsPath(2, 'sd') },
  ],
}
const encodedRecording = {
  ...recording,
  encodedAssets: [{ profile: ENCODE_PROFILE, sizeBytes: 1_000_000 }],
}

await validateFixturesOrExit([
  ['original-only recording', ListRecordingsResponseItem, recording],
  ['encoded recording', ListRecordingsResponseItem, encodedRecording],
  ['recording API HLS', ListRecordingsResponseItem, apiHlsRecording],
  ['recording API HLS v1 before generation change', ListRecordingsResponseItem, apiHlsRecoveryV1],
  ['recording API HLS during generation change', ListRecordingsResponseItem, apiHlsRecoveryEmpty],
  ['recording API HLS v2 after generation change', ListRecordingsResponseItem, apiHlsRecoveryV2],
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
// API_HLS_ONLY は encoded を使わない（期待値は HLS fixture の目印時刻）ので、encoded の生成物は読まない。
const encodedTimes = API_HLS_ONLY ? [] : runFFprobe([
  '-v', 'error', '-select_streams', 'v:0',
  '-show_entries', 'frame=pts_time,best_effort_timestamp_time', '-of', 'json', encodedPath,
]).frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time)).sort((a, b) => a - b)
// API_HLS_ONLY は原本 HLS の fixture だけを使う。原本と encoded のフレーム対応は
// 通常の時間軸判定でだけ必要であり、API HLS 専用判定を別環境の encoder 差で落とさない。
if (!API_HLS_ONLY && encodedTimes.length !== decodedFrames.length) {
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
  if (!API_HLS_ONLY && !Number.isFinite(encodedPts)) ng.push(`encoded の目印 frame ${frameIndex} の PTS を得られない`)
  return { frame: frameIndex, markerSlot, pts, legacySeconds: pts - earliestStart, expectedSeconds: encodedPts }
})
log(`  ffprobe: fps=${videoStream?.r_frame_rate}, earliest=${earliestStart.toFixed(6)}s, audio lead=${(videoStart - audioStart).toFixed(6)}s`)
if (!API_HLS_ONLY) log(`  目印 encoded PTS (主判定の基準): ${markerTimes.map((item) => `${item.frame}:${item.expectedSeconds.toFixed(6)}`).join(', ')}`)
log(`  目印 原本 PTS - earliest start_time (参考): ${markerTimes.map((item) => `${item.frame}:${item.legacySeconds.toFixed(6)}`).join(', ')}`)

// 録画 API HLS の途中シークの期待値は、ブラウザが実際に再生する HLS fixture（offset 0 の master）の
// 目印フレームの時刻にする。HLS は先頭に音声の先行ぶん（約 0.7 秒）のフレームを足すので、原本の
// frame 番号では引けない。そこで縮小した各フレームの目印の画素（__displayedMarkerSlot と同じ位置・閾値）
// で目印を探し、その PTS から variant の最初の映像フレームの PTS を引く。
function hlsFixtureMarkerSeconds() {
  const masterPath = path.join(fixtureDir, manifest.hls.offset0)
  const ptsList = runFFprobe([
    '-v', 'error', '-select_streams', 'v:0',
    '-show_entries', 'frame=pts_time,best_effort_timestamp_time', '-of', 'json', masterPath,
  ]).frames.map((frame) => Number(frame.best_effort_timestamp_time ?? frame.pts_time)).sort((a, b) => a - b)
  const width = 64
  const height = 36
  const gray = execFileSync('ffmpeg', [
    '-v', 'error', '-i', masterPath, '-map', '0:v:0', '-fps_mode', 'passthrough',
    '-vf', `scale=${width}:${height}`, '-f', 'rawvideo', '-pix_fmt', 'gray', '-',
  ], { maxBuffer: 64 * 1024 * 1024 })
  const frameCount = gray.length / (width * height)
  if (frameCount !== ptsList.length) ng.push(`HLS fixture の復号フレーム数が ffprobe と違う (decoded=${frameCount}, probed=${ptsList.length})`)
  const seconds = new Map()
  for (let i = 0; i < frameCount; i += 1) {
    MARKER_PIXELS.forEach(([x, y], slot) => {
      if (gray[i * width * height + y * width + x] > 210 && !seconds.has(slot)) seconds.set(slot, ptsList[i] - ptsList[0])
    })
  }
  return seconds
}
if (API_HLS_ONLY) {
  const hlsMarkerSeconds = hlsFixtureMarkerSeconds()
  for (const marker of markerTimes) {
    marker.hlsSeconds = hlsMarkerSeconds.get(marker.markerSlot) ?? Number.NaN
    if (!Number.isFinite(marker.hlsSeconds)) ng.push(`HLS fixture に目印 ${marker.markerSlot} のフレームが無い`)
  }
  log(`  目印 HLS fixture の時刻 (録画 API HLS の基準): ${markerTimes.map((item) => `${item.frame}:${item.hlsSeconds.toFixed(6)}`).join(', ')}`)
  log(`  HLS fixture - 原本 PTS の差 (ms): ${markerTimes.map((item) => `${item.frame}:${((item.hlsSeconds - item.legacySeconds) * 1000).toFixed(3)}`).join(', ')}`)
}

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
const recordingHlsRequests = []
const recordingHls410Requests = []
const recordingHlsApiResponses = []
// この世代の要求（master・child playlist・セグメント）は全部 410 にする。
let expiredRecordingHlsGeneration = null
// 410 の後の最初の詳細 GET で empty を返し、それ以降は next() を返す（サーバーの v1 → 空 → v2）。
let recordingHlsGap = null
// v2 の master を返す直前に読んだ、止まる直前の再生位置。
let recordingHlsStopPositionSeconds = null

function growingPlaylist(text, segmentLimit = INITIAL_HLS_SEGMENTS) {
  const out = []
  let segments = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-ENDLIST')) continue
    if (line.startsWith('#EXTINF:')) segments += 1
    if (segments > segmentLimit) break
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
  window.__videoFrameFingerprint = () => {
    const video = document.querySelector('video')
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 36
    const ctx = canvas.getContext('2d', { willReadFrequently: true })
    if (!video || !ctx) return null
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
    const pixels = ctx.getImageData(0, 0, canvas.width, canvas.height).data
    let hash = 2_166_136_261
    for (const pixel of pixels) hash = Math.imul(hash ^ pixel, 16_777_619)
    return (hash >>> 0).toString(16).padStart(8, '0')
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
}, MARKER_PIXELS)

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

async function capturePresentedFrameAt(page, action, targetTime) {
  await page.locator('video').evaluate((video) => {
    window.__nextPresentedFrame = new Promise((resolve) => {
      if (typeof video.requestVideoFrameCallback !== 'function') {
        resolve(null)
        return
      }
      let done = false
      let callbackId
      const finish = (frame) => {
        if (done) return
        done = true
        clearTimeout(timer)
        if (callbackId !== undefined) video.cancelVideoFrameCallback?.(callbackId)
        window.__armPresentedFramePaintCapture = undefined
        resolve(frame)
      }
      const snapshot = (mediaTime, source) => ({
        mediaTime,
        source,
        currentTime: video.currentTime,
        paused: video.paused,
        seeking: video.seeking,
        markerSlot: window.__displayedMarkerSlot(),
        fingerprint: window.__videoFrameFingerprint(),
      })
      // 補助経路（seeked 後の二重 rAF）は表示中の目印 slot と fingerprint だけ記録する。mediaTime は
      // 表示フレームの PTS を読めないので NaN にし、currentTime から逆算した値で判定を通さない。
      // rVFC が来なければタイムアウトでこの結果を返し、時刻の判定は NG になる。
      let seeked = false
      let paintSnapshot = null
      const capturePaint = () => {
        requestAnimationFrame(() => requestAnimationFrame(() => {
          if (done || video.seeking) return
          try {
            paintSnapshot = snapshot(Number.NaN, 'paint')
          } catch {
            // 読めなければ補助の記録は無し。
          }
        }))
      }
      const timer = setTimeout(() => finish(paintSnapshot), 5000)
      window.__armPresentedFramePaintCapture = () => {
        if (seeked) capturePaint()
      }
      const captureVisibleFrame = (_now, metadata) => {
        callbackId = undefined
        // seeking 中の callback も採る。Chromium の原本 HLS は目的のフレームの callback が seeking:true で
        // 来て、一時停止中は次が来ない。seek 前の古いフレームで発火しても、mediaTime か目印が合わず NG に
        // なる側に倒れる（誤って通ることはない）。
        try {
          finish(snapshot(metadata.mediaTime, 'rvfc'))
        } catch {
          finish(null)
        }
      }
      // 一時停止中の seek は、seeked より前にフレームが出る実装があるので、callback は action の前に
      // 登録する（seeked から登録すると rVFC を取りこぼす）。
      callbackId = video.requestVideoFrameCallback(captureVisibleFrame)
      video.addEventListener('seeked', () => {
        seeked = true
        capturePaint()
      }, { once: true })
    })
  })
  await action()
  await page.evaluate(() => {
    // rVFC を優先する。一時停止中に次の callback を出さないブラウザでは、seeked 後に落ち着いた
    // 最初の描画を読む。
    window.__armPresentedFramePaintCapture?.()
  })
  const frame = await page.evaluate(() => window.__nextPresentedFrame)
  // captured は rVFC か補助経路で何かを捕捉できたか。タイムアウトなら false。matched は rVFC の
  // mediaTime が targetTime の 1 フレーム内か（補助経路の NaN は常に false）。
  return {
    ...frame,
    captured: frame !== null,
    matched: frame !== null && Number.isFinite(targetTime) &&
      Math.abs(frame.mediaTime - targetTime) <= 1 / (2 * SOURCE_FRAME_RATE) + GRID_TOLERANCE_SECONDS,
  }
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

  await exerciseBoundaryControls(page, '非カット MP4', { first: 0, stop: 2, from: 3 }, 0)
  await page.setViewportSize({ width: 400, height: 800 })
  // 400 幅の帯: 3 つのモードボタンが映像の枠内に収まり、帯の中の要素（再生ボタン・時刻・3 ボタン・速度）
  // 同士が重ならない。
  const modeLayout = await page.getByTestId('chapter-edit-playback-modes').evaluate((element) => {
    const frame = element.closest('[data-testid="recording-player-frame"]')
    if (!frame) return null
    const frameRect = frame.getBoundingClientRect()
    const band = element.parentElement
    // 速度ボタンも含む（`chapter-edit-playback-modes` の中にある）。
    const items = Array.from(band.querySelectorAll('button, [data-testid="chapter-edit-playhead"]'))
      .map((node) => {
        const rect = node.getBoundingClientRect()
        return { name: node.getAttribute('aria-label') ?? node.textContent?.trim() ?? '', left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width }
      })
      .filter((item) => item.width > 0)
    const inside = items.every((item) => item.left >= frameRect.left - 1 && item.right <= frameRect.right + 1)
    const overlaps = []
    for (let i = 0; i < items.length; i += 1) {
      for (let j = i + 1; j < items.length; j += 1) {
        const a = items[i]
        const b = items[j]
        if (a.left < b.right - 1 && b.left < a.right - 1 && a.top < b.bottom - 1 && b.top < a.bottom - 1) {
          overlaps.push(`${a.name} と ${b.name}`)
        }
      }
    }
    return { inside, overlaps, count: items.length }
  })
  if (modeLayout === null || !modeLayout.inside) ng.push('400×800 で再生型のボタンが映像下端の帯からはみ出す')
  else if (modeLayout.overlaps.length > 0) ng.push(`400×800 の帯の中で要素が重なる (${modeLayout.overlaps.join(', ')})`)
  else if (modeLayout.count < 6) ng.push(`400×800 の帯の要素が 6 個（再生・時刻・3 ボタン・速度）に満たない (${modeLayout.count})`)
  await page.setViewportSize({ width: 1280, height: 900 })
}

/**
 * exerciseBoundaryControls は編集モードで、境界の選択・±1 フレーム・「境界まで」「境界から」を
 * 目印フレームの表示で判定する。picks は使う目印の index（選択と ±1 フレーム / 境界まで / 境界から）。
 * offset は原本 HLS のセッション起点（`video.currentTime` = 原本時間 - offset）。非カット MP4 は 0。
 * preroll は押す前に境界の何秒手前へ置くか。利用者は遠い位置から境界を選ぶので、原本 HLS offset 0 は
 * 0.5 秒手前から選んだ後に最初に表示されるフレームも見る。
 */
async function exerciseBoundaryControls(page, label, picks, offset, preroll = 0.5) {
  log(`\n=== ${label}: 境界の選択と1フレーム調整 ===`)
  const video = page.locator('video')
  const toSession = (seconds) => seconds - offset
  const settledOn = (slot) => page.waitForFunction((target) => {
    const el = document.querySelector('video')
    return el && el.paused && !el.seeking && window.__displayedMarkerSlot() === target
  }, slot, { timeout: 5000 })
  const clickBoundary = async (marker) => {
    const ms = Math.round(marker.expectedSeconds * 1000)
    try {
      await page.locator(`[data-testid="chapter-filmstrip-boundary"][data-time-ms="${ms}"]`)
        .evaluate((button) => button.click(), undefined, { timeout: 5000 })
      return true
    } catch {
      const present = await page.$$eval('[data-testid="chapter-filmstrip-boundary"]', (nodes) => nodes.map((n) => n.getAttribute('data-time-ms')))
      ng.push(`${label}: 境界 ${ms}ms のボタンが無い (あるのは ${present.join(',')})`)
      return false
    }
  }
  // 一時停止して目印 slot が映るまで待つ。落ちたら理由を ng に積んで false。
  const T = 1 / SOURCE_FRAME_RATE
  const expectSettled = async (marker, what) => {
    try {
      await settledOn(marker.markerSlot)
      return true
    } catch {
      const state = await page.evaluate(() => {
        const el = document.querySelector('video')
        return { slot: window.__displayedMarkerSlot(), paused: el?.paused, seeking: el?.seeking, t: el?.currentTime }
      }).catch(() => ({}))
      if (engine === 'webkit' && label.startsWith('原本 HLS')) {
        // 判定には使わない。同じ位置への微小 seek で表示が直るかをログに残す。
        await video.evaluate((el) => { el.currentTime += 0.0001 }).catch(() => {})
        await page.waitForTimeout(500)
        const nudged = await page.evaluate(() => {
          const el = document.querySelector('video')
          return { slot: window.__displayedMarkerSlot(), paused: el?.paused, seeking: el?.seeking, t: el?.currentTime }
        }).catch(() => ({}))
        log(`  診断: ${what} の後に追加 +0.0001 → ${JSON.stringify(nudged)}`)
      }
      ng.push(`${label}: ${what} (目印 frame ${marker.frame} slot=${marker.markerSlot} が映らない: ${JSON.stringify(state)})`)
      return false
    }
  }
  await video.evaluate((el) => {
    el.pause()
    el.playbackRate = 1
  })
  const placeBefore = async (marker, distance = preroll) => {
    const target = toSession(marker.expectedSeconds - distance)
    await video.evaluate((el, seconds) => {
      el.pause()
      el.currentTime = seconds
    }, target)
    await page.waitForFunction((t) => {
      const el = document.querySelector('video')
      return el && el.paused && !el.seeking && Math.abs(el.currentTime - t) < 0.05
    }, target, { timeout: 10_000 })
  }
  const firstMarker = markerTimes[picks.first]
  if (label === '原本 HLS offset 0') {
    await placeBefore(firstMarker, 0.5)
    await page.evaluate(() => {
      window.__boundarySelectSeeked = false
      document.querySelector('video')?.addEventListener('seeked', () => {
        window.__boundarySelectSeeked = true
      }, { once: true })
    })
    const center = toSession(encodedTimes[firstMarker.frame] + T / 2)
    const selectedPresented = await capturePresentedFrameAt(
      page,
      () => clickBoundary(firstMarker),
      center,
    )
    if (!selectedPresented.captured || selectedPresented.markerSlot !== firstMarker.markerSlot) {
      ng.push(`${label}: 遠距離境界選択後の最初の表示フレームが frame ${firstMarker.frame} ではない (${JSON.stringify(selectedPresented)})`)
    }
    if (selectedPresented.captured) {
      const selectedSettled = await page.waitForFunction(([target, tolerance]) => {
        const el = document.querySelector('video')
        return el && el.paused && !el.seeking && Math.abs(el.currentTime - target) < tolerance
      }, [center, 0.003], { timeout: 3000 }).then(() => true).catch(() => false)
      const selectSeeked = await page.waitForFunction(() => window.__boundarySelectSeeked, undefined, { timeout: 3000 }).then(() => true).catch(() => false)
      await page.waitForTimeout(500)
      const selectedState = await page.evaluate(() => {
        const el = document.querySelector('video')
        return { slot: window.__displayedMarkerSlot(), currentTime: el?.currentTime, paused: el?.paused, seeking: el?.seeking }
      })
      log(`  遠距離から境界を選択 (0.5s 手前): firstFrame=${JSON.stringify(selectedPresented)}, settled=${selectedSettled}/${selectSeeked}, slot=${selectedState.slot}, t=${selectedState.currentTime?.toFixed(6)}, paused=${selectedState.paused}, seeking=${selectedState.seeking}`)
      if (selectedState.slot !== firstMarker.markerSlot) {
        ng.push(`${label}: 0.5秒手前から境界を選んでも frame ${firstMarker.frame} が表示されない (${JSON.stringify(selectedState)})`)
      }

      await page.evaluate(() => {
        window.__boundaryModeStarted = false
        document.querySelector('video')?.addEventListener('playing', () => {
          window.__boundaryModeStarted = true
        }, { once: true })
      })
      await page.getByRole('button', { name: '選択中の境界まで再生' }).click()
      const modeStarted = await page.waitForFunction(() => window.__boundaryModeStarted, undefined, { timeout: 5000 }).then(() => true).catch(() => false)
      const playedToBoundary = modeStarted && await page.waitForFunction(([target, tolerance]) => {
        const el = document.querySelector('video')
        return window.__boundaryModeStarted && el && el.paused && !el.seeking && Math.abs(el.currentTime - target) < tolerance
      }, [center, 0.003], { timeout: 7000 }).then(() => true).catch(() => false)
      await page.waitForTimeout(500)
      const boundaryModeState = await page.evaluate(() => {
        const el = document.querySelector('video')
        return { slot: window.__displayedMarkerSlot(), currentTime: el?.currentTime, paused: el?.paused, seeking: el?.seeking }
      })
      log(`  遠距離から「境界まで」: playing=${modeStarted}, settled=${playedToBoundary}, slot=${boundaryModeState.slot}, t=${boundaryModeState.currentTime?.toFixed(6)}, paused=${boundaryModeState.paused}, seeking=${boundaryModeState.seeking}`)
      if (!playedToBoundary || boundaryModeState.slot !== firstMarker.markerSlot) {
        ng.push(`${label}: 0.5秒手前から「境界まで」で frame ${firstMarker.frame} に止まらない (${JSON.stringify(boundaryModeState)})`)
      }
    }
  }
  await placeBefore(firstMarker)
  // 隣り合う境界は数 px しか離れない。DOM のボタンを直接 click して隣へ当たらないようにする。
  if (!(await clickBoundary(firstMarker))) return
  await expectSettled(firstMarker, '境界を選んでも一時停止して境界のコマが映らない')
  if (!(await page.getByTestId('chapter-boundary-frame-note').isVisible())) {
    ng.push(`${label}: 境界フレームの表示中に「境界の直後」の説明が出ない`)
  }

  // ±1 フレームは区間の終端の境界（目印の次のフレーム k+1）で測る。1 フレームの区間の開始境界を
  // 進めると区間が空になって消えてしまい、選択が別の境界へ移る。終端なら区間は残る。
  const endMs = Math.round((firstMarker.expectedSeconds + 1 / SOURCE_FRAME_RATE) * 1000)
  await page.locator(`[data-testid="chapter-filmstrip-boundary"][data-time-ms="${endMs}"]`)
    .evaluate((button) => button.click(), undefined, { timeout: 5000 })
    .catch((err) => ng.push(`${label}: 終端の境界 ${endMs}ms を選べない (${err.message})`))
  await page.waitForFunction(() => {
    const el = document.querySelector('video')
    return el && el.paused && !el.seeking
  }, undefined, { timeout: 5000 })
  const frameTime = (index) => encodedTimes[index]
  const expectPresented = async (button, index, what) => {
    const expectedTime = frameTime(index) - offset
    const presented = await capturePresentedFrameAt(
      page,
      () => button.click(),
      expectedTime,
    )
    log(`  SRC ${label} ${what} ${presented.source}`)
    const presentedTime = presented.mediaTime
    if (!Number.isFinite(presentedTime) || !Number.isFinite(frameTime(index)) || Math.abs(presentedTime + offset - frameTime(index)) > GRID_TOLERANCE_SECONDS) {
      ng.push(`${label}: ${what} (frame=${JSON.stringify(presented)}, want=${frameTime(index)}, index=${index})`)
    }
  }
  await expectPresented(
    page.getByRole('button', { name: '選択中の境界を 1 フレーム進める', exact: true }),
    firstMarker.frame + 2,
    '+1フレーム後に隣のフレームへ seek しない',
  )
  await expectPresented(
    page.getByRole('button', { name: '選択中の境界を1フレーム戻す', exact: true }),
    firstMarker.frame + 1,
    '-1フレーム後に元のフレームへ seek しない',
  )

  // 長押しの連続送り（初回遅延 450ms + 200ms 間隔）。押している間にシークが詰まらないか、離した後に
  // 最後の境界のコマへ着地するか、元に戻す 1 回で押す前へ戻るかを測る。
  const selectedText = () => page.getByTestId('chapter-selected-boundary').textContent()
  const msOf = (text) => {
    const m = /^(\d+):(\d\d)\.(\d{3})$/.exec((text ?? '').trim())
    return m ? (Number(m[1]) * 60 + Number(m[2])) * 1000 + Number(m[3]) : Number.NaN
  }
  const beforeHold = await selectedText()
  await video.evaluate((el) => {
    window.__seekLog = []
    const mark = (type) => window.__seekLog.push({ type, at: performance.now() })
    el.addEventListener('seeking', () => mark('seeking'))
    el.addEventListener('seeked', () => mark('seeked'))
  })
  const holdButton = page.getByRole('button', { name: '選択中の境界を 1 フレーム進める', exact: true })
  const holdBox = await holdButton.boundingBox()
  if (!holdBox) {
    ng.push(`${label}: 長押しのボタンの位置を取れない`)
  } else {
    await page.mouse.move(holdBox.x + holdBox.width / 2, holdBox.y + holdBox.height / 2)
    await page.mouse.down()
    await page.waitForTimeout(1500)
    const releasedAt = await page.evaluate(() => performance.now())
    await page.mouse.up()
    await page.waitForFunction(() => {
      const el = document.querySelector('video')
      return el && el.paused && !el.seeking
    }, undefined, { timeout: 5000 })
    await page.waitForTimeout(300)
    const afterHold = await selectedText()
    const log2 = await page.evaluate(() => window.__seekLog)
    const seekings = log2.filter((e) => e.type === 'seeking').length
    const seekeds = log2.filter((e) => e.type === 'seeked').length
    const lastSeeked = log2.filter((e) => e.type === 'seeked').at(-1)?.at ?? Number.NaN
    // 各 seeking から次の seeked までの時間の最大（詰まり）。
    let maxSeekMs = 0
    for (let i = 0; i < log2.length; i += 1) {
      if (log2[i].type !== 'seeking') continue
      const done = log2.slice(i + 1).find((e) => e.type === 'seeked')
      if (done) maxSeekMs = Math.max(maxSeekMs, done.at - log2[i].at)
    }
    const heldFrames = Math.round((msOf(afterHold) - msOf(beforeHold)) / (T * 1000))
    log(`  長押し 1.5 秒: 境界 ${beforeHold} -> ${afterHold} (${heldFrames} フレーム) seeking=${seekings} seeked=${seekeds} 最大シーク時間=${maxSeekMs.toFixed(0)}ms 離してから最後の seeked まで=${(lastSeeked - releasedAt).toFixed(0)}ms`)
    // 初回 1 回 + 450ms 後から 200ms ごと（1.5 秒で 6 回）= 7 回前後。クリック 1 回分（1）や押した全時間の
    // 連打（200ms 無しで 30 回超）ではない。
    if (heldFrames < 5 || heldFrames > 8) ng.push(`${label}: 長押し 1.5 秒の送りが 5〜8 フレームでない (${heldFrames})`)
    const finalFrame = Math.round(msOf(afterHold) / (T * 1000))
    const settled = await page.evaluate(([center, tolerance]) => Math.abs(document.querySelector('video').currentTime - center) < tolerance,
      [toSession((finalFrame + 0.5) * T), 0.003])
    if (!settled) {
      const t = await video.evaluate((el) => el.currentTime)
      ng.push(`${label}: 長押しを離した後、最後の境界 ${afterHold} のコマ中央に着地しない (currentTime=${t})`)
    }
    await page.getByRole('button', { name: '元に戻す', exact: true }).click()
    const afterUndo = await selectedText()
    if (afterUndo !== beforeHold) {
      // 元に戻すは選択を動かさないので、境界が押す前の位置へ戻ったかは filmstrip の境界で見る。
      const stillThere = await page.locator(`[data-testid="chapter-filmstrip-boundary"][data-time-ms="${msOf(beforeHold)}"]`).count()
      if (stillThere !== 1) ng.push(`${label}: 長押しを元に戻す 1 回で押す前の境界 ${beforeHold} に戻らない`)
    }
  }

  log(`\n=== ${label}: 境界まで / 境界から ===`)
  const stopMarker = markerTimes[picks.stop]
  await placeBefore(stopMarker)
  if (!(await clickBoundary(stopMarker))) return
  if (!(await expectSettled(stopMarker, '境界を選んでも一時停止して境界のコマが映らない'))) return
  await video.evaluate((el) => {
    window.__boundaryModeStarted = false
    el.addEventListener('playing', () => {
      window.__boundaryModeStarted = true
    }, { once: true })
  })
  await page.getByRole('button', { name: '選択中の境界まで再生' }).click()
  const modeStarted = await page.waitForFunction(() => window.__boundaryModeStarted, undefined, { timeout: 5000 }).then(() => true).catch(() => false)
  if (!modeStarted) ng.push(`${label}: 「境界まで」が再生を始めない`)
  await expectSettled(stopMarker, '「境界まで」が境界のコマで止まらない')

  const fromMarker = markerTimes[picks.from]
  await placeBefore(fromMarker)
  if (!(await clickBoundary(fromMarker))) return
  if (!(await expectSettled(fromMarker, '境界を選んでも一時停止して境界のコマが映らない'))) return
  // 開始位置は play() を呼んだ瞬間の currentTime で測る（play イベントの時点では再生が数 ms 進みうる）。
  await video.evaluate((el) => {
    window.__playCalls = []
    const original = el.play.bind(el)
    el.play = () => {
      window.__playCalls.push(el.currentTime)
      return original()
    }
  })
  await page.getByRole('button', { name: '選択中の境界から再生' }).click()
  const playCalls = await page.evaluate(() => window.__playCalls)
  await video.evaluate((el) => { delete el.play })
  const actualPlayStart = (playCalls.at(-1) ?? Number.NaN) + offset
  const fromFrameStart = encodedTimes[fromMarker.frame]
  const fromFrameEnd = encodedTimes[fromMarker.frame + 1] ?? (fromFrameStart + 1 / SOURCE_FRAME_RATE)
  if (!(actualPlayStart >= fromFrameStart && actualPlayStart < fromFrameEnd)) {
    ng.push(`${label}: 「境界から」の再生開始が選択フレーム内でない (start=${actualPlayStart}, frame=[${fromFrameStart}, ${fromFrameEnd}))`)
  }
  const modeCompletion = await video.evaluate((el, boundarySession) => new Promise((resolve) => {
    const timer = setTimeout(() => resolve(false), 8000)
    const check = () => {
      if (el.paused && !el.seeking && el.currentTime >= boundarySession + 2.9) {
        clearTimeout(timer)
        resolve(true)
        return
      }
      requestAnimationFrame(check)
    }
    check()
  }), toSession(fromMarker.expectedSeconds))
  if (!modeCompletion) ng.push(`${label}: 「境界から」が境界から約 3 秒で停止しない`)
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
        video.addEventListener('seeked', () => {
          video.requestVideoFrameCallback(onFrame)
          void video.play()
        }, { once: true })
        video.currentTime = seconds
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
  const saveButton = page.getByRole('button', { name: '保存' })
  if (!await saveButton.isEnabled()) {
    ng.push(`${label} 現在位置に合わせる: 保存できる境界変更が無い`)
    return
  }
  await saveButton.click()
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
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0, awaitingReview: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([activeRecording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') {
    if (recordingHlsGap?.armed) {
      const { empty, next } = recordingHlsGap
      recordingHlsGap = null
      activeRecording = next()
      recordingHlsApiResponses.push('empty')
      return json(empty)
    }
    if (recordingHlsGap) recordingHlsApiResponses.push('v1')
    else if (activeRecording.hlsPlayback.some(({ masterUrl }) => masterUrl === apiHlsPath(2, 'hd'))) recordingHlsApiResponses.push('v2')
    return json(activeRecording)
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapter-edits` && method === 'PUT') {
    const body = route.request().postDataJSON()
    chapterPuts.push(body)
    return json({ version: 'timeline-v2', detectionPending: false, source: 'user', spans: body.spans })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    const spans = REAL_HLS_URL && chaptersForHls
      ? [{ startMs: REAL_HLS_BOUNDARY_MS, endMs: REAL_HLS_BOUNDARY_MS + 1000, label: '検証境界', cut: false }]
      : activeRecording.encodedAssets.length > 0 || chaptersForHls
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

  const apiHlsMatch = new RegExp(`^/api/media/stores/media/recordings/${RECORDING_ID}/hls/v(\\d+)/([^/]+)/(.+)$`).exec(requestPath)
  if (apiHlsMatch) {
    const generation = Number(apiHlsMatch[1])
    const profile = apiHlsMatch[2]
    const resource = apiHlsMatch[3]
    const fixtureMaster = path.join(fixtureDir, manifest.hls.offset0)
    const outputDir = path.dirname(fixtureMaster)
    recordingHlsRequests.push({ url: requestPath, resource, generation, profile })
    if (generation === expiredRecordingHlsGeneration) {
      recordingHls410Requests.push(requestPath)
      if (recordingHlsGap) recordingHlsGap.armed = true
      return route.fulfill({ status: 410, body: 'expired recording HLS generation' })
    }
    if (generation === 2 && resource === 'master.m3u8' && recordingHlsStopPositionSeconds === null) {
      // v2 の映像はまだ届いていないので、ここで読む位置は v1 で止まる直前の再生位置である。
      recordingHlsStopPositionSeconds = await route.request().frame()
        .evaluate(() => window.__lastPlayingSeconds ?? null).catch(() => null)
    }
    if (resource === 'master.m3u8') {
      return route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: readFileSync(fixtureMaster),
      })
    }
    const file = path.join(outputDir, resource)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: `fixture missing: ${resource}` })
    let body = readFileSync(file)
    // 取り込み中の EVENT playlist として、映像も音声も ENDLIST を外す。v2 は全セグメントを載せる。
    // WebKit は v1 のバッファの終わり（約 10 秒）で止まるので、その先が要る。
    if (/^hd\.\d+\.m3u8$/.test(resource)) {
      body = Buffer.from(growingPlaylist(body.toString('utf8'), generation === 2 ? Infinity : INITIAL_HLS_SEGMENTS))
    }
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }

  const originalPrefix = `/api/sites/${SITE}/recordings/${RECORDING_ID}/original-vod/`
  if (requestPath.startsWith(originalPrefix)) {
    const relative = requestPath.slice(originalPrefix.length)
    const offsetMatch = relative.match(/^offset\/(\d+)\//)
    const offset = offsetMatch ? Number(offsetMatch[1]) : 0
    const resource = offsetMatch ? relative.slice(offsetMatch[0].length) : relative
    if (resource === 'leave') return route.fulfill({ status: 204 })
    if (REAL_HLS_URL) {
      const remoteURL = resource === 'playlist.m3u8'
        ? new URL(REAL_HLS_URL)
        : new URL(resource, REAL_HLS_URL)
      remoteURL.searchParams.delete('profile')
      const response = await route.fetch({ url: remoteURL.href })
      return route.fulfill({ response })
    }
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

async function stubRecordingEventSource(page) {
  await page.addInitScript(() => {
    const eventSources = []
    const seekedPositions = []
    const observedVideos = new WeakSet()
    const observeVideo = (node) => {
      if (!(node instanceof HTMLVideoElement) || observedVideos.has(node)) return
      observedVideos.add(node)
      node.addEventListener('seeked', () => seekedPositions.push(node.currentTime))
      let previous = null
      window.__progressedSeconds = 0
      node.addEventListener('timeupdate', () => {
        if (!node.paused && node.currentTime > 0) window.__lastPlayingSeconds = node.currentTime
        // 再生で進んだ秒だけを足す（シークの飛びは足さない）。
        const step = previous === null ? 0 : node.currentTime - previous
        if (!node.paused && step > 0 && step < 0.5) window.__progressedSeconds += step
        previous = node.currentTime
      })
    }
    class StubEventSource extends EventTarget {
      constructor(url) {
        super()
        this.url = url
        eventSources.push(this)
      }
      close() {}
    }
    window.EventSource = StubEventSource
    window.__recordingEventSources = eventSources
    window.__recordingHlsSeekedPositions = seekedPositions
    window.__sendRecordingEvent = (topic) => {
      for (const source of eventSources) source.dispatchEvent(new Event(topic))
    }
    const observer = new MutationObserver(() => {
      document.querySelectorAll('video').forEach(observeVideo)
    })
    observer.observe(document, { childList: true, subtree: true })
    document.querySelectorAll('video').forEach(observeVideo)
  })
}

async function runRecordingApiHlsCheck() {
  // 録画開始は実行時に決める。時間軸の右端は録画済み時間なので、CI の起動の遅れで長くなると
  // 1 px が 1 フレームより長くなり、途中シークで窓に入る整数 px が無くなる。EVENT playlist の
  // seekable（約 10 秒）より 2 秒以上長ければ足りる。
  const apiHlsStartedAtNow = new Date(Date.now() - 13_000).toISOString()
  const apiHlsLiveRecording = { ...apiHlsRecording, startAt: apiHlsStartedAtNow, startedAt: apiHlsStartedAtNow }
  activeRecording = apiHlsLiveRecording
  recordingHlsRequests.length = 0
  playlistRequests.length = 0
  const page = await context.newPage()
  await installApiStubs(page, timelineHandler)
  await stubRecordingEventSource(page)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  await page.getByTestId('recording-playback-start').click()
  await page.locator('video').waitFor({ timeout: 15000 })

  const mediaReady = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.seekable.length > 0
  }, undefined, { timeout: 20000 }).then(() => true).catch(() => false)
  if (!mediaReady) {
    ng.push(`録画 API HLS の映像データが届かない (requests=${JSON.stringify(recordingHlsRequests)})`)
    await page.close()
    return
  }

  // max は Date.now() - startedAt なので、複数の中間位置を調べる間も 1 秒ごとに
  // 伸びる。メディア準備後の同じ時点で browser / Node の基準時刻を固定し、座標の
  // 算出とシーク後の検査で px → 秒の対応を保つ。timer は進むので再生待ちには影響しない。
  const timelineAsOf = Date.now()
  await page.clock.setFixedTime(new Date(timelineAsOf))

  const slider = page.getByRole('slider', { name: 'シークバー' })
  const timelineSeconds = Number(await slider.getAttribute('aria-valuemax'))
  const mediaState = await page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    seekableEnd: video.seekable.end(video.seekable.length - 1),
    readyState: video.readyState,
    videoWidth: video.videoWidth,
  }))
  const recordedProgressSeconds = (timelineAsOf - Date.parse(apiHlsStartedAtNow)) / 1000
  log(`\n=== 録画 API HLS: 録画中の時間軸 ===`)
  log(`  右端=${timelineSeconds.toFixed(2)}s / 壁時計の録画済み=${recordedProgressSeconds.toFixed(2)}s / HLS seekable=${mediaState.seekableEnd.toFixed(2)}s`)
  if (Math.abs(timelineSeconds - recordedProgressSeconds) > 2.5) {
    ng.push(`録画中の時間軸の右端が録画済み時間ではない (axis=${timelineSeconds}, recorded=${recordedProgressSeconds})`)
  }
  if (!(timelineSeconds > mediaState.seekableEnd + 2)) {
    ng.push(`録画中の時間軸が EVENT playlist の末尾で止まっている (axis=${timelineSeconds}, seekable=${mediaState.seekableEnd})`)
  }
  if (!recordingHlsRequests.some(({ url }) => url === apiHlsPath(1))) {
    ng.push(`Recording.hlsPlayback.masterUrl を直接要求しない (${JSON.stringify(recordingHlsRequests)})`)
  }

  // 途中の位置へのシーク: 時間軸を整数 px でクリックし、currentTime が録画開始からの秒になり、
  // 表示中のフレームがその秒のフレーム（目印）であることを測る。WebKit は mouse.click の座標を
  // 整数 px に切り捨て、1 px が 1 フレーム以上（35 ms 前後）なので、小数座標で 1 フレームを狙えない。
  // そこで、整数 px のうち、アプリが代入する秒 (x - box.x) / width * axisMax が目印フレームの窓
  // （HLS fixture の目印フレームの時刻から 1 フレーム。hlsFixtureMarkerSeconds）の中に入るものを選ぶ。
  // 秒の計算は Chrome と WebKit で同じで、encoded には依存しない。
  await page.locator('video').evaluate((video) => video.pause())
  const box = await slider.boundingBox()
  const axisMax = Number(await slider.getAttribute('aria-valuemax'))
  const frameSeconds = 1 / SOURCE_FRAME_RATE
  // 窓の端を避ける余白。時間軸が 1 秒ごとに伸びて axisMax が変わる分を、上下とも同じ幅で吸収する。
  const margin = 0.006
  const picks = []
  for (const marker of markerTimes) {
    if (picks.length === 2) break
    if (!(marker.hlsSeconds < mediaState.seekableEnd - 1)) continue
    for (let x = Math.ceil(box.x); x < box.x + box.width; x += 1) {
      const seconds = (x - box.x) / box.width * axisMax
      if (seconds >= marker.hlsSeconds + margin && seconds < marker.hlsSeconds + frameSeconds - margin) {
        picks.push({ marker, x, seconds })
        break
      }
    }
  }
  // 1 px が 1 フレームより長い（CI は起動が遅く時間軸が 50 秒近くなり、約 47 ms）と、窓に入る px が無い
  // 目印がある。seekable 内の 4 つの目印のうち 1 つでも見つかれば判定できる。
  if (picks.length < 1) ng.push(`途中シークの判定に使える整数 px が見つからない (picks=${picks.length}, width=${box.width}, axisMax=${axisMax})`)
  for (const { marker, x, seconds } of picks) {
    await page.mouse.click(x, box.y + box.height / 2)
    const landed = await page.waitForFunction(({ slot, from, to }) => {
      const video = document.querySelector('video')
      return video && !video.seeking && video.currentTime >= from && video.currentTime < to &&
        window.__displayedMarkerSlot() === slot
    }, { slot: marker.markerSlot, from: marker.hlsSeconds, to: marker.hlsSeconds + frameSeconds }, { timeout: 5000 })
      .then(() => true).catch(() => false)
    const observed = await page.evaluate(() => ({
      currentTime: document.querySelector('video').currentTime,
      slot: window.__displayedMarkerSlot(),
    }))
    log(`  中間シーク frame ${marker.frame}: x=${x} 目標=${seconds.toFixed(4)}s / currentTime=${observed.currentTime.toFixed(4)}s / 表示=${observed.slot} (期待 ${marker.markerSlot})`)
    if (!landed) {
      // 診断: 同じ位置へ再 seek して表示が直るか（WebKit の native HLS の既知の問題と同じ現象かの見分け）。
      const again = await page.locator('video').evaluate(async (video) => {
        const t = video.currentTime
        video.pause()
        await new Promise((resolve) => {
          video.addEventListener('seeked', resolve, { once: true })
          video.currentTime = t
          setTimeout(resolve, 3000)
        })
        await new Promise((resolve) => setTimeout(resolve, 500))
        return { t, now: video.currentTime, slot: window.__displayedMarkerSlot() }
      })
      log(`  診断 frame ${marker.frame}: 同じ位置へ再 seek 後 ${JSON.stringify(again)}`)
      ng.push(`途中へのシークで currentTime が録画秒にならない、または表示が目印 ${marker.frame} ではない (${JSON.stringify(observed)}, want=${marker.hlsSeconds})`)
    }
  }

  // 末尾へのシーク。これは「ブラウザ込みで、末尾への操作が playlist の末尾に収まる」ことだけを測る。
  // 変異確認で seekTarget の playlistEndSeconds による丸めを外しても通った（HTML のシーク手順で
  // ブラウザが seekable の最寄りへ丸める）ので、アプリの丸めはこの判定では守れていない。
  // アプリの丸めは playback-mode.test.ts の clamp のテストが守る。
  await slider.focus()
  await page.keyboard.press('End')
  const clamped = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    if (!video || video.seekable.length === 0) return false
    const end = video.seekable.end(video.seekable.length - 1)
    return Math.abs(video.currentTime - end) < 0.5
  }, undefined, { timeout: 5000 }).then(() => true).catch(() => false)
  if (!clamped) {
    const afterSeek = await page.locator('video').evaluate((video) => ({
      currentTime: video.currentTime,
      seeking: video.seeking,
      seekableEnd: video.seekable.length ? video.seekable.end(video.seekable.length - 1) : null,
    }))
    ng.push(`時間軸の末尾への操作で currentTime が playlist 末尾に収まらない (${JSON.stringify(afterSeek)})`)
  }
  if (playlistRequests.length > 0) ng.push(`録画 API HLS が従来の原本 VOD URL へ逸れる (${JSON.stringify(playlistRequests)})`)

  const masterRequestsBeforeCommit = recordingHlsRequests.filter(({ resource }) => resource === 'master.m3u8').length
  activeRecording = {
    ...apiHlsLiveRecording,
    status: 'finished',
    endedAt: new Date().toISOString(),
    resumePositionMs: 10_000,
    // ingest commit 後も API が返す master URL は維持される。
    hlsPlayback: apiHlsRecording.hlsPlayback,
  }
  const eventSourceCount = await page.evaluate(() => window.__recordingEventSources.length)
  if (eventSourceCount === 0) ng.push('録画状態の SSE を購読していない')
  const refreshed = page.waitForResponse((response) => {
    const responseURL = new URL(response.url())
    return response.request().method() === 'GET' && responseURL.pathname === `/api/recordings/${RECORDING_ID}`
  }, { timeout: 10000 })
  await page.evaluate(() => window.__sendRecordingEvent('recordings'))
  const refreshedSuccessfully = await refreshed.then((response) => response.ok()).catch(() => false)
  if (!refreshedSuccessfully) ng.push('commit 後の詳細 API を取り直せない')
  await page.waitForTimeout(250)
  const masterRequestsAfterCommit = recordingHlsRequests.filter(({ resource }) => resource === 'master.m3u8').length
  log(`  commit 前後の master 要求数=${masterRequestsBeforeCommit}/${masterRequestsAfterCommit}`)
  if (masterRequestsAfterCommit !== masterRequestsBeforeCommit) {
    ng.push('ingest commit をまたぐとき、同じ master URL を維持した再生を張り直した')
  }

  await page.evaluate(() => localStorage.clear())
  await page.close()
}

async function runRecordingApiHlsRecoveryCheck() {
  // サーバーの世代は v1 → 空 → v2 と動く。temp を捨てると進捗行が消えて hlsPlayback が空・ingest が
  // pending になり、次の試行の進捗行ができると v2 になる。この遷移に SSE は来ないので、合図は送らない。
  const fresh = () => new Date().toISOString()
  const v1 = { ...apiHlsRecoveryV1, ingest: { ...apiHlsRecoveryV1.ingest, observedAt: fresh() } }
  activeRecording = v1
  recordingHlsRequests.length = 0
  recordingHls410Requests.length = 0
  recordingHlsApiResponses.length = 0
  playlistRequests.length = 0
  expiredRecordingHlsGeneration = null
  recordingHlsStopPositionSeconds = null
  recordingHlsGap = {
    armed: false,
    empty: apiHlsRecoveryEmpty,
    next: () => ({ ...apiHlsRecoveryV2, ingest: { ...apiHlsRecoveryV2.ingest, observedAt: fresh() } }),
  }
  const page = await context.newPage()
  const otherSourceRequests = []
  page.on('request', (request) => {
    const { pathname } = new URL(request.url())
    if (pathname.includes('/chase/') || pathname.includes('/original-vod/')) otherSourceRequests.push(pathname)
  })
  await installApiStubs(page, timelineHandler)
  await stubRecordingEventSource(page)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  log(`\n=== 録画 API HLS: v1 → 空 → v2 の世代の切り替えで、止まる直前の位置から続ける ===`)

  await page.getByTestId('recording-playback-start').waitFor({ timeout: 15000 })
  await page.getByTestId('recording-playback-start').click()
  // currentTime だけを見ると、WebKit が EVENT playlist の端から始めて開始位置へ戻す前の値（約 4 秒）で
  // 通ってしまう。再生で実際に 3 秒以上進んだことを待つ。
  const started = await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video && !video.paused && window.__progressedSeconds >= 3
  }, undefined, { timeout: 15000 }).then(() => true).catch(() => false)
  if (!started) {
    ng.push('録画 API HLS の 410 復旧判定前に実ブラウザ再生が始まらない')
    await page.close()
    return
  }
  const sessionStartSeconds = apiHlsRecoveryV1.resumePositionMs / 1000
  // 応答を待つ（要求の時点では、止まる直前の位置を読む handler がまだ終わっていない）。
  const nextMaster = page.waitForResponse((response) => new URL(response.url()).pathname === apiHlsPath(2, 'hd'), { timeout: 30000 })
    .then(() => true).catch(() => false)
  const seekedStart = await page.evaluate(() => window.__recordingHlsSeekedPositions.length)
  // ここから v1 の master・child playlist・セグメントは全部 410 を返し続ける。
  expiredRecordingHlsGeneration = 1
  const expiredAt = Date.now()

  // 待つのは 30 秒まで。lib/events.ts の 60 秒 invalidate が v2 を拾う前に打ち切り、停止検知と
  // 410 からの再取得の経路だけで v2 へ移れることを測る。
  const v2Loaded = await nextMaster
  log(`  410 開始から v2 master 要求まで=${((Date.now() - expiredAt) / 1000).toFixed(1)}s / 詳細 API の応答=${JSON.stringify(recordingHlsApiResponses)}`)
  if (recordingHls410Requests.length < 1) {
    ng.push(`再生中の v1 が 410 にならない (${JSON.stringify(recordingHls410Requests)})`)
  }
  if (!v2Loaded) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused })).catch(() => null)
    ng.push(`410 の後 30 秒以内に v2/hd master を要求しない (video=${JSON.stringify(current)}, api=${JSON.stringify(recordingHlsApiResponses)}, 410=${recordingHls410Requests.length})`)
    await page.close()
    return
  }
  if (recordingHlsApiResponses.indexOf('empty') < 0 || recordingHlsApiResponses.indexOf('v2') < recordingHlsApiResponses.indexOf('empty')) {
    ng.push(`詳細 API が v1 → 空 → v2 の順に返っていない (${JSON.stringify(recordingHlsApiResponses)})`)
  }
  const stopSeconds = recordingHlsStopPositionSeconds
  if (typeof stopSeconds !== 'number' || !(stopSeconds - sessionStartSeconds > 1)) {
    ng.push(`止まる直前の位置がセッション開始と見分けられない (stop=${stopSeconds}, sessionStart=${sessionStartSeconds})`)
    await page.close()
    return
  }
  const resumedAt = await page.waitForFunction(({ seekedStart }) => {
    const video = document.querySelector('video')
    if (!video || video.paused || video.seeking || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return false
    // 作り直しで video 要素が替わるので、最初の再生位置は seeked の記録から取る。
    return window.__recordingHlsSeekedPositions.slice(seekedStart).at(-1) ?? video.currentTime
  }, { seekedStart }, { timeout: 10000 })
    .then((handle) => handle.jsonValue())
    .catch(() => null)
  log(`  セッション開始=${sessionStartSeconds.toFixed(2)}s / 止まる直前=${stopSeconds.toFixed(2)}s / v2 の再開位置=${typeof resumedAt === 'number' ? resumedAt.toFixed(2) : resumedAt}s`)
  if (typeof resumedAt !== 'number' || Math.abs(resumedAt - stopSeconds) > 1) {
    const current = await page.locator('video').evaluate((video) => ({ currentTime: video.currentTime, paused: video.paused }))
    ng.push(`v2 で止まる直前の位置（±1 秒）から再開しない (stop=${stopSeconds}, resumed=${resumedAt}, current=${JSON.stringify(current)})`)
    await page.close()
    return
  }
  const playingContinued = await page.waitForFunction((from) => {
    const video = document.querySelector('video')
    return video && !video.paused && video.currentTime > from + 0.5
  }, resumedAt, { timeout: 10000 }).then(() => true).catch(() => false)
  if (!playingContinued) ng.push('410 後に v2 で再開位置から再生が続かない')
  if (otherSourceRequests.length > 0) ng.push(`世代の切り替え中に追っかけ・原本 HLS へ逸れる (${JSON.stringify(otherSourceRequests)})`)
  if (playlistRequests.length > 0) ng.push(`世代の切り替え中に従来の原本 VOD URL へ逸れる (${JSON.stringify(playlistRequests)})`)
  expiredRecordingHlsGeneration = null
  recordingHlsGap = null
  await page.evaluate(() => localStorage.clear())
  await page.close()
}

if (API_HLS_ONLY) {
  await runRecordingApiHlsCheck()
  await runRecordingApiHlsRecoveryCheck()
  await finish(ng, browser)
}

async function runRawSeekDiagnostics() {
  if (REAL_HLS_URL) return runRealRecordingSeekDiagnostics()
  const marker = markerTimes.find((item) => item.frame === 45)
  const label = '原本 HLS offset 0'

  chaptersForHls = true
  log(`\n=== ${engine} 原本 HLS の素の currentTime seek 診断 (編集モード、frame ${marker.frame}; 連続再生で時刻を校正) ===`)
  const page = await context.newPage()
  await installApiStubs(page, timelineHandler)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  await page.getByTestId('recording-playback-start').click()
  await page.locator('video').waitFor({ timeout: 15000 })
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video?.videoWidth > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA
  }, undefined, { timeout: 15000 })
  await page.getByTestId('recording-player-shell').hover()
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.getByRole('menuitem', { name: 'チャプターを直す' }).click()
  await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })

  await page.evaluate(() => {
    window.__timelineMarks.length = 0
    window.__startTimelineCapture(0)
  })
  await page.waitForFunction(() => (
    window.__timelineMarks.length > 0 || window.__timelineCaptureError !== null
  ), undefined, { timeout: 10_000 })
  const calibration = await page.evaluate(() => window.__timelineMarks[0])
  if (calibration?.markerSlot !== marker.markerSlot || !Number.isFinite(calibration?.mediaTime)) {
    ng.push(`${label}: 連続再生で frame ${marker.frame} の時刻を校正できない (${JSON.stringify(calibration)})`)
    await page.close()
    return
  }
  const target = calibration.mediaTime + 1 / (2 * SOURCE_FRAME_RATE)

  const measure = async (from, nudgeCount = 0) => page.locator('video').evaluate(async (video, input) => {
    const seek = (seconds, timeoutMs = 5000) => new Promise((resolve) => {
      const timeout = setTimeout(() => resolve(false), timeoutMs)
      video.addEventListener('seeked', () => {
        clearTimeout(timeout)
        resolve(true)
      }, { once: true })
      video.pause()
      video.currentTime = seconds
    })
    const beforeSeeked = await seek(input.from)
    const seeked = await seek(input.target)
    await new Promise((resolve) => setTimeout(resolve, 500))
    const afterSeek = {
      slot: window.__displayedMarkerSlot(),
      currentTime: video.currentTime,
      paused: video.paused,
      seeking: video.seeking,
    }
    const afterNudges = []
    for (let index = 0; index < input.nudgeCount; index += 1) {
      const nudgeSeeked = await seek(video.currentTime + 0.0001, 1000)
      await new Promise((resolve) => setTimeout(resolve, 500))
      afterNudges.push({
        seeked: nudgeSeeked,
        slot: window.__displayedMarkerSlot(),
        currentTime: video.currentTime,
        paused: video.paused,
        seeking: video.seeking,
      })
    }
    return { beforeSeeked, seeked, afterSeek, afterNudges }
  }, { from, target, nudgeCount })

  const far = await measure(target - 0.5)
  const near = await measure(target - 0.05)
  const oneNudge = await measure(target - 0.5, 1)
  const twoNudges = await measure(target - 0.5, 2)
  const format = (sample) => `slot=${sample.slot}, t=${sample.currentTime.toFixed(6)}, paused=${sample.paused}, seeking=${sample.seeking}`
  const nudgesSummary = (sample) => sample.afterNudges.map((nudge, index) => `#${index + 1} seeked=${nudge.seeked} ${format(nudge)}`).join('; ')
  log(`  ${label}: frame=${marker.frame} calibrated=${calibration.mediaTime.toFixed(6)}s; 遠距離(0.5s) seeked=${far.seeked}/${far.beforeSeeked} ${format(far.afterSeek)}; 近距離(0.05s) seeked=${near.seeked}/${near.beforeSeeked} ${format(near.afterSeek)}; 遠距離 + 1 微小 seek: ${nudgesSummary(oneNudge)}; 遠距離 + 2 微小 seek: ${nudgesSummary(twoNudges)}`)
  if (!far.beforeSeeked || !far.seeked || !near.beforeSeeked || !near.seeked ||
    !oneNudge.beforeSeeked || !oneNudge.seeked || oneNudge.afterNudges.some((item) => !item.seeked) ||
    !twoNudges.beforeSeeked || !twoNudges.seeked || twoNudges.afterNudges.some((item) => !item.seeked)) {
    ng.push(`${label}: 素の seek 診断で seeked イベントを観測できない`)
  }
  await page.close()
}

async function runRealRecordingSeekDiagnostics() {
  const boundaryFrame = Math.round(REAL_HLS_BOUNDARY_MS / (1001 / 30))
  const target = (boundaryFrame + 0.5) / SOURCE_FRAME_RATE
  chaptersForHls = true
  log(`\n=== ${engine} 実録画 HLS 診断 (編集モード、境界=${REAL_HLS_BOUNDARY_MS}ms / target=${target.toFixed(6)}s) ===`)
  const page = await context.newPage()
  await installApiStubs(page, timelineHandler)
  await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
  await page.getByTestId('recording-playback-start').click()
  await page.locator('video').waitFor({ timeout: 15000 })
  await page.waitForFunction(() => {
    const video = document.querySelector('video')
    return video?.videoWidth > 0 && video.seekable.length > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA
  }, undefined, { timeout: 30000 })
  const initialMediaRange = await page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    seekable: Array.from({ length: video.seekable.length }, (_, index) => [video.seekable.start(index), video.seekable.end(index)]),
  }))
  if (!initialMediaRange.seekable.some(([start, end]) => start <= target && target <= end)) {
    ng.push(`実録画の境界 target=${target.toFixed(6)}s が seekable 範囲にない (${JSON.stringify(initialMediaRange.seekable)})`)
    await page.unrouteAll({ behavior: 'ignoreErrors' })
    await page.close()
    return
  }
  await page.getByTestId('recording-player-shell').hover()
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.getByRole('menuitem', { name: 'チャプターを直す' }).click()
  await page.waitForSelector('[data-testid="chapter-edit-layout"]', { timeout: 5000 })

  const referenceFrames = await page.locator('video').evaluate(async (video, stopAt) => {
    video.pause()
    const start = video.seekable.length > 0 ? video.seekable.start(0) : 0
    if (Math.abs(video.currentTime - start) > 0.01) {
      await new Promise((resolve, reject) => {
        const check = () => {
          if (!video.seeking && Math.abs(video.currentTime - start) <= 0.05) {
            clearTimeout(timer)
            video.removeEventListener('seeked', check)
            video.removeEventListener('timeupdate', check)
            resolve()
          }
        }
        const timer = setTimeout(() => {
          video.removeEventListener('seeked', check)
          video.removeEventListener('timeupdate', check)
          reject(new Error(`seek to start ${start} timed out at ${video.currentTime}; ranges=${video.seekable.length}`))
        }, 10_000)
        video.addEventListener('seeked', check)
        video.addEventListener('timeupdate', check)
        video.currentTime = start
        check()
      })
    }
    video.muted = true
    const frames = []
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`sequential playback did not reach ${stopAt}s`)), 30_000)
      const capture = (_now, metadata) => {
        frames.push({ mediaTime: metadata.mediaTime, fingerprint: window.__videoFrameFingerprint() })
        if (metadata.mediaTime >= stopAt) {
          clearTimeout(timer)
          video.pause()
          resolve(frames)
        } else {
          video.requestVideoFrameCallback(capture)
        }
      }
      video.requestVideoFrameCallback(capture)
      void video.play().catch((error) => {
        clearTimeout(timer)
        reject(error)
      })
    })
  }, target + 0.5)
  if (referenceFrames.length === 0) throw new Error('連続再生から基準フレームを取得できない')

  const closestReference = (sample) => {
    if (!sample || !Number.isFinite(sample.mediaTime)) return null
    return referenceFrames.reduce((closest, frame) =>
      Math.abs(frame.mediaTime - sample.mediaTime) < Math.abs(closest.mediaTime - sample.mediaTime) ? frame : closest,
    )
  }
  const compareAt = (sample, expectedTime) => {
    const reference = Number.isFinite(sample?.mediaTime)
      ? closestReference(sample)
      : referenceFrames.reduce((closest, frame) =>
        Math.abs(frame.mediaTime - expectedTime) < Math.abs(closest.mediaTime - expectedTime) ? frame : closest,
      )
    return reference !== null && reference.fingerprint === sample?.fingerprint
  }
  const measureRaw = async (from, nudges = 0) => page.locator('video').evaluate(async (video, input) => {
    const seek = (seconds) => new Promise((resolve, reject) => {
      const before = video.currentTime
      let seeked = false
      const ranges = Array.from({ length: video.seekable.length }, (_, index) => [video.seekable.start(index), video.seekable.end(index)])
      const timer = setTimeout(() => {
        reject(new Error(`seek timed out from ${before} to ${seconds}; currentTime=${video.currentTime}, seeking=${video.seeking}, paused=${video.paused}, readyState=${video.readyState}, seeked=${seeked}, ranges=${JSON.stringify(ranges)}`))
      }, 10_000)
      const onSeeked = () => { seeked = true }
      const captureAfterPaint = () => {
        if (!video.seeking && Math.abs(video.currentTime - seconds) < 0.05) {
          clearTimeout(timer)
          video.removeEventListener('seeked', onSeeked)
          resolve({ currentTime: video.currentTime, fingerprint: window.__videoFrameFingerprint(), seeked })
          return
        }
        requestAnimationFrame(captureAfterPaint)
      }
      video.addEventListener('seeked', onSeeked)
      video.pause()
      requestAnimationFrame(() => requestAnimationFrame(captureAfterPaint))
      video.currentTime = seconds
    })
    await seek(input.from)
    const first = await seek(input.target)
    await new Promise((resolve) => setTimeout(resolve, 500))
    const settled = {
      currentTime: video.currentTime,
      fingerprint: window.__videoFrameFingerprint(),
      paused: video.paused,
      seeking: video.seeking,
    }
    const nudged = []
    for (let index = 0; index < input.nudges; index += 1) {
      nudged.push(await seek(video.currentTime + 0.0001))
    }
    if (input.nudges > 0) await new Promise((resolve) => setTimeout(resolve, 500))
    return {
      first,
      settled: input.nudges > 0
        ? { currentTime: video.currentTime, fingerprint: window.__videoFrameFingerprint(), paused: video.paused, seeking: video.seeking }
        : settled,
      nudged,
    }
  }, { from, target, nudges })

  const far = await measureRaw(target - 0.5)
  const near = await measureRaw(target - 0.05)
  const oneNudge = await measureRaw(target - 0.5, 1)
  const twoNudges = await measureRaw(target - 0.5, 2)
  const summary = (sample) => `first=${sample.first.currentTime.toFixed(6)}s/${compareAt(sample.first, target)}, settled=${sample.settled.currentTime.toFixed(6)}s/${compareAt(sample.settled, target)}, paused=${sample.settled.paused}, seeking=${sample.settled.seeking}`
  const expectedFrameStart = target - 1 / (2 * SOURCE_FRAME_RATE)
  const expectedFrame = referenceFrames.reduce((closest, frame) =>
    Math.abs(frame.mediaTime - expectedFrameStart) < Math.abs(closest.mediaTime - expectedFrameStart) ? frame : closest,
  )
  const expectedFrameTimeDiff = Math.abs(expectedFrame.mediaTime - expectedFrameStart)
  log(`  連続再生で frame ${boundaryFrame} を ${referenceFrames.length} 枚取得`)
  log(`  基準 frame PTS=${expectedFrame.mediaTime.toFixed(6)}s (want=${expectedFrameStart.toFixed(6)}s, diff=${(expectedFrameTimeDiff * 1000).toFixed(2)}ms), hash=${expectedFrame.fingerprint}`)
  log(`  素の遠距離 seek: ${summary(far)} hash=${far.settled.fingerprint}`)
  log(`  素の近距離 seek: ${summary(near)} hash=${near.settled.fingerprint}`)
  log(`  遠距離 + 1 回微小 seek: ${summary(oneNudge)} hash=${oneNudge.settled.fingerprint}`)
  log(`  遠距離 + 2 回微小 seek: ${summary(twoNudges)} hash=${twoNudges.settled.fingerprint}`)

  const from = target - 0.5
  await page.locator('video').evaluate((video, seconds) => new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('製品 seek 前の位置へ移動できない')), 10_000)
    const finish = () => {
      clearTimeout(timer)
      resolve()
    }
    video.addEventListener('seeked', finish, { once: true })
    video.pause()
    video.currentTime = seconds
  }), from)
  const boundaryButton = page.locator(`[data-testid="chapter-filmstrip-boundary"][data-time-ms="${REAL_HLS_BOUNDARY_MS}"]`)
  await page.locator('video').evaluate((video, expected) => {
    window.__boundaryFrameCallbacks = []
    const sourceBeforeSeek = { src: video.getAttribute('src'), currentSrc: video.currentSrc }
    const requestFrame = video.requestVideoFrameCallback.bind(video)
    video.requestVideoFrameCallback = (callback) => requestFrame((now, metadata) => {
      const sample = {
        mediaTime: metadata.mediaTime,
        currentTime: video.currentTime,
        seeking: video.seeking,
      }
      callback(now, metadata)
      window.__boundaryFrameCallbacks.push({
        ...sample,
        nearTarget: Math.abs(video.currentTime - expected.target) <= 0.01,
        nearFrame: Math.abs(metadata.mediaTime - video.currentTime) <= 1001 / 30000 / 2 + 0.001,
        sourceUnchanged: sourceBeforeSeek.src === video.getAttribute('src') && sourceBeforeSeek.currentSrc === video.currentSrc,
      })
    })
  }, { target })
  const productFirst = await capturePresentedFrameAt(page, () => boundaryButton.evaluate((button) => button.click()), target)
  await page.waitForTimeout(500)
  const productSettled = await page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    fingerprint: window.__videoFrameFingerprint(),
    paused: video.paused,
    seeking: video.seeking,
  }))
  const callbackLog = await page.evaluate(() => window.__boundaryFrameCallbacks)
  log(`  製品の境界選択: first=${productFirst.mediaTime?.toFixed(6)}s/${compareAt(productFirst, target)} hash=${productFirst.fingerprint}, settled=${productSettled.currentTime.toFixed(6)}s/${compareAt(productSettled, target)} hash=${productSettled.fingerprint}, paused=${productSettled.paused}, seeking=${productSettled.seeking}, callbacks=${JSON.stringify(callbackLog.slice(-6))}`)
  if (
    expectedFrameTimeDiff > GRID_TOLERANCE_SECONDS ||
    !productFirst.matched ||
    !Number.isFinite(productFirst.mediaTime) ||
    Math.abs(productFirst.mediaTime - expectedFrameStart) > GRID_TOLERANCE_SECONDS ||
    productFirst.fingerprint !== expectedFrame.fingerprint
  ) {
    ng.push('実録画: 製品の境界選択後、最初に表示したフレームが連続再生の対象フレームと一致しない')
  }
  if (!compareAt(productSettled, target)) ng.push('実録画: 製品の境界選択後に連続再生で見た同じフレームを表示しない')
  await page.unrouteAll({ behavior: 'ignoreErrors' })
  await page.close()
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

if (RAW_SEEK_DIAGNOSTICS) await runRawSeekDiagnostics()
if (RAW_SEEK_ONLY) await finish(ng, browser)

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
  // 原本 HLS（LivePlayer）でも、境界を選ぶと一時停止して目印フレームが映り、「境界まで」「境界から」が
  // 目印フレームで止まる / 始まる。この後のシークバー操作に備えて編集は閉じる。
  await hlsPage.getByTestId('recording-player-shell').hover()
  await hlsPage.getByRole('button', { name: '再生設定' }).click()
  await hlsPage.getByRole('menuitem', { name: 'チャプターを直す' }).click()
  await exerciseBoundaryControls(hlsPage, '原本 HLS offset 0', { first: 0, stop: 2, from: 1 }, 0, 0.1)
  if (BOUNDARY_SEEK_ONLY) await finish(ng, browser)
  await hlsPage.getByRole('button', { name: 'やめる', exact: true }).click()
  await hlsPage.waitForSelector('[data-testid="chapter-edit-layout"]', { state: 'detached' })
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
    await exerciseBoundaryControls(hlsPage, `原本 HLS offset ${OFFSET_SECONDS}`, { first: 6, stop: 5, from: 4 }, sessionOrigin(OFFSET_SECONDS), 0.1)
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
