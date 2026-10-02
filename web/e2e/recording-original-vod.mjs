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
// ビットマップ字幕を作れない）。配る playlist は最初から ENDLIST 済みなので、
// 変換中に伸びる playlist を追う挙動も未検証（それは live-player のユニットテストが信号だけ見る）。
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

function runFFmpeg(args, cwd) {
  execFileSync('ffmpeg', args, { cwd, stdio: 'pipe' })
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
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)
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
const playbackPositionWrites = []
const watchedWrites = []
const seekTileRequests = []
const masterPlaylistRequests = []
const audioPlaylistRequests = []
const offsetVideoSegmentRequests = []
const originalVODLeaveRequests = []
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
  if (requestPath === '/api/live-profiles') return json([{ name: 'h264', height: 360 }])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (requestPath === '/api/recordings' && method === 'GET') return json([recording])
  if (requestPath === `/api/recordings/${RECORDING_ID}` && method === 'GET') return json(recording)
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
const originalRegion = page.getByRole('region', { name: '原本 TS をブラウザ再生' })
await originalRegion.waitFor({ timeout: 15000 })
// 映像の上に見出しを置かない（docs/frontend/recordings.md「録画詳細の面積配分と構成」）。
if ((await originalRegion.getByRole('heading').count()) !== 0) ng.push('① 原本 VOD の映像の上に見出しがある')
const video = page.locator('video')
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

const audioSettingsButton = page.getByRole('button', { name: '再生設定' })
await audioSettingsButton.click()
const screenshotDir = process.env.E2E_SCREENSHOT_DIR
if (screenshotDir) {
  mkdirSync(screenshotDir, { recursive: true })
  await page.screenshot({ path: path.join(screenshotDir, 'desktop.png'), fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 400, height: 800 })
  await page.screenshot({ path: path.join(screenshotDir, 'mobile.png'), fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 1280, height: 900 })
}
const settingsMenu = page.getByRole('menu', { name: '再生設定' })
const audioSettingsRow = settingsMenu.getByRole('menuitem', { name: '音声' })
if (await audioSettingsRow.count() !== 1) {
  ng.push('① 音声の設定項目がメニューにない')
} else {
  const masterCountBeforeAudioChange = masterPlaylistRequests.length
  const leaveCountBeforeAudioChange = originalVODLeaveRequests.length
  await audioSettingsRow.click()
  const audioMenu = page.getByRole('menu', { name: '音声' })
  const audioLabels = (await audioMenu.getByRole('menuitemradio').allTextContents()).map((label) => label.trim())
  if (audioLabels.length !== 3 || audioLabels[0] !== '標準' || audioLabels[1] !== '主音声' || audioLabels[2] !== '副音声') {
    ng.push(`① 音声の選択肢が不正 (${audioLabels.join(', ')})`)
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

const inRangeSeekbarBox = await seekbars.boundingBox()
if (!inRangeSeekbarBox) {
  ng.push('② 範囲内 seek の操作バーを取得できない')
} else {
  await page.waitForTimeout(300)
  const playlistCountBeforeInRangeSeek = playlistRequests.length
  const masterCountBeforeInRangeSeek = masterPlaylistRequests.length
  const leaveCountBeforeInRangeSeek = originalVODLeaveRequests.length
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
while (playbackPositionWrites.length === 0 && Date.now() < positionWriteDeadline) {
  await page.waitForTimeout(50)
}
const savedPositionMs = recording.resumePositionMs
if (savedPositionMs === undefined || savedPositionMs < 6000 || savedPositionMs > 10_000) {
  ng.push(`② seek 位置がサーバーの再開位置として保存されない (${savedPositionMs})`)
}
const savedPosition = (savedPositionMs ?? 0) / 1000
log(`  保存位置: ${savedPosition}s (${savedPositionMs}ms)`)

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
await page.goto(`${URL_BASE}/recordings/${RECORDING_ID}`, { waitUntil: 'domcontentloaded' })
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

await finish(ng, browser)
