// 原本 MPEG-2 TS の一時 HLS を実ブラウザで確認する。
//
// original-only の完了録画を開き、FFmpeg が生成した VOD playlist / TS segments を
// HLS player が再生すること、実 seek、字幕 cue、保存位置の復元、終端到達を測る。
// 元 TS には MPEG-2 video / MP2 audio を含め、FFmpeg で H.264/AAC の VOD HLS に
// 変換する。字幕レンディションは同じ fixture の SRT から WebVTT に変換する。
// API と streamer の URL だけを page.route で差し替えるので、録画 DB やチューナーは要らない。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:recording-original-vod
//   E2E_URL=http://localhost:4173 E2E_BROWSER=webkit pnpm e2e:recording-original-vod
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
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
const RECORDING_ID = 920
const PLAYBACK_PROFILE = 'vod-h264'
const ng = []
const startedAt = new Date(Date.now() - 16_000).toISOString()
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
  title: '原本 MPEG-2 のブラウザ再生',
  description: 'MPEG-2 TS 原本だけがある完了録画の実ブラウザ E2E fixture。',
  startAt: startedAt,
  durationMs: 16_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: [PLAYBACK_PROFILE],
  sizeBytes: 1_000_000,
  encodedAssets: [],
}

function runFFmpeg(args, cwd) {
  execFileSync('ffmpeg', args, { cwd, stdio: 'pipe' })
}

/** MPEG-2 source TS と、その実変換結果となる VOD HLS を用意する。 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-original-vod-fixture')
  const sourcePath = path.join(fixtureDir, 'original.ts')
  const masterPath = path.join(fixtureDir, 'playlist.m3u8')
  const videoPlaylistPath = path.join(fixtureDir, 'playlist_0.m3u8')
  if (existsSync(sourcePath) && existsSync(masterPath) && existsSync(videoPlaylistPath)) {
    const sourceInfo = execFileSync(
      'ffprobe',
      ['-v', 'error', '-show_entries', 'stream=codec_name', '-of', 'csv=p=0', sourcePath],
      { encoding: 'utf8' },
    )
    const playlist = readFileSync(videoPlaylistPath, 'utf8')
    const master = readFileSync(masterPath, 'utf8')
    if (sourceInfo.includes('mpeg2video') && playlist.includes('#EXT-X-ENDLIST') && master.includes('TYPE=SUBTITLES')) return fixtureDir
  }

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
    execFileSync('ffprobe', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(path.join(fixtureDir, 'segments'), { recursive: true })
  const captionPath = path.join(fixtureDir, 'caption.srt')
  writeFileSync(
    captionPath,
    '1\n00:00:02,000 --> 00:00:06,000\nMPEG-2 original subtitle\n\n' +
      '2\n00:00:08,000 --> 00:00:12,000\nSeeked subtitle\n',
  )
  log(`MPEG-2 原本と VOD HLS を生成中... (${fixtureDir})`)

  // SRT を MPEG-TS の DVB subtitle stream として原本へ mux する。
  runFFmpeg(
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=25',
      '-f', 'lavfi', '-i', 'sine=frequency=440',
      '-i', captionPath,
      '-t', '16',
      '-map', '0:v:0', '-map', '1:a:0',
      '-c:v', 'mpeg2video', '-b:v', '1400k', '-g', '50', '-sc_threshold', '0',
      '-pix_fmt', 'yuv420p',
      '-c:a', 'mp2', '-b:a', '128k',
      '-f', 'mpegts', sourcePath,
    ],
    fixtureDir,
  )

  const sourceStreams = execFileSync(
    'ffprobe',
    ['-v', 'error', '-show_entries', 'stream=codec_name', '-of', 'csv=p=0', sourcePath],
    { encoding: 'utf8' },
  )
  if (!sourceStreams.includes('mpeg2video')) {
    throw new Error(`fixture must contain MPEG-2 video (streams=${sourceStreams.trim()})`)
  }

  // VOD conversion uses the same shape as the captions-enabled streamer: one video
  // variant, separate standard/main/sub audio renditions, and an optional WebVTT track.
  runFFmpeg(
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-i', sourcePath, '-i', captionPath,
      '-map', '0:v:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '1:s:0',
      '-c:v:0', 'libx264', '-profile:v:0', 'baseline', '-level:v:0', '3.0',
      '-pix_fmt:v:0', 'yuv420p', '-preset:v:0', 'veryfast', '-g:v:0', '50',
      '-sc_threshold:v:0', '0', '-force_key_frames:v:0', 'expr:gte(t,n_forced*2)',
      '-c:a:0', 'aac', '-b:a:0', '64k',
      '-c:a:1', 'aac', '-b:a:1', '64k', '-filter:a:1', 'pan=stereo|c0=c0|c1=c0',
      '-c:a:2', 'aac', '-b:a:2', '64k', '-filter:a:2', 'pan=stereo|c0=c1|c1=c1',
      '-c:s', 'webvtt',
      '-var_stream_map', 'v:0,agroup:a0,s:0,sgroup:subs a:0,agroup:a0,default:yes a:1,agroup:a0 a:2,agroup:a0',
      '-master_pl_name', 'playlist.m3u8',
      '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0', '-hls_playlist_type', 'vod',
      '-hls_flags', 'temp_file', '-hls_base_url', 'segments/',
      '-hls_segment_filename', path.join(fixtureDir, 'segments', '%v_seg%05d.ts'),
      '-hls_subtitle_path', path.join(fixtureDir, 'subtitles_%v.m3u8'),
      path.join(fixtureDir, 'playlist_%v.m3u8'),
    ],
    fixtureDir,
  )
  return fixtureDir
}

log('\n=== 契約検証: original-only recording fixture ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

let fixtureDir
try {
  fixtureDir = ensureFixture()
} catch (err) {
  ng.push(`MPEG-2 / DVB subtitle の FFmpeg fixture を生成できない: ${err.message}`)
  await finish(ng)
}
if (fixtureDir === undefined) {
  ng.push('ffmpeg / ffprobe が無いため MPEG-2 HLS の実ブラウザ判定を実行できない')
  await finish(ng)
}

const engine = process.env.E2E_BROWSER ?? 'chrome'
log(`\n=== 実ブラウザ: ${engine} ===`)
const browser = engine === 'chrome'
  ? await launchBrowser('chromium', { channel: 'chrome' })
  : await launchBrowser(engine)
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
const page = await context.newPage()
const playlistRequests = []
const segmentRequests = []
const subtitleRequests = []
const encodedRequests = []

await installApiStubs(page, async ({ path: requestPath, url, json, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/live-profiles') return json([{ name: 'h264', height: 360 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([recording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') return json(recording)
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) {
    return route.fulfill({ status: 404 })
  }
  if (requestPath.startsWith(`/api/sites/${SITE}/recordings/${RECORDING_ID}/original-vod/`)) {
    const relative = requestPath.split(`/original-vod/`)[1]
    if (relative.endsWith('.m3u8')) playlistRequests.push(relative)
    if (relative.includes('segments/')) {
      const name = relative.split('/').pop()
      if (name.endsWith('.vtt')) subtitleRequests.push(name)
      else segmentRequests.push(name)
      const file = name.endsWith('.vtt')
        ? path.join(fixtureDir, name)
        : path.join(fixtureDir, 'segments', name)
      if (!existsSync(file)) return route.fulfill({ status: 404, body: 'fixture missing' })
      return route.fulfill({ status: 200, contentType: name.endsWith('.vtt') ? 'text/vtt; charset=utf-8' : 'video/mp2t', body: readFileSync(file) })
    }
    const name = relative.split('/').pop()
    const file = path.join(fixtureDir, name)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: 'fixture missing' })
    return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body: readFileSync(file) })
  }
  if (/^\/api\/media\/recordings\/\d+\/file$/.test(requestPath)) {
    encodedRequests.push(url.href)
    return route.fulfill({ status: 404 })
  }
  if (requestPath.endsWith('/original-vod/leave') && method === 'POST') {
    return route.fulfill({ status: 204 })
  }
  return json([])
})

log('\n=== ① encoded なしの完了録画で原本 HLS を再生 ===')
await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
await page.getByRole('region', { name: '原本 TS をブラウザ再生' }).waitFor({ timeout: 15000 })
const video = page.locator('video')
await video.waitFor({ timeout: 15000 })
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && Number.isFinite(element.duration) && element.duration > 0
}, undefined, { timeout: 20000 })
const initial = await video.evaluate((element) => ({ time: element.currentTime, duration: element.duration }))
if (initial.time > 1.5) ng.push(`① 先頭から始まらない（currentTime=${initial.time.toFixed(2)}）`)
if (encodedRequests.length !== 0) ng.push(`① original-only なのに encoded MP4 を要求する (${encodedRequests.join(', ')})`)
if (!playlistRequests.some((name) => name === 'playlist.m3u8')) {
  ng.push(`① 原本 VOD の master playlist が要求されない (${playlistRequests.join(', ') || 'none'})`)
}
if (!readFileSync(path.join(fixtureDir, 'playlist_0.m3u8'), 'utf8').includes('#EXT-X-ENDLIST')) {
  ng.push('① VOD variant に #EXT-X-ENDLIST がない')
}

await video.evaluate(async (element) => {
  element.muted = true
  await element.play()
})
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && element.videoWidth > 0 && element.currentTime > 1
}, undefined, { timeout: 15000 }).catch(() => ng.push('① H.264 映像の実再生が始まらない'))

log('\n=== ② 実 seek・字幕 cue・保存位置の復元 ===')
await page.waitForFunction(() => {
  const tracks = Array.from(document.querySelector('video')?.textTracks ?? [])
  return tracks.some((track) => track.kind === 'subtitles')
}, undefined, { timeout: 10000 }).catch(() => ng.push('② MPEG-2 原本の DVB subtitle が HLS text track にならない'))
const cueResult = await video.evaluate(async (element) => {
  const track = Array.from(element.textTracks).find((candidate) => candidate.kind === 'subtitles')
  if (!track) return { trackFound: false, cueCount: 0 }
  track.mode = 'hidden'
  const deadline = Date.now() + 8000
  while ((track.cues?.length ?? 0) === 0 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  return { trackFound: true, cueCount: track.cues?.length ?? 0 }
})
if (!cueResult.trackFound || cueResult.cueCount === 0) {
  ng.push(`② 原本由来の字幕 cue を読み込めない (${JSON.stringify(cueResult)})`)
}

await video.evaluate((element) => { element.currentTime = 7.25 })
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && Math.abs(element.currentTime - 7.25) < 1.25
}, undefined, { timeout: 10000 }).catch(() => ng.push('② currentTime を指定位置へ seek できない'))
const playbackKey = `rokuban:playback:${RECORDING_ID}:${PLAYBACK_PROFILE}`
await video.evaluate((element) => element.pause())
await page.waitForFunction((key) => {
  const saved = Number(localStorage.getItem(key))
  return Number.isFinite(saved) && saved >= 6 && saved <= 10
}, playbackKey, { timeout: 5000 }).catch(() => ng.push('② seek 位置が再生位置として localStorage に保存されない'))
const savedPosition = Number(await page.evaluate((key) => localStorage.getItem(key), playbackKey))
log(`  保存位置: ${savedPosition}s`)

await page.reload({ waitUntil: 'domcontentloaded' })
await page.locator('video').waitFor({ timeout: 15000 })
await page.waitForFunction((expected) => {
  const element = document.querySelector('video')
  return element !== null && element.duration > 0 && Math.abs(element.currentTime - expected) < 1.5
}, savedPosition, { timeout: 15000 }).catch(() => ng.push('② reload 後に原本 HLS の保存位置を復元しない'))

log('\n=== ③ 終端まで再生 ===')
await page.locator('video').evaluate(async (element) => {
  element.muted = true
  element.currentTime = element.duration - 0.8
  await element.play()
})
await page.waitForFunction(() => document.querySelector('video')?.ended === true, undefined, { timeout: 10000 })
  .catch(() => ng.push('③ 原本 HLS の #EXT-X-ENDLIST まで再生し終わらない'))
const finalSavedPosition = await page.evaluate((key) => localStorage.getItem(key), playbackKey)
if (finalSavedPosition !== null) ng.push(`③ 終端付近の保存位置が残る (${finalSavedPosition})`)
if (segmentRequests.length === 0) ng.push('③ HLS segment を要求していない')
if (subtitleRequests.length === 0) ng.push('③ 原本由来の WebVTT segment を要求していない')
log(`  variant playlists=${playlistRequests.length}, video segments=${segmentRequests.length}, subtitle segments=${subtitleRequests.length}`)

await finish(ng, browser)
