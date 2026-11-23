// e2e-needs: s3
import { randomUUID } from 'node:crypto'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { createServer } from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { createPlaywrightAdapter } from './lib.mjs'
import {
  createS3FixtureClient,
  createS3ThrottleProxyServer,
  listenFixtureServer,
  makeS3Video,
  startStreamerFixture,
  stopStreamerFixture,
  waitForFixture,
  waitForRustFS,
} from './s3-fixture.mjs'

const { request: s3Request } = createS3FixtureClient()
const RUN_ID = randomUUID()
const BUCKET = 'rokuban-crypt-proxy-' + RUN_ID
const KEY = 'encoded/番組/2026/11/特別編_0900-' + RUN_ID + '.s3conformance.u0.mp4'
const MARKER = '.rokuban-crypt'
const REPO_ROOT = fileURLToPath(new URL('../..', import.meta.url))
const SEEK_TARGET = 90
const VIDEO_SECONDS = 120
// makeS3Video の映像と音声の目標 bitrate の 3 倍で配る。先読みがシーク先に届いていないことは video.buffered で判定する。
const VIDEO_BYTES_PER_SECOND = (1000 + 96) * 1000 / 8 * 3

async function bucketKeys() {
  const listing = await s3Request('GET', '/' + BUCKET + '?list-type=2&encoding-type=url')
  const xml = await listing.text()
  return [...xml.matchAll(/<Key>([^<]+)<\/Key>/g)].map(([, key]) => decodeURIComponent(key))
}

function objectPath(key) {
  return '/' + BUCKET + '/' + key.split('/').map(encodeURIComponent).join('/')
}

async function checkEncryptedBucket(keys, video) {
  const exposedNames = [KEY, '番組', '特別編']
  if (keys.length !== 2 || !keys.includes(MARKER)) {
    throw new Error('expected one completed object and the key marker in RustFS: ' + JSON.stringify(keys))
  }
  const sealedKey = keys.find((key) => key !== MARKER)
  const clearParts = KEY.split('/')
  if (sealedKey.split('/').some((part) => clearParts.includes(part)) ||
      [...exposedNames, '.mp4'].some((name) => sealedKey.includes(name))) {
    throw new Error('RustFS object key exposes the plaintext name: ' + sealedKey)
  }
  const response = await s3Request('GET', objectPath(sealedKey))
  const sealed = Buffer.from(await response.arrayBuffer())
  if (!sealed.subarray(0, 8).equals(Buffer.from('RCLONE\0\0'))) {
    throw new Error('RustFS object body lacks the crypt header')
  }
  if (exposedNames.some((name) => sealed.includes(Buffer.from(name)))) {
    throw new Error('RustFS object body exposes the plaintext name')
  }
  // 平文の MP4 の先頭と各 64 KiB ブロックの先頭を探し、ヘッダだけ付けた本文や一部のブロックの漏れを検出する。
  for (let start = 0; start + 32 <= video.length; start += 64 * 1024) {
    if (sealed.includes(video.subarray(start, start + 32))) {
      throw new Error('RustFS object body exposes plaintext video bytes at offset ' + start)
    }
  }
}

function playbackPage(mediaURL) {
  return `<!doctype html><meta charset="utf-8"><title>Encrypted S3 proxy playback</title>
<button id="play">Play</button><video id="player" muted playsinline preload="metadata"></video>
<script>
const video = document.querySelector('#player');
window.seeked = 0;
video.addEventListener('seeked', () => window.seeked++);
document.querySelector('#play').onclick = () => video.play().catch((error) => { window.playError = String(error); });
video.src = ${JSON.stringify(mediaURL)};
</script>`
}

async function waitForVideo(browser, predicate, label) {
  await waitForFixture(async () => {
    const error = await browser.evaluate(() => document.querySelector('#player').error?.message ?? window.playError)
    if (error) throw new Error(label + ': ' + error)
    return browser.evaluate(predicate, SEEK_TARGET)
  }, label, 60000)
}

async function mediaRangeRequests(origin) {
  const response = await fetch(origin + '/__stats')
  if (!response.ok) throw new Error('streamer request counter returned ' + response.status)
  return (await response.json()).mediaRangeGetRequests
}

async function main() {
  const binary = process.env.E2E_STREAMER_FIXTURE_BINARY
  if (!binary) throw new Error('E2E_STREAMER_FIXTURE_BINARY is required')
  await waitForRustFS()
  const tempDir = await mkdtemp(path.join(os.tmpdir(), 'rokuban-s3-crypt-proxy-'))
  let browser
  let streamer
  let proxy
  let pageServer
  let bucketCreated = false
  let keys
  try {
    const videoFile = makeS3Video(tempDir, { fileName: 'crypt.mp4', duration: VIDEO_SECONDS })
    const video = await readFile(videoFile)
    await s3Request('PUT', '/' + BUCKET)
    bucketCreated = true

    let s3SeekRanges = 0
    proxy = createS3ThrottleProxyServer({
      buckets: [BUCKET],
      bytesPerSecond: VIDEO_BYTES_PER_SECOND,
      shouldThrottle: (url, request) => request.method === 'GET' && url.pathname !== objectPath(MARKER),
      onRequest: ({ request }) => request.method === 'GET' && /^bytes=[1-9]\d*-$/.test(request.headers.range ?? ''),
      onResponse: (seekRange, response) => {
        if (seekRange && response.statusCode === 206) s3SeekRanges++
      },
    })
    const endpoint = await listenFixtureServer(proxy, 0)
    const started = await startStreamerFixture({
      binary, repoRoot: REPO_ROOT, bucket: BUCKET, key: KEY, size: video.length, endpoint,
      extraEnv: { E2E_S3_DELIVERY: 'crypt', E2E_S3_VIDEO_FILE: videoFile },
    })
    streamer = started.child

    keys = await bucketKeys()
    await checkEncryptedBucket(keys, video)

    const mediaURL = started.origin + '/api/media/recordings/' + started.recordingID + '/file?profile=s3conformance'
    const head = await fetch(mediaURL, { method: 'HEAD', redirect: 'manual' })
    if (head.status !== 200 || head.headers.has('location') || head.headers.get('accept-ranges') !== 'bytes' || head.headers.get('content-length') !== String(video.length)) {
      throw new Error('encrypted proxy HEAD must serve the plaintext size and byte ranges directly: ' + head.status + ' ' + JSON.stringify(Object.fromEntries(head.headers)))
    }
    pageServer = createServer((_request, response) => {
      response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
      response.end(playbackPage(mediaURL))
    })
    const origin = await listenFixtureServer(pageServer, 0)
    browser = await createPlaywrightAdapter('chrome')
    console.log('Browser: chrome ' + browser.version)
    await browser.navigate(origin)
    await waitForVideo(browser, () => document.querySelector('#player').readyState >= 1, 'encrypted video metadata')
    await browser.click('#play')
    await waitForVideo(browser, () => document.querySelector('#player').currentTime >= 2, 'encrypted video initial playback')
    await browser.evaluate(() => document.querySelector('#player').pause())

    const buffered = await browser.evaluate((target) => {
      const ranges = document.querySelector('#player').buffered
      return Array.from({ length: ranges.length }, (_, i) => i)
        .some((i) => target >= ranges.start(i) && target <= ranges.end(i))
    }, SEEK_TARGET)
    if (buffered) throw new Error('encrypted video seek target is already buffered')
    const requestsBefore = await mediaRangeRequests(started.origin)
    const s3RangesBefore = s3SeekRanges
    await browser.evaluate((target) => { document.querySelector('#player').currentTime = target }, SEEK_TARGET)
    await waitForVideo(browser, (target) => window.seeked > 0 && Math.abs(document.querySelector('#player').currentTime - target) < 0.5, 'encrypted video seek')
    if (await mediaRangeRequests(started.origin) <= requestsBefore || s3SeekRanges <= s3RangesBefore) {
      throw new Error('encrypted video seek must issue new Range GETs to streamer and RustFS')
    }
    await browser.click('#play')
    await waitForVideo(browser, (target) => document.querySelector('#player').currentTime >= target + 1, 'encrypted video playback after seek')
    console.log('PASS: Chrome played and sought encrypted S3 video through streamer, and RustFS stored ciphertext')
  } finally {
    if (browser) await browser.close().catch(() => {})
    if (pageServer?.listening) await new Promise((resolve) => pageServer.close(resolve))
    await stopStreamerFixture(streamer).catch(() => {})
    if (proxy?.listening) {
      await new Promise((resolve) => {
        proxy.close(resolve)
        proxy.closeAllConnections()
      })
    }
    if (bucketCreated) {
      for (const key of keys ?? await bucketKeys().catch(() => [])) {
        await s3Request('DELETE', objectPath(key)).catch(() => {})
      }
      await s3Request('DELETE', '/' + BUCKET).catch(() => {})
    }
    await rm(tempDir, { recursive: true, force: true })
  }
}

main().catch((error) => { console.error(error.stack ?? String(error)); process.exitCode = 1 })
