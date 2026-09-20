// 録画中の追っかけ再生（issue #579）の実ブラウザ判定。
//
// jsdom では測れないものだけを見る。録画中の録画詳細へ `#chase` で入り、
// EVENT playlist が成長する間も hls.js が先頭から再生を始め、`最新` で末尾へ
// 移動できることを、Chromium + 実 HLS セグメントで確認する。mirakc / DB の
// 録画ファイルは使わず、録画 API と HLS は page.route で差し替える。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:chase
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync } from 'node:fs'
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

const recording = {
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'テスト局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '録画中の番組',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'recording',
  keepOriginal: 'always',
  startedAt: '2026-01-01T12:00:00Z',
  createdAt: '2026-01-01T12:00:00Z',
}

/** ffmpeg の実 H.264/AAC セグメントを一度だけ作る。 */
function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-chase-fixture')
  const playlistPath = path.join(fixtureDir, 'playlist.m3u8')
  if (existsSync(playlistPath)) {
    const segments = readFileSync(playlistPath, 'utf8').match(/^#EXTINF:/gm) ?? []
    if (segments.length >= 3) return fixtureDir
  }

  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }

  mkdirSync(path.join(fixtureDir, 'segments'), { recursive: true })
  log(`追っかけ用 HLS フィクスチャを生成中... (${fixtureDir})`)
  execFileSync(
    'ffmpeg',
    [
      '-y',
      '-f',
      'lavfi',
      '-i',
      'testsrc=size=640x360:rate=25',
      '-f',
      'lavfi',
      '-i',
      'sine=frequency=440',
      '-t',
      '12',
      '-c:v',
      'libx264',
      '-profile:v',
      'baseline',
      '-level',
      '3.0',
      '-g',
      '50',
      '-keyint_min',
      '50',
      '-sc_threshold',
      '0',
      '-pix_fmt',
      'yuv420p',
      '-preset',
      'veryfast',
      '-c:a',
      'aac',
      '-b:a',
      '64k',
      '-f',
      'hls',
      '-hls_time',
      '2',
      '-hls_list_size',
      '0',
      '-hls_flags',
      'independent_segments',
      '-hls_base_url',
      'segments/',
      '-hls_segment_filename',
      'segments/segment_%03d.ts',
      'playlist.m3u8',
    ],
    { cwd: fixtureDir, stdio: 'ignore' },
  )
  return existsSync(playlistPath) ? fixtureDir : undefined
}

function eventPlaylists(fixtureDir) {
  const lines = readFileSync(path.join(fixtureDir, 'playlist.m3u8'), 'utf8')
    .split(/\r?\n/)
    .filter(Boolean)
  const firstEntry = lines.findIndex((line) => line.startsWith('#EXTINF:'))
  const header = lines
    .slice(0, firstEntry)
    .filter((line) => !line.startsWith('#EXT-X-PLAYLIST-TYPE:'))
  const entries = []
  for (let i = firstEntry; i < lines.length; i += 1) {
    if (!lines[i].startsWith('#EXTINF:')) continue
    entries.push([lines[i], lines[i + 1]])
  }
  const playlist = (count, end) =>
    [...header, '#EXT-X-PLAYLIST-TYPE:EVENT', ...entries.slice(0, count).flat(), ...(end ? ['#EXT-X-ENDLIST'] : [])].join(
      '\n',
    ) + '\n'
  return { entries, playlist }
}

log(`URL: ${URL_BASE}`)
log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['recording', ListRecordingsResponseItem, recording]], ng)

log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const fixtureDir = ensureFixture()
if (fixtureDir === undefined) {
  log('  ffmpeg が無いため、追っかけの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}

const { entries, playlist } = eventPlaylists(fixtureDir)
if (entries.length < 3) {
  ng.push(`フィクスチャのセグメント数が少なすぎる（${entries.length}）`)
  await finish(ng)
}

const browser = await launchBrowser('chromium')
const context = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  locale: 'ja-JP',
})
await context.addInitScript(() => localStorage.clear())
const page = await context.newPage()

await installApiStubs(page, async ({ path: requestPath, url, json, route }) => {
  const method = route.request().method()
  if (requestPath === '/api/sites') return json(['default'])
  if (requestPath === '/api/capabilities') return json({ live: true })
  if (requestPath === '/api/breakers') return json([])
  if (requestPath === '/api/events') return sseKeepAlive(route)
  if (requestPath === '/api/rules' || requestPath === '/api/encode-profiles') return json([])
  if (requestPath === '/api/encode-queue') return json({ queued: 0, running: 0 })
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(requestPath)) {
    return route.fulfill({ status: 404 })
  }
  if (requestPath === '/api/recordings' && method === 'GET') return json([recording])
  if (requestPath === '/api/recordings/1' && method === 'GET') return json(recording)
  if (requestPath === '/api/sites/default/recordings/1/chase/leave' && method === 'POST') {
    return route.fulfill({ status: 204 })
  }
  // `url` is intentionally read here so the handler remains total if a future
  // page query adds a harmless query parameter to one of the stubs.
  void url
  return json([])
})

const chaseBase = '/api/sites/default/recordings/1/chase'
let playlistRequests = 0
const playlistSizes = []
let playlistEnded = false

await page.route(`**${chaseBase}/playlist.m3u8*`, async (route) => {
  playlistRequests += 1
  const count = playlistRequests < 2 ? 1 : entries.length
  const end = playlistRequests >= 2
  playlistSizes.push(count)
  playlistEnded ||= end
  await route.fulfill({
    status: 200,
    contentType: 'application/vnd.apple.mpegurl',
    body: playlist(count, end),
  })
})

await page.route(`**${chaseBase}/segments/*`, async (route) => {
  const name = new URL(route.request().url()).pathname.split('/').pop()
  const file = path.join(fixtureDir, 'segments', name)
  if (!existsSync(file)) {
    await route.fulfill({ status: 404, body: 'not found' })
    return
  }
  await route.fulfill({ status: 200, contentType: 'video/mp2t', body: readFileSync(file) })
})

log('\n=== ① 録画詳細の #chase で追っかけプレイヤーを開く ===')
await page.goto(`${URL_BASE}/recordings/1#chase`, { waitUntil: 'domcontentloaded' })
await page.getByRole('region', { name: '追っかけ再生' }).waitFor({ timeout: 15000 })
await page.locator('video').waitFor({ timeout: 15000 })

await page.waitForFunction(
  () => {
    const video = document.querySelector('video')
    return video !== null && Number.isFinite(video.duration) && video.duration > 0
  },
  { timeout: 15000 },
).catch(() => {
  ng.push('① 実 HLS の duration が確定しない')
})

if (playlistRequests === 0) {
  ng.push('① chase playlist が一度も要求されない')
}
const startTime = await page.locator('video').evaluate((video) => video.currentTime)
if (startTime > 2) {
  ng.push(`① 追っかけの開始位置が先頭でない（currentTime=${startTime}）`)
}

log('\n=== ② EVENT playlist の成長と「最新」 ===')
const growthDeadline = Date.now() + 12_000
while (Date.now() < growthDeadline && new Set(playlistSizes).size < 2) {
  await page.waitForTimeout(250)
}
if (new Set(playlistSizes).size < 2) {
  ng.push(`② EVENT playlist が成長しない（sizes=${playlistSizes.join(',') || 'none'}）`)
}

const latest = page.getByRole('button', { name: '最新' })
if ((await latest.count()) !== 1) {
  ng.push('② 「最新」ボタンが表示されない')
} else {
  await latest.click()
  await page.waitForTimeout(250)
  const position = await page.locator('video').evaluate((video) => ({
    currentTime: video.currentTime,
    duration: video.duration,
  }))
  if (Number.isFinite(position.duration) && position.duration > 0 && position.currentTime < position.duration - 1) {
    ng.push(`② 「最新」が EVENT の末尾へ移動しない（${position.currentTime}/${position.duration}）`)
  }
}

if (!playlistEnded) {
  ng.push('② 成長後の playlist が ENDLIST にならない')
}

await finish(ng, browser)
