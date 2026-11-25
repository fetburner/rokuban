// e2e-needs: s3
import { randomUUID } from 'node:crypto'
import { createServer } from 'node:http'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

import { createPlaywrightAdapter } from './lib.mjs'
import {
  S3_URL,
  createS3ThrottleProxyServer,
  createS3FixtureClient,
  listenFixtureServer as listen,
  makeS3Video,
  startStreamerFixture,
  stopStreamerFixture,
  waitForFixture,
  waitForRustFS,
} from './s3-fixture.mjs'

const { request: s3Request } = createS3FixtureClient()
const RUN_ID = randomUUID().replace(/-/g, '').slice(0, 12)
const BUCKET = 'rokuban-proxy-' + RUN_ID
const KEY = 'encoded/s3-proxy-' + RUN_ID + '.mp4'
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const STREAMER_PORT = 4176
const S3_PROXY_PORT = 4175
const PAGE_PORT = 4174
// PAUSE_MS は一時停止から再開までの待ち（ms）。跨ぐ待ち時間は未特定。
// 調べた範囲（出典に書かれていることだけを書く。書かれていないものは「記載なし・未検証」）:
// - streamer の本番 HTTP サーバー（cmd/rokuban/server.go）: ReadHeaderTimeout 10s、IdleTimeout 120s、
//   ReadTimeout・WriteTimeout は未設定。https://pkg.go.dev/net/http#Server は IdleTimeout を「次のリクエストを待つ時間」、
//   0 のとき ReadTimeout を使うと書く。応答の書き込み中の期限は記載なし（同ファイルのテストが実測を主張）。
// - conformance フィクスチャ（internal/streamer/conformance/main.go）: ReadHeaderTimeout 5s のみ。IdleTimeout・ReadTimeout は未設定で、
//   net/http の doc によると keep-alive の待ちに期限は無い。ReadHeaderTimeout が keep-alive の待ちに効くとの記載は無い。
// - streamer の S3 クライアント（aws-sdk-go-v2）: https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws/transport/http の
//   BuildableClient は既定 Transport を使う。DefaultHTTPTransportIdleConnTimeout は 90s（アイドルな接続の待ち）。
//   ResponseHeaderTimeout は挙げていない。読み途中の応答本体の期限は記載なし。
// - Node の throttle proxy（テスト専用。本番には無い）: https://nodejs.org/api/http.html の既定は server.timeout 0、
//   keepAliveTimeout 5000 ms（応答を書き終えた後のアイドル）、requestTimeout 300000 ms（要求の受信）、headersTimeout は min(requestTimeout, 60000)。
//   一時停止中の応答途中への効きは記載なし。判定を誤らせうるのは、テスト経路だけが持つ 5s の keep-alive 期限。
// - RustFS: 公式文書の該当項目は見つからなかった。GitHub issue rustfs/rustfs#3076 の maintainer は、
//   RUSTFS_HTTP1_HEADER_READ_TIMEOUT（5s から 75s に変更と記載）が「idle な keep-alive 接続」を閉じる時間に効くと述べる。
//   一時停止中の応答途中に効くかは記載なし・未検証。
// - Chrome: 検索（"Chromium media element paused stops fetching buffer discard idle socket timeout" ほか）では、
//   取得の停止と idle ソケットの閉じ方を定めた現行の公開文書は見つからなかった。HTML Standard の media 節は、
//   取得の停止とデータの破棄をユーザーエージェントが行ってよいと定め、時間の条件は書いていない。
// 180000 は main 向けの従来値で、根拠は無い。PR では ci.yml が 0 を渡す（再開の経路だけを見る。待ちを跨ぐ失敗は見ない）。
// 環境変数が無いときは 180000 なので、ローカル実行の既定は変わらない。
const PAUSE_MS = parsePauseMs(process.env.E2E_S3_PROXY_PAUSE_MS)
const SEEK_TARGETS = [40, 80, 120]
const VIDEO_SECONDS = 300
// makeS3Video の目標 bitrate は映像 1,000 kbit/s + 音声 96 kbit/s。合計の 3 倍を上限にして、再生より速く先読みしすぎない範囲で Chrome の所要時間を抑える。
const VIDEO_BYTES_PER_SECOND = (1000 + 96) * 1000 / 8 * 3
const BROWSER = process.env.E2E_BROWSER ?? 'chrome'
let streamerOrigin = ''

/** parsePauseMs は E2E_S3_PROXY_PAUSE_MS を解釈する。未設定は 180000、0 以上の整数以外は例外にする。 */
function parsePauseMs(raw) {
  if (raw === undefined) return 180000
  if (!/^\d+$/.test(raw)) throw new Error('E2E_S3_PROXY_PAUSE_MS must be a non-negative integer of milliseconds: ' + JSON.stringify(raw))
  return Number(raw)
}

async function waitFor(fn, label, timeout = 20000) {
  return waitForFixture(fn, label, timeout)
}

async function getMediaGetRequests() {
  const response = await fetch(streamerOrigin + '/__stats')
  if (!response.ok) throw new Error('streamer request counter returned ' + response.status)
  return (await response.json()).mediaRangeGetRequests
}

function pageHTML() {
  return `<!doctype html><meta charset="utf-8"><title>S3 proxy playback</title>
<button id="play">Play / Resume</button><video id="player" muted playsinline preload="metadata"></video>
<script>
(() => {
 const video = document.querySelector('#player');
 const state = window.__state = { metadata: false, playing: false, paused: false, seekedCount: 0, mediaError: null, currentTime: 0, restoreTarget: null };
 video.addEventListener('loadedmetadata', () => {
   state.metadata = true; state.duration = video.duration;
   const saved = Number(localStorage.getItem('s3-proxy-resume'));
   if (Number.isFinite(saved) && saved > 0) { state.restoreTarget = saved; video.currentTime = saved; }
 });
 video.addEventListener('timeupdate', () => { state.currentTime = video.currentTime; });
 video.addEventListener('playing', () => { state.playing = true; state.paused = false; });
 video.addEventListener('pause', () => { state.paused = true; state.playing = false; });
 video.addEventListener('seeked', () => { state.seekedCount++; state.seekedAt = performance.now(); state.currentTime = video.currentTime; });
 video.addEventListener('error', () => { state.mediaError = { code: video.error && video.error.code, message: video.error && video.error.message }; });
 document.querySelector('#play').addEventListener('click', () => video.play().catch((error) => { state.playError = String(error); }));
 window.seekTo = (seconds) => { state.seekedCount = 0; state.seekStartedAt = performance.now(); video.currentTime = seconds; };
 window.isBuffered = (seconds) => Array.from({ length: video.buffered.length }, (_, index) => [video.buffered.start(index), video.buffered.end(index)])
   .some(([start, end]) => seconds >= start && seconds <= end);
 window.savePosition = () => { localStorage.setItem('s3-proxy-resume', String(video.currentTime)); return video.currentTime; };
 window.pausePlayback = () => video.pause();
 video.src = 'http://127.0.0.1:${STREAMER_PORT}/api/media/recordings/__RECORDING__/file?profile=s3conformance';
})();
</script>`
}

async function main() {
  if (S3_URL.protocol !== 'http:') throw new Error('E2E_S3_URL must use http for local RustFS')
  if (BROWSER !== 'chrome' && BROWSER !== 'webkit') throw new Error('E2E_BROWSER must be chrome or webkit')
  const binary = process.env.E2E_STREAMER_FIXTURE_BINARY
  if (!binary) throw new Error('E2E_STREAMER_FIXTURE_BINARY is required')
  await waitForRustFS()
  const tempDir = await mkdtemp(path.join(os.tmpdir(), 'rokuban-s3-proxy-'))
  const videoFile = makeS3Video(tempDir, { fileName: 'proxy.mp4', duration: VIDEO_SECONDS, tone: 440, constrained: false })
  const video = await readFile(videoFile)
  let browser
  let streamer
  let s3ProxyServer
  let pageServer
  let pageOrigin = ''
  let recordingID = 0
  try {
    await s3Request('PUT', '/' + BUCKET)
    await s3Request('PUT', '/' + BUCKET + '/' + KEY, video, { 'content-type': 'video/mp4' })
    s3ProxyServer = createS3ThrottleProxyServer({
      buckets: [BUCKET],
      bytesPerSecond: VIDEO_BYTES_PER_SECOND,
      shouldThrottle: (url, request) => request.method === 'GET' && url.pathname === '/' + BUCKET + '/' + KEY,
    })
    const s3ProxyOrigin = await listen(s3ProxyServer, S3_PROXY_PORT)
    const started = await startStreamerFixture({
      binary,
      repoRoot: REPO_ROOT,
      bucket: BUCKET,
      key: KEY,
      size: video.length,
      endpoint: s3ProxyOrigin,
      extraEnv: { E2E_S3_DELIVERY: 'proxy' },
    })
    streamer = started.child
    recordingID = started.recordingID

    const mediaURL = 'http://127.0.0.1:' + STREAMER_PORT + '/api/media/recordings/' + recordingID + '/file?profile=s3conformance'
    const head = await fetch(mediaURL, { method: 'HEAD', redirect: 'manual' })
    if (head.status !== 200 || head.headers.has('location') || head.headers.get('accept-ranges') !== 'bytes' || head.headers.get('content-length') !== String(video.length)) {
      throw new Error('proxy delivery HEAD was not a direct 200 with byte ranges and the fixture size: ' + JSON.stringify({ status: head.status, location: head.headers.get('location'), acceptRanges: head.headers.get('accept-ranges'), contentLength: head.headers.get('content-length') }))
    }

    pageServer = createServer((_request, response) => {
      response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
      response.end(pageHTML().replace('__RECORDING__', String(recordingID)))
    })
    pageOrigin = await listen(pageServer, PAGE_PORT)
    browser = await createPlaywrightAdapter(BROWSER)
    console.log('Browser: ' + BROWSER + ' ' + browser.version)
    streamerOrigin = started.origin
    console.log('Proxy fixture: ' + BUCKET + '/' + KEY + ', ' + video.length + ' bytes, duration: ' + VIDEO_SECONDS + 's')
    await browser.navigate(pageOrigin)
    await waitFor(async () => browser.evaluate(() => window.__state?.metadata), 'video metadata')
    await browser.click('#play')
    await waitFor(async () => browser.evaluate(() => window.__state?.playing && window.__state.currentTime >= 2), 'initial playback')

    for (const [index, target] of SEEK_TARGETS.entries()) {
      const alreadyBuffered = await browser.evaluate((seconds) => window.isBuffered(seconds), target)
      if (alreadyBuffered) throw new Error('seek target was already buffered before seek: ' + target)
      const requestsBefore = await getMediaGetRequests()
      await browser.evaluate((seconds) => window.seekTo(seconds), target)
      await waitFor(async () => browser.evaluate((seconds) => window.__state?.seekedCount > 0 && Math.abs(window.__state.currentTime - seconds) < 0.5 || window.__state?.mediaError, target), 'seek to ' + target, 60000)
      const seek = await browser.evaluate(() => ({ currentTime: document.querySelector('#player').currentTime, seekMs: Math.round(window.__state.seekedAt - window.__state.seekStartedAt), error: window.__state.mediaError }))
      if (seek.error || Math.abs(seek.currentTime - target) > 0.5) throw new Error('seek to ' + target + ' failed: ' + JSON.stringify(seek))
      const requestsAfter = await getMediaGetRequests()
      if (requestsAfter <= requestsBefore) throw new Error('seek to ' + target + ' caused no new streamer Range GET request')
      console.log('Proxy seek ' + (index + 1) + ' (' + target + 's) latency: ' + seek.seekMs + ' ms; streamer GETs: ' + requestsBefore + ' → ' + requestsAfter)
    }

    const saved = await browser.evaluate(() => window.savePosition())
    await browser.navigate(pageOrigin)
    await waitFor(async () => browser.evaluate((position) => window.__state?.metadata && window.__state.seekedCount > 0 && Math.abs(window.__state.currentTime - position) < 0.5, saved), 'restore saved position', 60000)
    await browser.click('#play')
    await waitFor(async () => browser.evaluate((position) => window.__state?.playing && window.__state.currentTime > position + 0.5, saved), 'playback from saved position', 20000)

    await browser.evaluate(() => window.pausePlayback())
    await waitFor(async () => browser.evaluate(() => window.__state?.paused), 'pause')
    console.log('Paused for: ' + PAUSE_MS + ' ms')
    await delay(PAUSE_MS)
    const pausedAt = await browser.evaluate(() => document.querySelector('#player').currentTime)
    await browser.click('#play')
    await waitFor(async () => browser.evaluate((position) => window.__state?.playing && window.__state.currentTime > position + 0.5, pausedAt), 'playback after pause', 20000)
    const final = await browser.evaluate(() => ({ currentTime: document.querySelector('#player').currentTime, mediaError: window.__state.mediaError }))
    if (final.mediaError) throw new Error('media element error: ' + JSON.stringify(final.mediaError))
    console.log('Playback resumed after pause: ' + JSON.stringify(final))
    console.log('PASS: S3 proxy browser playback')
  } finally {
    if (browser) await browser.close().catch(() => {})
    if (pageServer) await new Promise((resolve) => pageServer.close(resolve))
    await stopStreamerFixture(streamer).catch(() => {})
    if (s3ProxyServer?.listening) {
      await new Promise((resolve) => {
        s3ProxyServer.close(resolve)
        s3ProxyServer.closeAllConnections()
      })
    }
    await s3Request('DELETE', '/' + BUCKET + '/' + KEY).catch(() => {})
    await s3Request('DELETE', '/' + BUCKET).catch(() => {})
    await rm(tempDir, { recursive: true, force: true })
  }
}

main().catch((error) => { console.error(error.stack ?? String(error)); process.exitCode = 1 })
