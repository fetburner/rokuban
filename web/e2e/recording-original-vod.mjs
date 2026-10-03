// 原本 MPEG-2 TS の一時 HLS を実ブラウザで確認する。
//
// original-only の完了録画を開き、HLS player が H.264/AAC の HLS を再生すること、
// 実 seek、HLS の WebVTT 字幕レンディション、保存位置の復元、終端到達を測る。
//
// **この E2E は streamer を起動しない。** API と streamer の URL を page.route で差し替え、
// fixture を自前の ffmpeg 引数で作って配るだけである。製品の ffmpeg 引数（event playlist /
// `hls_list_size 0` / `temp_file` / `segments/` base URL）そのものは Go のテスト
// （internal/streamer の BuildOriginalVODFFmpegArgs と偽 ffmpeg のテスト）が見ており、
// ここの手書き引数はそれと同じ形に揃えてあるだけで、同一であることは保証しない。
// 元 TS は MPEG-2 video / MP2 audio だけを持つ。字幕は SRT から直接 WebVTT にしており、
// **原本（ARIB / DVB 字幕ストリーム）由来の字幕は未検証**（ffmpeg はテキスト字幕から
// ビットマップ字幕を作れない）。①〜③ の playlist は最初から ENDLIST 済みである。
// ⑤ は streamer と同じ `-ss {offset}`（0 起点）で offset ごとに作り、ENDLIST を外した
// 変換中の playlist で開始位置・張り直し後の再生継続・末尾の 416・枠の操作を見る。
// 変換が進んで playlist が伸びていく挙動そのものは配らない（先端は 20 秒で止まる）。
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
const recordingDurationMs = 16_000
const startedAt = new Date(Date.now() - recordingDurationMs).toISOString()
const endedAt = new Date(Date.parse(startedAt) + recordingDurationMs).toISOString()
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
  // 予定尺と実尺をずらし、プレイヤーが startedAt〜endedAt を使うことを確かめる。
  durationMs: 60_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'disabled' },
  startedAt,
  endedAt,
  createdAt: '2026-01-01T12:00:00Z',
  encodeProfiles: [PLAYBACK_PROFILE],
  sizeBytes: 1_000_000,
  encodedAssets: [],
}

// ⑤ の録画。映像は 60 秒、DB の実尺は 63 秒（末尾付近のクリックを 416 にする）。
const OFFSET_ID = 921
const OFFSET_VIDEO_SECONDS = 60
// この合成 TS は `-ss 59` で映像が 0 フレームになる（ffmpeg 実測。-ss 58 は 50 フレーム）。
// streamer の範囲判定（映像の終端 - 0.5 秒より後ろを 416）を、使える映像の終端 58.5 秒で真似る。
const OFFSET_PLAYABLE_END = 58.5
const OFFSET_DB_SECONDS = 63
const offsetStartedAt = new Date(Date.now() - 3_600_000).toISOString()
const offsetRecording = {
  ...recording,
  id: OFFSET_ID,
  title: '原本 offset セッションの再生',
  startAt: offsetStartedAt,
  startedAt: offsetStartedAt,
  endedAt: new Date(Date.parse(offsetStartedAt) + OFFSET_DB_SECONDS * 1000).toISOString(),
  durationMs: 30 * 60_000,
}
delete offsetRecording.resumePositionMs
delete offsetRecording.watchedAt

function runFFmpeg(args, cwd) {
  execFileSync('ffmpeg', args, { cwd, stdio: 'pipe' })
}

function ensureEncodedFixture(fixtureDir) {
  const encodedPath = path.join(fixtureDir, 'encoded.mp4')
  if (existsSync(encodedPath)) return encodedPath
  runFFmpeg(
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-i', path.join(fixtureDir, 'original.ts'),
      '-map', '0:v:0', '-map', '0:a:0',
      '-c:v', 'libx264', '-profile:v', 'baseline', '-level', '3.0', '-pix_fmt', 'yuv420p',
      '-preset', 'veryfast', '-g', '50', '-c:a', 'aac', '-b:a', '96k',
      '-movflags', '+faststart', encodedPath,
    ],
    fixtureDir,
  )
  return encodedPath
}

/** MPEG-2 source TS と、それを変換した HLS（EVENT playlist + ENDLIST）を用意する。 */
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
    if (
      sourceInfo.includes('mpeg2video') &&
      playlist.includes('#EXT-X-ENDLIST') &&
      playlist.includes('#EXT-X-PLAYLIST-TYPE:EVENT') &&
      master.includes('TYPE=SUBTITLES')
    ) return fixtureDir
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
  log(`MPEG-2 原本と HLS を生成中... (${fixtureDir})`)

  // 原本は映像（MPEG-2）と音声（MP2）だけ。字幕は原本へ mux しない。
  runFFmpeg(
    [
      '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
      '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=25',
      '-f', 'lavfi', '-i', 'sine=frequency=440',
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

  // 製品（captions 有効の streamer）と同じ形: video variant 1 本、標準 / 主 / 副の音声
  // rendition、WebVTT 字幕（ここでは SRT から直接作る）。playlist は EVENT で、ffmpeg の
  // 終了時に ENDLIST が付く。`vod` は終了時まで playlist を書かないので使わない。
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
      '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0', '-hls_playlist_type', 'event',
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
await validateFixturesOrExit([
  ['recording', ListRecordingsResponseItem, recording],
  ['offset recording', ListRecordingsResponseItem, offsetRecording],
], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

let fixtureDir
let seekTileSprite
try {
  fixtureDir = ensureFixture()
} catch (err) {
  ng.push(`MPEG-2 原本と HLS の FFmpeg fixture を生成できない: ${err.message}`)
  await finish(ng)
}
if (fixtureDir === undefined) {
  ng.push('ffmpeg / ffprobe が無いため原本 HLS の実ブラウザ判定を実行できない')
  await finish(ng)
}
seekTileSprite = execFileSync('ffmpeg', [
  '-hide_banner', '-loglevel', 'error', '-y', '-i', path.join(fixtureDir, 'original.ts'),
  '-vf', 'fps=1/10,scale=160:90:force_original_aspect_ratio=decrease,pad=160:90:(ow-iw)/2:(oh-ih)/2,setsar=1,tpad=stop_mode=clone:stop_duration=100,tile=10x2:padding=0:margin=0',
  '-frames:v', '1', '-f', 'image2pipe', '-vcodec', 'mjpeg', 'pipe:1',
])
const encodedFixturePath = ensureEncodedFixture(fixtureDir)

const engine = process.env.E2E_BROWSER ?? 'chrome'
log(`\n=== 実ブラウザ: ${engine} ===`)
const browser = engine === 'chrome'
  ? await launchBrowser('chromium', { channel: 'chrome' })
  : await launchBrowser(engine)
log(`  browser version: ${browser.version()}`)
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
await context.addInitScript(() => {
  const sources = []
  class E2EEventSource extends EventTarget {
    static CONNECTING = 0
    static OPEN = 1
    static CLOSED = 2
    readyState = E2EEventSource.OPEN
    withCredentials = false

    constructor(url) {
      super()
      this.url = String(url)
      sources.push(this)
      queueMicrotask(() => this.dispatchEvent(new Event('open')))
    }

    close() {
      this.readyState = E2EEventSource.CLOSED
    }
  }
  window.EventSource = E2EEventSource
  window.__emitE2EEvent = (type) => {
    for (const source of sources) source.dispatchEvent(new Event(type))
  }
})
const page = await context.newPage()
const playlistRequests = []
const segmentRequests = []
const subtitleRequests = []
const encodedRequests = []
const encodedRangeRequests = []
const playbackPositionWrites = []
const watchedWrites = []
const seekTileRequests = []
const masterPlaylistRequests = []
const audioPlaylistRequests = []
const offsetVideoSegmentRequests = []
const originalVODLeaveRequests = []
let recordingDetailRequests = 0
// ④ で true にする。variant / 字幕 playlist を先頭 4 segment で切り、ENDLIST を外して返す
// （変換中の EVENT playlist の先端を再現する）。
let growingEdge = false

function playlistAtOffset(text, offsetSeconds) {
  const firstSegment = Math.floor(offsetSeconds / 2)
  const out = []
  let segment = -1
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-MEDIA-SEQUENCE:')) {
      out.push(`#EXT-X-MEDIA-SEQUENCE:${firstSegment}`)
      continue
    }
    if (line.startsWith('#EXTINF:')) segment += 1
    if (segment < 0 || segment >= firstSegment || line.startsWith('#EXT-X-ENDLIST')) out.push(line)
  }
  return out.join('\n') + '\n'
}

function growingEdgePlaylist(text) {
  const out = []
  let extinf = 0
  for (const line of text.split('\n')) {
    if (line.startsWith('#EXT-X-ENDLIST')) continue
    if (line.startsWith('#EXTINF')) extinf += 1
    if (extinf > 4) break
    out.push(line)
  }
  return out.join('\n') + '\n'
}

await installApiStubs(page, async ({ path: requestPath, url, json, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/live-profiles') {
    return json([{ name: 'hd', height: 720 }, { name: 'sd', height: 480 }])
  }
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([recording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') {
    recordingDetailRequests += 1
    return json(recording)
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/chapters`) {
    return json({
      version: 'chapters-v1',
      detectionPending: false,
      source: 'auto',
      spans: [{ startMs: 10_000, endMs: 15_000, label: 'CM', cut: true }],
    })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/playback-position` && method === 'PUT') {
    const body = route.request().postDataJSON()
    recording.resumePositionMs = body.positionMs
    playbackPositionWrites.push(body.positionMs)
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/playback-position` && method === 'DELETE') {
    delete recording.resumePositionMs
    return route.fulfill({ status: 204 })
  }
  if (requestPath === `/api/recordings/${RECORDING_ID}/watched` && method === 'PUT') {
    recording.watchedAt = new Date().toISOString()
    delete recording.resumePositionMs
    watchedWrites.push(recording.watchedAt)
    return route.fulfill({ status: 204 })
  }
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) {
    return route.fulfill({ status: 404 })
  }
  if (requestPath === `/api/media/recordings/${RECORDING_ID}/seek-tiles`) {
    seekTileRequests.push(requestPath)
    return route.fulfill({
      status: 200,
      contentType: 'image/jpeg',
      body: seekTileSprite,
    })
  }
  if (requestPath.startsWith(`/api/sites/${SITE}/recordings/${RECORDING_ID}/original-vod/`)) {
    const relative = requestPath.split(`/original-vod/`)[1]
    const offsetMatch = relative.match(/^offset\/(\d+)\//)
    const offsetSeconds = offsetMatch ? Number(offsetMatch[1]) : undefined
    const resource = offsetMatch ? relative.slice(offsetMatch[0].length) : relative
    if (relative.endsWith('.m3u8')) playlistRequests.push(relative)
    if (resource === 'playlist.m3u8') masterPlaylistRequests.push(relative)
    if (/^playlist_[1-3]\.m3u8$/.test(resource)) audioPlaylistRequests.push(resource)
    if (resource.includes('segments/')) {
      const name = resource.split('/').pop()
      if (offsetSeconds !== undefined && name.startsWith('0_seg')) {
        offsetVideoSegmentRequests.push({ offsetSeconds, name })
      }
      if (name.endsWith('.vtt')) subtitleRequests.push(name)
      else segmentRequests.push(name)
      const file = name.endsWith('.vtt')
        ? path.join(fixtureDir, name)
        : path.join(fixtureDir, 'segments', name)
      if (!existsSync(file)) return route.fulfill({ status: 404, body: 'fixture missing' })
      return route.fulfill({ status: 200, contentType: name.endsWith('.vtt') ? 'text/vtt; charset=utf-8' : 'video/mp2t', body: readFileSync(file) })
    }
    const name = resource.split('/').pop()
    const file = path.join(fixtureDir, name)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: 'fixture missing' })
    let body = readFileSync(file)
    if (offsetSeconds !== undefined && /^(playlist|subtitles)_\d+\.m3u8$/.test(name)) {
      body = playlistAtOffset(body.toString('utf8'), offsetSeconds)
    } else if (growingEdge && offsetSeconds === undefined && /^(playlist|subtitles)_\d+\.m3u8$/.test(name)) {
      body = growingEdgePlaylist(body.toString('utf8'))
    }
    return route.fulfill({ status: 200, contentType: 'application/vnd.apple.mpegurl', body })
  }
  if (/^\/api\/media\/recordings\/\d+\/file$/.test(requestPath)) {
    encodedRequests.push(url.href)
    if (recording.encodedAssets.length > 0) {
      const bytes = readFileSync(encodedFixturePath)
      const range = /^bytes=(\d+)-(\d*)$/.exec(route.request().headers().range ?? '')
      if (!range) {
        return route.fulfill({
          status: 200,
          contentType: 'video/mp4',
          body: bytes,
          headers: { 'Accept-Ranges': 'bytes' },
        })
      }
      const start = Number(range[1])
      const end = Math.min(range[2] ? Number(range[2]) : bytes.length - 1, bytes.length - 1)
      if (start >= bytes.length || end < start) {
        return route.fulfill({ status: 416, headers: { 'Content-Range': `bytes */${bytes.length}` } })
      }
      encodedRangeRequests.push({ start, end })
      return route.fulfill({
        status: 206,
        contentType: 'video/mp4',
        body: bytes.subarray(start, end + 1),
        headers: {
          'Accept-Ranges': 'bytes',
          'Content-Range': `bytes ${start}-${end}/${bytes.length}`,
        },
      })
    }
    return route.fulfill({ status: 404 })
  }
  if (requestPath.endsWith('/original-vod/leave') && method === 'POST') {
    originalVODLeaveRequests.push(requestPath)
    return route.fulfill({ status: 204 })
  }
  return json([])
})

log('\n=== ① encoded なしの完了録画で原本 HLS を再生 ===')
await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
const playbackGroup = page.getByTestId('recording-playback-group')
await playbackGroup.waitFor({ timeout: 15000 })
const obsoleteOriginalHeadingCount = playbackGroup.getByRole('heading', {
  name: /原本 TS をブラウザ再生/,
}).count()
if ((await obsoleteOriginalHeadingCount) !== 0) ng.push('① 原本 VOD の映像の上に旧見出しがある')
const video = page.locator('video')
await page.waitForTimeout(750)
const playlistsBeforePlay = playlistRequests.length
if (playlistsBeforePlay !== 0) {
  ng.push(`① 再生ボタンを押す前に original HLS playlist を要求した (${playlistRequests.join(', ')})`)
} else {
  const playButton = page.getByRole('button', { name: '再生', exact: true })
  // 再生前のポスターは再生後のプレイヤー枠と同じ寸法で、視聴済みの操作も枠の中にある
  // （枠の外に出すと、押した後に消えて下の要素が跳ぶ）。
  const posterBox = await page.evaluate(() => {
    const poster = document.querySelector('[data-testid="recording-playback-poster"]')
    const group = document.querySelector('[data-testid="recording-playback-group"]')
    const watched = Array.from(document.querySelectorAll('button')).filter((b) => /視聴済みにする|未視聴に戻す/.test(b.textContent ?? ''))
    const rect = poster?.getBoundingClientRect()
    return {
      width: rect?.width, height: rect?.height, top: rect?.top,
      groupHeight: group?.getBoundingClientRect().height,
      watchedButtons: watched.length,
      watchedInsidePoster: watched.every((b) => poster?.contains(b)),
    }
  })
  if (posterBox.watchedButtons !== 1 || !posterBox.watchedInsidePoster) {
    ng.push(`① 再生前の視聴済み操作が枠の中に 1 つでない（${JSON.stringify(posterBox)}）`)
  }
  if (await playButton.count() !== 1) {
    ng.push(`① playlist が始まる再生ボタンが 1 つでない (${await playButton.count()})`)
  } else {
    await playButton.click()
    await page.getByTestId('recording-player-frame').waitFor({ timeout: 15000 })
    const frameBox = await page.evaluate(() => {
      const frame = document.querySelector('[data-testid="recording-player-frame"]').getBoundingClientRect()
      const group = document.querySelector('[data-testid="recording-playback-group"]')
      return { width: frame.width, height: frame.height, top: frame.top, groupHeight: group.getBoundingClientRect().height }
    })
    if (Math.abs(frameBox.width - posterBox.width) > 1 || Math.abs(frameBox.height - posterBox.height) > 1 ||
      Math.abs(frameBox.top - posterBox.top) > 1 || Math.abs(frameBox.groupHeight - posterBox.groupHeight) > 1) {
      ng.push(`① 原本 HLS の枠が再生の前後で変わる（前 ${JSON.stringify(posterBox)} 後 ${JSON.stringify(frameBox)}）`)
    }
    const playlistDeadline = Date.now() + 10000
    while (playlistRequests.length === 0 && Date.now() < playlistDeadline) await page.waitForTimeout(50)
    if (playlistRequests.length === 0) ng.push('① 再生ボタンを押しても original HLS playlist を要求しない')
  }
}
await video.waitFor({ timeout: 15000 })
if (await video.evaluate((element) => element.controls)) {
  ng.push('① 原本 VOD の video に native controls が残っている')
}
const seekbars = page.getByTestId('seek-scrub')
if (await seekbars.count() !== 1) {
  ng.push(`① 共通操作バーのシークバーが 1 本ではない (${await seekbars.count()})`)
} else {
  const box = await seekbars.boundingBox()
  if (!box) {
    ng.push('① 共通シークバーの位置を取得できない')
  } else {
    await page.mouse.move(box.x + box.width * 0.7, box.y + box.height / 2)
    await page.locator('[data-testid="seek-tile-preview"]').waitFor({ timeout: 1500 })
      .catch(() => ng.push('① シークバーのホバーでタイルプレビューが出ない'))
    if (seekTileRequests.length === 0) ng.push('① ホバー時に seek-tile 画像を要求しない')
  }
}
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
await page.getByTestId('chapter-marker').waitFor({ timeout: 5000 }).catch(() => {})
if (await page.getByTestId('chapter-marker').count() !== 1) {
  ng.push('① 原本 VOD のチャプター目盛りが出ない')
}
if (!readFileSync(path.join(fixtureDir, 'playlist_0.m3u8'), 'utf8').includes('#EXT-X-ENDLIST')) {
  ng.push('① variant に #EXT-X-ENDLIST がない')
}

await video.evaluate(async (element) => {
  element.muted = true
  await element.play()
})
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && element.videoWidth > 0 && element.currentTime > 1
}, undefined, { timeout: 15000 }).catch(() => ng.push('① H.264 映像の実再生が始まらない'))
if (process.env.E2E_SHOT_DIR) {
  mkdirSync(process.env.E2E_SHOT_DIR, { recursive: true })
  await page.screenshot({ path: path.join(process.env.E2E_SHOT_DIR, 'v3-original-vod.png') })
}

const screenshotDir = process.env.E2E_SCREENSHOT_DIR
if (screenshotDir) mkdirSync(screenshotDir, { recursive: true })

const audioSettingsButton = page.getByRole('button', { name: '再生設定' })
await audioSettingsButton.click()
const settingsMenu = page.getByRole('menu', { name: '再生設定' })
const settingsOrder = async () => settingsMenu.locator('[role^="menuitem"]').evaluateAll((items) =>
  items
    .filter((item) => item.getClientRects().length > 0)
    .map((item) => ({ label: item.getAttribute('aria-label'), top: item.getBoundingClientRect().top }))
    .filter((item) => item.label !== null)
    .sort((a, b) => a.top - b.top)
    .map((item) => item.label),
)
// #1013 の決定: デスクトップは CM・字幕・再生速度・画質（画質が歯車に近い最下段）、スマホのシートは
// 画質が先頭。原本 HLS もエンコード版と同じ順にし、音声は画質の隣に入れる。
const expectedDesktopOrder = ['CM を飛ばす', '字幕', '再生速度', '音声', '画質']
const expectedPhoneOrder = ['画質', '音声', '再生速度', '字幕', 'CM を飛ばす']
if ((await settingsOrder()).join('|') !== expectedDesktopOrder.join('|')) {
  ng.push(`① デスクトップの設定の順がエンコード版と違う (${(await settingsOrder()).join(', ')})`)
}
if (screenshotDir) {
  await page.screenshot({ path: path.join(screenshotDir, 'desktop-settings.png'), fullPage: true, animations: 'disabled' })
}
await page.setViewportSize({ width: 400, height: 800 })
if ((await settingsOrder()).slice(0, expectedPhoneOrder.length).join('|') !== expectedPhoneOrder.join('|')) {
  ng.push(`① スマホのシートの順がエンコード版と違う (${(await settingsOrder()).join(', ')})`)
}
if (screenshotDir) {
  await page.screenshot({ path: path.join(screenshotDir, 'mobile-settings.png'), fullPage: true, animations: 'disabled' })
}
await page.setViewportSize({ width: 1280, height: 900 })
const audioSettingsRow = settingsMenu.getByRole('menuitem', { name: '音声' })
if (await audioSettingsRow.count() !== 1) {
  ng.push('① 音声の設定項目がメニューにない')
} else {
  const skipRow = settingsMenu.getByRole('menuitemcheckbox', { name: 'CM を飛ばす' })
  if (await skipRow.count() !== 1 || await skipRow.getAttribute('aria-checked') !== 'true') {
    ng.push('① チャプターがあるのに CM 自動スキップが設定にない')
  }
  const masterCountBeforeAudioChange = masterPlaylistRequests.length
  const leaveCountBeforeAudioChange = originalVODLeaveRequests.length
  await audioSettingsRow.click()
  const audioMenu = page.getByRole('menu', { name: '音声' })
  const audioLabels = (await audioMenu.getByRole('menuitemradio').allTextContents()).map((label) => label.trim())
  if (audioLabels.length !== 3 || audioLabels[0] !== '標準' || audioLabels[1] !== '主音声' || audioLabels[2] !== '副音声') {
    ng.push(`① 音声の選択肢が不正 (${audioLabels.join(', ')})`)
  }
  if (screenshotDir) {
    await page.screenshot({ path: path.join(screenshotDir, 'desktop-audio.png'), fullPage: true, animations: 'disabled' })
    await page.setViewportSize({ width: 400, height: 800 })
    await page.screenshot({ path: path.join(screenshotDir, 'mobile-audio.png'), fullPage: true, animations: 'disabled' })
    await page.setViewportSize({ width: 1280, height: 900 })
  }
  await audioMenu.getByRole('menuitemradio', { name: '主音声' }).click()
  await page.waitForTimeout(500)
  await audioSettingsButton.click()
  await page.getByRole('menu', { name: '再生設定' }).getByRole('menuitem', { name: '音声' }).click()
  if (await page.getByRole('menu', { name: '音声' }).getByRole('menuitemradio', { name: '主音声' }).getAttribute('aria-checked') !== 'true') {
    ng.push('① 主音声の選択状態が維持されない')
  }
  await audioSettingsButton.click()
  if (!audioPlaylistRequests.includes('playlist_2.m3u8')) {
    ng.push(`① 主音声の playlist を取得しない (${audioPlaylistRequests.join(', ') || 'none'})`)
  }
  if (masterPlaylistRequests.length !== masterCountBeforeAudioChange) {
    ng.push('① 音声切替で master playlist を取り直した')
  }
  if (originalVODLeaveRequests.length !== leaveCountBeforeAudioChange) {
    ng.push('① 音声切替で HLS セッションを張り直した')
  }
}

log('\n=== ② 実 seek・字幕 cue・保存位置の復元 ===')
await page.waitForFunction(() => {
  const tracks = Array.from(document.querySelector('video')?.textTracks ?? [])
  return tracks.some((track) => track.kind === 'subtitles')
}, undefined, { timeout: 10000 }).catch(() => ng.push('② HLS の WebVTT 字幕レンディションが text track にならない'))
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
  ng.push(`② WebVTT 字幕の cue を読み込めない (${JSON.stringify(cueResult)})`)
}

// 同じ原本 TS から作った 10 秒タイルと HLS 映像を同じ既知時刻で並べて確認する。
await video.evaluate((element) => {
  element.pause()
  element.currentTime = 10.2
})
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && Math.abs(element.currentTime - 10.2) < 0.3
}, undefined, { timeout: 10000 }).catch(() => ng.push('② 既知位置 10.2 秒へ seek できない'))
const alignmentSeekbar = await seekbars.boundingBox()
if (!alignmentSeekbar) {
  ng.push('② タイル位置比較のシークバーを取得できない')
} else {
  await page.mouse.move(
    alignmentSeekbar.x + alignmentSeekbar.width * (10.2 / (recordingDurationMs / 1000)),
    alignmentSeekbar.y + alignmentSeekbar.height / 2,
  )
  await page.getByTestId('seek-tile-preview').waitFor({ timeout: 1500 })
    .catch(() => ng.push('② 既知位置 10.2 秒でタイルプレビューが出ない'))
  const previewLabel = await page.getByTestId('seek-tile-label').textContent().catch(() => null)
  if (!previewLabel?.trim().startsWith('0:10')) {
    ng.push(`② 10.2 秒に 10 秒タイルが対応しない (${previewLabel})`)
  }
  if (screenshotDir) {
    await page.screenshot({ path: path.join(screenshotDir, 'tile-alignment-10s.png'), fullPage: true, animations: 'disabled' })
  }
}

let positionWritesBeforeInRangeSeek = playbackPositionWrites.length
const inRangeSeekbarBox = await seekbars.boundingBox()
if (!inRangeSeekbarBox) {
  ng.push('② 範囲内 seek の操作バーを取得できない')
} else {
  await page.waitForTimeout(300)
  const playlistCountBeforeInRangeSeek = playlistRequests.length
  const masterCountBeforeInRangeSeek = masterPlaylistRequests.length
  const leaveCountBeforeInRangeSeek = originalVODLeaveRequests.length
  positionWritesBeforeInRangeSeek = playbackPositionWrites.length
  const inRangeFraction = 7.25 / (recordingDurationMs / 1000)
  await page.mouse.click(
    inRangeSeekbarBox.x + inRangeSeekbarBox.width * inRangeFraction,
    inRangeSeekbarBox.y + inRangeSeekbarBox.height / 2,
  )
  await page.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && Math.abs(element.currentTime - 7.25) < 1.25
  }, undefined, { timeout: 10000 }).catch(() => ng.push('② シークバーから範囲内へ seek できない'))
  if (playlistRequests.length !== playlistCountBeforeInRangeSeek || masterPlaylistRequests.length !== masterCountBeforeInRangeSeek) {
    ng.push('② 範囲内 seek で HLS playlist を取り直す')
  }
  if (originalVODLeaveRequests.length !== leaveCountBeforeInRangeSeek) {
    ng.push('② 範囲内 seek で HLS セッションを張り直す')
  }
}
await video.evaluate((element) => element.pause())
const positionWriteDeadline = Date.now() + 5000
while (playbackPositionWrites.length <= positionWritesBeforeInRangeSeek && Date.now() < positionWriteDeadline) {
  await page.waitForTimeout(50)
}
const savedPositionMs = recording.resumePositionMs
if (savedPositionMs === undefined || savedPositionMs < 6000 || savedPositionMs > 10_000) {
  ng.push(`② seek 位置がサーバーの再開位置として保存されない (${savedPositionMs})`)
}
const savedPosition = (savedPositionMs ?? 0) / 1000
log(`  保存位置: ${savedPosition}s (${savedPositionMs}ms)`)

const playlistsBeforeReload = playlistRequests.length
await page.reload({ waitUntil: 'domcontentloaded' })
await page.waitForTimeout(750)
if (playlistRequests.length !== playlistsBeforeReload) {
  ng.push('② reload 後、再生ボタンを押す前に original HLS playlist を要求した')
}
const resumePlaybackButton = page.getByTestId('recording-playback-start')
if (await resumePlaybackButton.count() === 1) {
  await resumePlaybackButton.click()
} else {
  ng.push(`② reload 後の再生ボタンが 1 つでない (${await resumePlaybackButton.count()})`)
}
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
const watchedWriteDeadline = Date.now() + 5000
while (watchedWrites.length === 0 && Date.now() < watchedWriteDeadline) {
  await page.waitForTimeout(50)
}
while (watchedWrites.length > 0 && recording.resumePositionMs !== undefined && Date.now() < watchedWriteDeadline) {
  await page.waitForTimeout(50)
}
if (recording.resumePositionMs !== undefined) ng.push(`③ 視聴済み後も再開位置が残る (${recording.resumePositionMs})`)
if (watchedWrites.length === 0 || recording.watchedAt === undefined) ng.push('③ ENDLIST 後の視聴済み印がサーバーに保存されない')
if (segmentRequests.length === 0) ng.push('③ HLS segment を要求していない')
if (subtitleRequests.length === 0) ng.push('③ WebVTT segment を要求していない')
log(`  variant playlists=${playlistRequests.length}, video segments=${segmentRequests.length}, subtitle segments=${subtitleRequests.length}`)

log('\n=== ④ ENDLIST の無い変換中 playlist の先端で ended が発火しない ===')
// live-player.tsx の onEnded は「ended は ENDLIST 済みの終端でしか発火しない」ことに依存する。
// 先頭 4 segment（約 8 秒）で切った ENDLIST 無しの playlist を先端まで再生して確かめる。
growingEdge = true
delete recording.resumePositionMs
const watchedCountBeforeGrowingEdge = watchedWrites.length
const playlistsBeforeGrowingEdge = playlistRequests.length
await page.reload({ waitUntil: 'domcontentloaded' })
await page.waitForTimeout(750)
if (playlistRequests.length !== playlistsBeforeGrowingEdge) {
  ng.push('④ 再生ボタンを押す前に変換中 original HLS playlist を要求した')
}
const growingEdgePlaybackButton = page.getByTestId('recording-playback-start')
if (await growingEdgePlaybackButton.count() === 1) {
  await growingEdgePlaybackButton.click()
} else {
  ng.push(`④ 変換中原本HLSの再生ボタンが 1 つでない (${await growingEdgePlaybackButton.count()})`)
}
await page.locator('video').waitFor({ timeout: 15000 })
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && element.duration > 0
}, undefined, { timeout: 20000 })
await page.locator('video').evaluate(async (element) => {
  element.muted = true
  await element.play()
})
await page.waitForFunction(() => (document.querySelector('video')?.currentTime ?? 0) > 2, undefined, { timeout: 15000 })
  .catch(() => ng.push('④ 切った playlist の再生が始まらない'))
const edge = await page.locator('video').evaluate(async (element) => {
  element.currentTime = Math.max(0, element.duration - 0.8)
  await new Promise((resolve) => setTimeout(resolve, 8000))
  const seekableRanges = Array.from({ length: element.seekable.length }, (_, index) => [
    element.seekable.start(index),
    element.seekable.end(index),
  ])
  const seekableEnd = seekableRanges.at(-1)?.[1] ?? null
  return { ended: element.ended, time: element.currentTime, duration: element.duration, seekableEnd, seekableRanges }
})
log(`  先端到達後 8 秒: ${JSON.stringify(edge)}`)
if (edge.ended) ng.push(`④ ENDLIST の無い先端で ended が発火した (${JSON.stringify(edge)})`)
if (engine === 'webkit' && edge.duration === Infinity) {
  if (edge.seekableEnd === null || edge.seekableEnd > 12) {
    ng.push(`④ native HLS の seekable 範囲が切れていない (${edge.seekableEnd})`)
  }
} else if (edge.duration > 12) {
  ng.push(`④ playlist が切れていない (duration=${edge.duration})`)
}
if (watchedWrites.length !== watchedCountBeforeGrowingEdge) ng.push('④ ENDLIST の無い先端を視聴済みにした')

const offsetSeekbar = page.getByTestId('seek-scrub')
const offsetSeekbarBox = await offsetSeekbar.boundingBox()
if (!offsetSeekbarBox) {
  ng.push('④ offset seek のシークバーが見つからない')
} else {
  const offsetSeekbarY = offsetSeekbarBox.y + offsetSeekbarBox.height / 2
  await page.mouse.move(offsetSeekbarBox.x + offsetSeekbarBox.width * 0.7, offsetSeekbarY)
  await page.mouse.down()
  await page.mouse.move(offsetSeekbarBox.x + offsetSeekbarBox.width * 0.875, offsetSeekbarY)
  await page.waitForTimeout(250)
  if (playlistRequests.includes('offset/14/playlist.m3u8')) {
    ng.push('④ ドラッグ中に offset/14 playlist を要求する')
  }
  await page.mouse.up()
  const offsetDeadline = Date.now() + 10_000
  while (!playlistRequests.includes('offset/14/playlist.m3u8') && Date.now() < offsetDeadline) {
    await page.waitForTimeout(50)
  }
  if (!playlistRequests.includes('offset/14/playlist.m3u8')) {
    ng.push(`④ 変換端より後ろへの seek で offset/14 playlist に張り直さない (${playlistRequests.join(', ')})`)
  } else {
    const segmentDeadline = Date.now() + 10_000
    while (!offsetVideoSegmentRequests.some((request) => request.offsetSeconds === 14) && Date.now() < segmentDeadline) {
      await page.waitForTimeout(50)
    }
    const firstOffsetSegment = offsetVideoSegmentRequests.find((request) => request.offsetSeconds === 14)
    if (firstOffsetSegment?.name !== '0_seg00007.ts') {
      ng.push(`④ offset/14 の先頭 video segment が 14 秒位置のものではない (${firstOffsetSegment?.name ?? 'none'})`)
    }
  }
}

log('\n=== ⑤ 製品と同じ offset セッション（-ss、0 起点、ENDLIST の無い変換中 playlist） ===')
// ①〜④ の fixture は ENDLIST 済みで、offset 付きも元の PTS のまま segment を間引くだけなので、
// WebKit のライブ端への飛び・セッションの張り直し後の再生継続・終端付近の 416 を再現しない。
// ここでは streamer と同じく offset ごとに `-ss {offset} -i` で 0 起点の HLS を作り、
// 変換の先端（20 秒）で切って ENDLIST を外して配る。映像は 60 秒、DB の実尺は 63 秒にして
// 末尾付近のクリックを 416 にする（streamer は「映像の終端 - 0.5 秒」より後ろを 416 にする）。
const offsetFixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-original-vod-offsets')
const offsetSourcePath = path.join(offsetFixtureDir, 'original.ts')
mkdirSync(offsetFixtureDir, { recursive: true })
if (!existsSync(offsetSourcePath)) {
  runFFmpeg([
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', `testsrc2=size=640x360:rate=25:duration=${OFFSET_VIDEO_SECONDS}`,
    '-f', 'lavfi', '-i', `sine=frequency=440:duration=${OFFSET_VIDEO_SECONDS}`,
    '-map', '0:v:0', '-map', '1:a:0', '-c:v', 'mpeg2video', '-b:v', '1200k', '-g', '50',
    '-c:a', 'mp2', '-b:a', '128k', '-f', 'mpegts', offsetSourcePath,
  ], offsetFixtureDir)
}
/** offsetSession は streamer と同じ入力側 seek（-copyts 無し）で 0 起点の HLS を作る。先端の 20 秒だけ。 */
function offsetSession(offset) {
  const dir = path.join(offsetFixtureDir, `offset-${offset}`)
  if (existsSync(path.join(dir, 'playlist.m3u8'))) return dir
  mkdirSync(path.join(dir, 'segments'), { recursive: true })
  runFFmpeg([
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    ...(offset > 0 ? ['-ss', String(offset)] : []), '-i', offsetSourcePath, '-t', '20',
    '-map', '0:v:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '0:a:0',
    '-c:v', 'libx264', '-profile:v', 'baseline', '-pix_fmt', 'yuv420p', '-preset', 'veryfast', '-g', '50',
    '-sc_threshold', '0', '-force_key_frames', 'expr:gte(t,n_forced*2)', '-c:a', 'aac', '-b:a', '64k',
    '-var_stream_map', 'v:0,agroup:a0 a:0,agroup:a0,default:yes a:1,agroup:a0 a:2,agroup:a0',
    '-master_pl_name', 'playlist.m3u8', '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0',
    '-hls_playlist_type', 'event', '-hls_base_url', 'segments/',
    '-hls_segment_filename', path.join(dir, 'segments', '%v_seg%05d.ts'), path.join(dir, 'playlist_%v.m3u8'),
  ], dir)
  return dir
}
const offsetRequests = []
let offsetFailAll = false
const offsetHandler = async ({ path: requestPath, json, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json([SITE])
  if (requestPath === '/api/capabilities') return json({ live: true, cmDetect: false })
  if (requestPath === '/api/breakers' || requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/live-profiles') return json([{ name: 'hd', height: 720 }, { name: 'sd', height: 480 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([offsetRecording])
  if (requestPath === `/api/recordings/${OFFSET_ID}` && method === 'GET') return json(offsetRecording)
  if (requestPath === `/api/recordings/${OFFSET_ID}/chapters`) {
    return json({
      version: 'chapters-offsets',
      detectionPending: false,
      source: 'auto',
      spans: [{ startMs: 40_000, endMs: 45_000, label: '気象情報', cut: false }],
    })
  }
  if (requestPath.startsWith(`/api/recordings/${OFFSET_ID}/`) && method !== 'GET') return route.fulfill({ status: 204 })
  if (requestPath === `/api/media/recordings/${OFFSET_ID}/seek-tiles`) return route.fulfill({ status: 404 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) return route.fulfill({ status: 404 })
  if (requestPath.startsWith(`/api/sites/${SITE}/recordings/${OFFSET_ID}/original-vod/`)) {
    const relative = requestPath.split('/original-vod/')[1]
    if (relative.endsWith('leave')) return route.fulfill({ status: 204 })
    const match = relative.match(/^offset\/(\d+)\//)
    const offset = match ? Number(match[1]) : 0
    const resource = match ? relative.slice(match[0].length) : relative
    if (offsetFailAll) return route.fulfill({ status: 500, contentType: 'text/plain', body: 'stub failure\n' })
    if (offset > OFFSET_PLAYABLE_END - 0.5) {
      if (resource === 'playlist.m3u8') offsetRequests.push({ offset, status: 416 })
      return route.fulfill({ status: 416, contentType: 'text/plain', body: 'offset is outside the original\n' })
    }
    if (resource === 'playlist.m3u8') offsetRequests.push({ offset, status: 200 })
    const file = path.join(offsetSession(offset), resource)
    if (!existsSync(file)) return route.fulfill({ status: 404, body: 'fixture missing' })
    let body = readFileSync(file)
    // 変換中: ENDLIST を外す（-t 20 で先端は 20 秒）。
    if (/^playlist_\d+\.m3u8$/.test(resource)) body = body.toString('utf8').replace('#EXT-X-ENDLIST\n', '')
    return route.fulfill({
      status: 200,
      contentType: resource.endsWith('.ts') ? 'video/mp2t' : 'application/vnd.apple.mpegurl',
      body,
    })
  }
  return json([])
}

/** sampleOffsetPlayer は原本時間軸の位置・再生状態・バーの表示を読む。 */
const sampleOffsetPlayer = (target) => target.evaluate(() => {
  const element = document.querySelector('video')
  const slider = document.querySelector('[role="slider"][aria-label="シークバー"]')
  const controls = document.querySelector('[data-testid="player-controls"]')
  const play = Array.from(document.querySelectorAll('[data-testid="player-controls"] button'))
    .find((button) => ['再生', '一時停止'].includes(button.getAttribute('aria-label') ?? '') && button.getBoundingClientRect().width > 0)
  return {
    time: element?.currentTime ?? null,
    paused: element?.paused ?? null,
    position: Number(slider?.getAttribute('aria-valuenow') ?? NaN),
    controlsOpacity: controls ? getComputedStyle(controls).opacity : null,
    playLabel: play?.getAttribute('aria-label') ?? null,
    error: /エラー/.test(document.querySelector('[data-testid="recording-player-frame"]')?.textContent ?? ''),
  }
})

log('\n=== ⑤-manual 手動の ▶ は、待ち時間 0 / 1 / 3 秒でも変換中 playlist の先頭から始まる ===')
const manualPlayMeasurements = []
for (const waitMs of [0, 1000, 3000]) {
  delete offsetRecording.resumePositionMs
  delete offsetRecording.watchedAt
  const manualPage = await context.newPage()
  await installApiStubs(manualPage, offsetHandler)
  // Poster start mounts the source and asks for playback, but a browser can reject
  // that asynchronous request after the click's user activation has expired. Model
  // that policy result so the toolbar's manual ▶ is the first successful play().
  await manualPage.addInitScript(() => {
    const play = HTMLMediaElement.prototype.play
    let blocked = false
    HTMLMediaElement.prototype.play = function (...args) {
      if (!blocked && this instanceof HTMLVideoElement) {
        blocked = true
        window.__e2eInitialVideoPlayBlocked = true
        return Promise.reject(new DOMException('autoplay blocked for manual-play coverage', 'NotAllowedError'))
      }
      return play.apply(this, args)
    }
  })
  await manualPage.goto(`${URL_BASE}/recordings/${OFFSET_ID}`, { waitUntil: 'domcontentloaded' })
  await manualPage.getByTestId('recording-playback-start').click()
  await manualPage.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && element.duration > 0 && element.readyState >= HTMLMediaElement.HAVE_FUTURE_DATA &&
      element.paused && window.__e2eInitialVideoPlayBlocked === true
  }, undefined, { timeout: 20000 }).catch(() => ng.push(`⑤-manual ${waitMs}ms: 手動再生用に HLS を読み込めない`))
  await manualPage.locator('video').evaluate((element) => {
    element.pause()
    element.currentTime = 0
  })
  await manualPage.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && element.paused && element.readyState >= HTMLMediaElement.HAVE_FUTURE_DATA &&
      Math.abs(element.currentTime) < 0.25
  }, undefined, { timeout: 10000 }).catch(() => ng.push(`⑤-manual ${waitMs}ms: 手動再生前に 0 秒で一時停止できない`))
  if (waitMs > 0) await manualPage.waitForTimeout(waitMs)
  const beforeManualPlay = await sampleOffsetPlayer(manualPage)
  const manualPlayRequestedAt = Date.now()
  await manualPage.locator('[data-testid="player-controls"]')
    .getByRole('button', { name: '再生', exact: true }).click()
  await manualPage.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && !element.paused && element.currentTime > 0.25
  }, undefined, { timeout: 10000 }).catch(() => ng.push(`⑤-manual ${waitMs}ms: 手動の ▶ で再生が始まらない`))
  const untilMeasurement = manualPlayRequestedAt + 1000 - Date.now()
  if (untilMeasurement > 0) await manualPage.waitForTimeout(untilMeasurement)
  const afterManualPlay = await sampleOffsetPlayer(manualPage)
  const measurement = { waitMs, before: beforeManualPlay.time, after: afterManualPlay.time, paused: afterManualPlay.paused }
  manualPlayMeasurements.push(measurement)
  log(`  手動 ▶ 待ち ${waitMs}ms、押下前 ${beforeManualPlay.time?.toFixed(2)}s → 1 秒後 ${afterManualPlay.time?.toFixed(2)}s`)
  if (afterManualPlay.time === null || afterManualPlay.time > 4 || afterManualPlay.paused !== false) {
    ng.push(`⑤-manual ${waitMs}ms 待って押した手動の ▶ が 0 秒付近から始まらない（${JSON.stringify(measurement)}）`)
  }
  await manualPage.close()
}
log(`  実測値: ${JSON.stringify(manualPlayMeasurements)}`)
delete offsetRecording.resumePositionMs
delete offsetRecording.watchedAt

{
  const offsetContext = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'ja-JP' })
  const offsetPage = await offsetContext.newPage()
  await installApiStubs(offsetPage, offsetHandler)
  await offsetPage.goto(`${URL_BASE}/recordings/${OFFSET_ID}`, { waitUntil: 'domcontentloaded' })
  // ⑤-a 先頭（offset 0・続きから位置なし）をポスターの ▶ で再生すると、手で play() を呼ばなくても
  // 再生が始まり、0 から始まる。WebKit のネイティブ HLS は開始位置を明示しないと ENDLIST の無い
  // EVENT playlist のライブ端近くから始める。
  await offsetPage.getByTestId('recording-playback-start').click()
  await offsetPage.locator('video').waitFor({ timeout: 15000 })
  await offsetPage.mouse.move(5, 5)
  await offsetPage.waitForTimeout(2500)
  const afterStart = await sampleOffsetPlayer(offsetPage)
  log(`  ポスターの ▶ の 2.5 秒後: ${JSON.stringify(afterStart)}`)
  if (afterStart.time === null || afterStart.time > 4 || afterStart.paused !== false || !(afterStart.time > 0.5)) {
    ng.push(`⑤-a ポスターの ▶ で再生が始まらないか 0 から始まらない（${JSON.stringify(afterStart)}）`)
  }
  // ⑤-b バーの ▶ をマウスで押した後もフォーカスでバーを出したままにしない（chapters.mjs ⑥-a と同じ）。
  const startFrameBox = await offsetPage.getByTestId('recording-player-frame').boundingBox()
  await offsetPage.mouse.move(startFrameBox.x + startFrameBox.width / 2, startFrameBox.y + startFrameBox.height / 2)
  await offsetPage.waitForTimeout(300)
  await offsetPage.locator('[data-testid="player-controls"]').getByRole('button', { name: '一時停止', exact: true }).click()
  await offsetPage.locator('[data-testid="player-controls"]').getByRole('button', { name: '再生', exact: true }).click()
  await offsetPage.mouse.move(5, 5)
  await offsetPage.waitForTimeout(1500)
  await offsetPage.waitForTimeout(3000)
  const afterIdle = await sampleOffsetPlayer(offsetPage)
  if (afterIdle.controlsOpacity !== '0') {
    ng.push(`⑤-b バーの ▶ をマウスで押した後、4.5 秒待ってもバーが隠れない（${JSON.stringify(afterIdle)}）`)
  }
  // ⑤-c 映像クリックで再生 → バーが隠れた後に Tab でバーへ届く（隠れたバーは inert。chapters.mjs ⑥-b と同じ）。
  await offsetPage.locator('video').evaluate((element) => element.pause())
  await offsetPage.locator('video').click()
  await offsetPage.mouse.move(5, 5)
  await offsetPage.waitForTimeout(4500)
  const hiddenBeforeTab = await sampleOffsetPlayer(offsetPage)
  if (hiddenBeforeTab.controlsOpacity !== '0' || hiddenBeforeTab.paused) {
    ng.push(`⑤-c 前提: 映像クリックで再生した後にバーが隠れていない（${JSON.stringify(hiddenBeforeTab)}）`)
  }
  await offsetPage.keyboard.press('Tab')
  await offsetPage.waitForTimeout(400)
  const afterTab = await offsetPage.evaluate(() => Boolean(document.activeElement?.closest('[data-testid="player-controls"]')))
  if (!afterTab || (await sampleOffsetPlayer(offsetPage)).controlsOpacity !== '1') {
    ng.push('⑤-c 再生中に Tab を押してもバーに届かない')
  }

  // ⑤-d 再生中にセッション外（変換の先端 20 秒より後ろ）へシークしても再生が続き、ボタンも一時停止のまま。
  const frameBox = await offsetPage.getByTestId('recording-player-frame').boundingBox()
  await offsetPage.mouse.move(frameBox.x + frameBox.width / 2, frameBox.y + frameBox.height / 2)
  const scrub = offsetPage.getByTestId('seek-scrub')
  const scrubBox = await scrub.boundingBox()
  const requestsBeforeSeek = offsetRequests.length
  await offsetPage.evaluate(() => { window.__e2eVideoBeforeRangeExit = document.querySelector('video') })
  await offsetPage.mouse.click(scrubBox.x + scrubBox.width * 0.5, scrubBox.y + scrubBox.height / 2)
  await offsetPage.waitForTimeout(2500)
  const afterOffsetSeek = await sampleOffsetPlayer(offsetPage)
  const seekRequests = offsetRequests.slice(requestsBeforeSeek)
  log(`  50% シークの 2.5 秒後: ${JSON.stringify(afterOffsetSeek)} 要求=${JSON.stringify(seekRequests)}`)
  if (!seekRequests.some((request) => request.offset === 31 && request.status === 200)) {
    ng.push(`⑤-d 50%（31.5 秒）のシークで offset/31 に張り直さない（${JSON.stringify(seekRequests)}）`)
  }
  // 再生元が同じ（原本 HLS のまま）なら video を作り直さない。作り直すと一時停止・全画面解除になる。
  if (!(await offsetPage.evaluate(() => window.__e2eVideoBeforeRangeExit === document.querySelector('video')))) {
    ng.push('⑤-d 原本 HLS のまま範囲外へシークしたら video を作り直した')
  }
  if (afterOffsetSeek.paused !== false || afterOffsetSeek.playLabel !== '一時停止') {
    ng.push(`⑤-d 再生中のセッション外シークで再生が止まるか、ボタンが再生中を示さない（${JSON.stringify(afterOffsetSeek)}）`)
  }
  if (!(afterOffsetSeek.position >= 31 && afterOffsetSeek.position < 36)) {
    ng.push(`⑤-d セッション外シーク後の位置が 31.5 秒付近でない（${afterOffsetSeek.position}）`)
  }

  // ⑤-e 次のチャプター / 前のチャプター（境界は 40 秒と 45 秒）。
  await offsetPage.mouse.move(frameBox.x + frameBox.width / 2, frameBox.y + frameBox.height / 2 + 10)
  await offsetPage.getByRole('button', { name: '次のチャプター' }).click()
  await offsetPage.waitForTimeout(800)
  const afterNext = await sampleOffsetPlayer(offsetPage)
  if (!(afterNext.position >= 39.5 && afterNext.position < 42)) {
    ng.push(`⑤-e 次のチャプターで 40 秒へ飛ばない（${afterNext.position}）`)
  }
  await offsetPage.getByRole('button', { name: '次のチャプター' }).click()
  await offsetPage.waitForTimeout(300)
  await offsetPage.getByRole('button', { name: '前のチャプター' }).click()
  await offsetPage.waitForTimeout(800)
  const afterPrev = await sampleOffsetPlayer(offsetPage)
  if (!(afterPrev.position >= 39.5 && afterPrev.position < 42)) {
    ng.push(`⑤-e 45 秒から前のチャプターで 40 秒へ戻らない（${afterPrev.position}）`)
  }

  // ⑤-f 末尾付近（99% = 62.4 秒。映像は 60 秒）のクリックは 416 になる。エラーにせず、
  // 有効な最後の offset へ丸めて再生する。
  // マウスで押したボタンのフォーカスでは出したままにしないので、バーを出してから押す。
  // ボタンの click で頁がスクロールしうるので、帯の位置は測り直す。
  await offsetPage.mouse.move(frameBox.x + frameBox.width / 2, frameBox.y + frameBox.height / 2)
  await offsetPage.waitForTimeout(300)
  const endScrubBox = await scrub.boundingBox()
  const requestsBeforeEnd = offsetRequests.length
  await offsetPage.mouse.click(endScrubBox.x + endScrubBox.width * 0.99, endScrubBox.y + endScrubBox.height / 2)
  const endDeadline = Date.now() + 15_000
  while (!offsetRequests.slice(requestsBeforeEnd).some((request) => request.status === 200) && Date.now() < endDeadline) {
    await offsetPage.waitForTimeout(100)
  }
  await offsetPage.waitForTimeout(1500)
  const nearEnd = await sampleOffsetPlayer(offsetPage)
  const endRequests = offsetRequests.slice(requestsBeforeEnd)
  log(`  99% クリック: ${JSON.stringify(nearEnd)} 要求=${JSON.stringify(endRequests)}`)
  const landed = endRequests.find((request) => request.status === 200)
  if (nearEnd.error || landed === undefined || landed.offset < OFFSET_PLAYABLE_END - 8) {
    ng.push(`⑤-f 末尾付近の 416 を有効な最後の offset へ丸めない（${JSON.stringify({ nearEnd, endRequests })}）`)
  }
  // 416 を挟んで張り直しても、再生中だったなら再生を続ける。
  if (nearEnd.paused !== false || nearEnd.playLabel !== '一時停止') {
    ng.push(`⑤-f 416 を丸めて張り直した後に再生が止まる（${JSON.stringify(nearEnd)}）`)
  }
  if (screenshotDir) {
    await offsetPage.mouse.move(endScrubBox.x + endScrubBox.width / 2, endScrubBox.y - 40)
    await offsetPage.screenshot({ path: path.join(screenshotDir, 'desktop-near-end.png'), animations: 'disabled' })
  }
  await offsetContext.close()
}

{
  // ⑤-g スマホ: 再生中に映像をタップすると操作が出て、再生は止まらない（chapters.mjs ⑧ と同じ）。
  // ⑤-h エラー表示と「再読み込み」が操作の幕の下に隠れず押せる。
  const phoneContext = await browser.newContext({
    viewport: { width: 400, height: 860 },
    hasTouch: true,
    isMobile: engine !== 'firefox',
    locale: 'ja-JP',
  })
  const phonePage = await phoneContext.newPage()
  await installApiStubs(phonePage, offsetHandler)
  await phonePage.goto(`${URL_BASE}/recordings/${OFFSET_ID}`, { waitUntil: 'domcontentloaded' })
  await phonePage.getByTestId('recording-playback-start').tap()
  await phonePage.locator('video').waitFor({ timeout: 15000 })
  await phonePage.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && element.readyState >= 2 && !element.paused
  }, undefined, { timeout: 30000 }).catch(() => ng.push('⑤-g スマホでポスターの ▶ を押しても再生が始まらない'))
  await phonePage.waitForTimeout(3800)
  const hiddenWhilePlaying = await sampleOffsetPlayer(phonePage)
  const phoneFrame = await phonePage.getByTestId('recording-player-frame').boundingBox()
  await phonePage.touchscreen.tap(phoneFrame.x + 30, phoneFrame.y + 30)
  await phonePage.waitForTimeout(400)
  const afterTap = await sampleOffsetPlayer(phonePage)
  if (hiddenWhilePlaying.controlsOpacity !== '0') {
    ng.push(`⑤-g 前提: スマホで再生中に操作が隠れない（${JSON.stringify(hiddenWhilePlaying)}）`)
  } else if (afterTap.controlsOpacity !== '1' || afterTap.paused) {
    ng.push(`⑤-g スマホで再生中に映像をタップしても操作が出ないか、再生が止まった（${JSON.stringify(afterTap)}）`)
  } else {
    // 暗い幕のタップで操作が隠れ、再生は止まらない（幕は click で閉じる。chapters.mjs ⑧）。
    await phonePage.touchscreen.tap(phoneFrame.x + 30, phoneFrame.y + 30)
    await phonePage.waitForTimeout(400)
    const afterScrimTap = await sampleOffsetPlayer(phonePage)
    if (afterScrimTap.controlsOpacity !== '0' || afterScrimTap.paused) {
      ng.push(`⑤-g スマホで暗い幕をタップしても操作が隠れないか、再生が止まった（${JSON.stringify(afterScrimTap)}）`)
    }
  }
  offsetFailAll = true
  // 操作を出してから中央の次のチャプター（40 秒。変換の先端より先）を押し、張り直しを失敗させる。
  if ((await sampleOffsetPlayer(phonePage)).controlsOpacity !== '1') {
    await phonePage.touchscreen.tap(phoneFrame.x + 30, phoneFrame.y + 30)
    await phonePage.waitForTimeout(400)
  }
  await phonePage.locator('[data-testid="player-controls"]').getByRole('button', { name: '次のチャプター' }).tap().catch(() => {})
  await phonePage.getByRole('button', { name: '再読み込み' }).waitFor({ timeout: 10_000 })
    .catch(() => ng.push('⑤-h 前提: 配信の失敗でエラー表示が出ない'))
  const reloadOnTop = await phonePage.evaluate(() => {
    const button = Array.from(document.querySelectorAll('button')).find((element) => element.textContent?.trim() === '再読み込み')
    if (!button) return null
    const rect = button.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
    return hit === button || button.contains(hit)
  })
  if (reloadOnTop !== true) ng.push(`⑤-h 400px で「再読み込み」が操作の幕の下に隠れて押せない（${reloadOnTop}）`)
  if (screenshotDir) await phonePage.screenshot({ path: path.join(screenshotDir, 'mobile-error.png'), animations: 'disabled' })
  offsetFailAll = false
  await phoneContext.close()
}

{
  // ⑤-i md 未満の幅でもマウスで映像（操作の幕）を押すと再生 / 一時停止する（chapters.mjs ⑨ と同じ）。
  const narrowContext = await browser.newContext({ viewport: { width: 600, height: 900 }, locale: 'ja-JP' })
  const narrowPage = await narrowContext.newPage()
  await installApiStubs(narrowPage, offsetHandler)
  await narrowPage.goto(`${URL_BASE}/recordings/${OFFSET_ID}`, { waitUntil: 'domcontentloaded' })
  await narrowPage.getByTestId('recording-playback-start').click()
  await narrowPage.locator('video').waitFor({ timeout: 15000 })
  await narrowPage.waitForFunction(() => {
    const element = document.querySelector('video')
    return element !== null && element.readyState >= 2 && !element.paused
  }, undefined, { timeout: 30000 }).catch(() => ng.push('⑤-i 600px でポスターの ▶ を押しても再生が始まらない'))
  const narrowFrame = await narrowPage.getByTestId('recording-player-frame').boundingBox()
  await narrowPage.mouse.click(narrowFrame.x + 30, narrowFrame.y + 30)
  await narrowPage.waitForTimeout(400)
  // 再生中に映像を押すと一時停止する（押して再生に切り替わる方向は ⑤-c が見る）。
  if (!(await sampleOffsetPlayer(narrowPage)).paused) ng.push('⑤-i md 未満の幅でマウスで映像を押しても一時停止しない')
  await narrowContext.close()
}

log('\n=== ⑥ エンコード完了時は HLS を保ち、次の範囲外 seek で encoded へ移る ===')
delete recording.resumePositionMs
recording.encodedAssets = []
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && element.duration > 0 && element.readyState >= HTMLMediaElement.HAVE_METADATA
}, undefined, { timeout: 20000 }).catch(() => ng.push('⑥ encode transition前に original HLS metadata が揃わない'))
await page.locator('video').evaluate(async (element) => {
  element.muted = true
  await element.play()
})
await page.waitForFunction(() => {
  const element = document.querySelector('video')
  return element !== null && element.currentTime > 1 && !element.paused
}, undefined, { timeout: 10000 }).catch(() => ng.push('⑥ encoding update前の original HLS 再生が始まらない'))
const originalSourceBeforeEncoding = await page.locator('video').evaluate((element) => {
  window.__e2eOriginalVideoBeforeEncoding = element
  return element.currentSrc
})
const playlistCountBeforeEncoding = playlistRequests.length
const encodedCountBeforeEncoding = encodedRequests.length
const detailRequestsBeforeEncoding = recordingDetailRequests
recording.encodedAssets = [{ profile: PLAYBACK_PROFILE, sizeBytes: 400_000 }]
recording.sizeBytes = 1_000_000
await page.evaluate(() => window.__emitE2EEvent('recordings'))
const encodingRefreshDeadline = Date.now() + 5000
while (recordingDetailRequests === detailRequestsBeforeEncoding && Date.now() < encodingRefreshDeadline) {
  await page.waitForTimeout(50)
}
if (recordingDetailRequests === detailRequestsBeforeEncoding) {
  ng.push('⑥ recordings SSE後に encoding result を含む詳細を再取得しない')
}
await page.waitForTimeout(750)
const encodingTransitionState = await page.evaluate(() => {
  const element = document.querySelector('video')
  return {
    sameVideo: element !== null && element === window.__e2eOriginalVideoBeforeEncoding,
    currentSrc: element?.currentSrc,
    currentTime: element?.currentTime,
    paused: element?.paused,
    videoCount: document.querySelectorAll('[data-testid="recording-playback-group"] video').length,
  }
})
log(
  `  encoding更新後: detailGET ${detailRequestsBeforeEncoding}→${recordingDetailRequests}, sameVideo=${encodingTransitionState.sameVideo}, src=${encodingTransitionState.currentSrc}, original playlists=${playlistCountBeforeEncoding}→${playlistRequests.length}, encoded requests=${encodedCountBeforeEncoding}→${encodedRequests.length}`,
)
if (!encodingTransitionState.sameVideo || encodingTransitionState.videoCount !== 1 ||
  encodingTransitionState.currentSrc !== originalSourceBeforeEncoding || encodingTransitionState.paused ||
  encodedRequests.length !== encodedCountBeforeEncoding) {
  ng.push('⑥ encoding状態更新だけで original HLS 再生元を替えた')
}

const transitionSeekTarget = 12
const transitionSeekbar = page.getByTestId('seek-scrub')
const transitionSeekbarBox = await transitionSeekbar.boundingBox()
if (!transitionSeekbarBox) {
  ng.push('⑥ encoding後の範囲外 seek 操作バーが無い')
} else {
  const axisMin = Number(await transitionSeekbar.getAttribute('aria-valuemin'))
  const axisMax = Number(await transitionSeekbar.getAttribute('aria-valuemax'))
  const seekX = transitionSeekbarBox.x + ((transitionSeekTarget - axisMin) / (axisMax - axisMin)) * transitionSeekbarBox.width
  const seekY = transitionSeekbarBox.y + transitionSeekbarBox.height / 2
  const encodedCountBeforeRangeExit = encodedRequests.length
  await page.mouse.move(seekX, seekY)
  await page.mouse.click(seekX, seekY)
  const encodedSwitchDeadline = Date.now() + 10000
  while (encodedRequests.length === encodedCountBeforeRangeExit && Date.now() < encodedSwitchDeadline) {
    await page.waitForTimeout(50)
  }
  await page.waitForFunction((target) => {
    const element = document.querySelector('video')
    return element !== null && element.currentSrc.includes('/file') && Math.abs(element.currentTime - target) < 1.5
  }, transitionSeekTarget, { timeout: 10000 }).catch(async () => {
    const state = await page.locator('video').evaluate((element) => ({
      currentSrc: element.currentSrc,
      currentTime: element.currentTime,
      duration: element.duration,
      readyState: element.readyState,
      seekable: Array.from({ length: element.seekable.length }, (_, index) => [
        element.seekable.start(index), element.seekable.end(index),
      ]),
    }))
    ng.push(`⑥ 範囲外 seek 後に encoded MP4 へ位置を持ち越さない (${JSON.stringify(state)})`)
  })
  log(`  range-exit encoded requests=${encodedRequests.length - encodedCountBeforeRangeExit}, axis=${await transitionSeekbar.getAttribute('aria-valuenow')}`)
  if (encodedRequests.length === encodedCountBeforeRangeExit) {
    ng.push('⑥ original HLS 範囲外 seek で encoded MP4 を要求しない')
  }
}

log('\n=== ⑦ 再生元ごとの再生前後で枠の寸法が変わらない（1280 / 400） ===')
const sourceShotDir = process.env.E2E_SHOT_DIR
/** playbackFrameBox は再生前のポスターか再生後のプレイヤー枠の寸法と、再生の塊の高さを返す。 */
const playbackFrameBox = () => page.evaluate(() => {
  const frame = document.querySelector('[data-testid="recording-playback-poster"]') ??
    document.querySelector('[data-testid="recording-player-frame"]')
  const rect = frame.getBoundingClientRect()
  return {
    width: rect.width,
    height: rect.height,
    top: rect.top,
    groupHeight: document.querySelector('[data-testid="recording-playback-group"]').getBoundingClientRect().height,
  }
})
for (const [label, viewport] of [['1280', { width: 1280, height: 900 }], ['400', { width: 400, height: 860 }]]) {
  await page.setViewportSize(viewport)
  for (const source of ['original-hls', 'encoded']) {
    recording.encodedAssets = source === 'encoded' ? [{ profile: PLAYBACK_PROFILE, sizeBytes: 400_000 }] : []
    delete recording.resumePositionMs
    await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
    await page.waitForSelector('[data-testid="recording-playback-poster"], [data-testid="recording-player-frame"]', { timeout: 15000 })
    await page.waitForTimeout(500)
    const before = await playbackFrameBox()
    if (sourceShotDir) await page.screenshot({ path: path.join(sourceShotDir, `fix-${source}-before-${label}.png`), animations: 'disabled' })
    if (source === 'original-hls') {
      await page.getByTestId('recording-playback-start').click()
    } else {
      await page.locator('[data-testid="player-controls"]').getByRole('button', { name: '再生', exact: true }).click()
    }
    await page.waitForFunction(() => {
      const element = document.querySelector('video')
      return element !== null && !element.paused && element.currentTime > 0.5
    }, undefined, { timeout: 15000 }).catch(() => ng.push(`⑦(${source}/${label}) 再生ボタンを押しても再生が進まない`))
    await page.mouse.move(200, 5)
    await page.waitForTimeout(500)
    if (sourceShotDir) await page.screenshot({ path: path.join(sourceShotDir, `fix-${source}-after-${label}.png`), animations: 'disabled' })
    const after = await playbackFrameBox()
    log(`  ${source}/${label}: 前 ${JSON.stringify(before)} 後 ${JSON.stringify(after)}`)
    if (Math.abs(after.width - before.width) > 1 || Math.abs(after.height - before.height) > 1 ||
      Math.abs(after.groupHeight - before.groupHeight) > 1 || Math.abs(after.top - before.top) > 1) {
      ng.push(`⑦(${source}/${label}) 再生の前後で枠の寸法・位置が変わる（前 ${JSON.stringify(before)} 後 ${JSON.stringify(after)}）`)
    }
  }
}

await finish(ng, browser)
