import { createHash, createHmac } from 'node:crypto'
import { execFileSync, spawn } from 'node:child_process'
import { request as httpRequest, createServer } from 'node:http'
import { Transform, pipeline } from 'node:stream'
import { setTimeout as delay } from 'node:timers/promises'
import path from 'node:path'

export const S3_URL = new URL(process.env.E2E_S3_URL ?? 'http://127.0.0.1:9000')
export const ACCESS_KEY = process.env.E2E_S3_ACCESS_KEY ?? 'rokuban'
export const SECRET_KEY = process.env.E2E_S3_SECRET_KEY ?? 'rokuban-test-secret'
export const REGION = 'us-east-1'

function sha256(data) {
  return createHash('sha256').update(data).digest('hex')
}

function hmac(key, value, encoding) {
  return createHmac('sha256', key).update(value).digest(encoding)
}

function awsEncode(value) {
  return encodeURIComponent(value).replace(/[!'()*]/g, (char) => '%' + char.charCodeAt(0).toString(16).toUpperCase())
}

function canonicalQuery(searchParams) {
  return [...searchParams.entries()]
    .map(([key, value]) => [awsEncode(key), awsEncode(value)])
    .sort((left, right) => left[0] === right[0]
      ? (left[1] < right[1] ? -1 : left[1] > right[1] ? 1 : 0)
      : (left[0] < right[0] ? -1 : left[0] > right[0] ? 1 : 0))
    .map(([key, value]) => key + '=' + value)
    .join('&')
}

function amzTimestamp(date) {
  return date.toISOString().replace(/[:-]|\.\d{3}/g, '')
}

export function createS3FixtureClient() {
  function signingKey(dateStamp) {
    const dateKey = hmac('AWS4' + SECRET_KEY, dateStamp)
    const regionKey = hmac(dateKey, REGION)
    const serviceKey = hmac(regionKey, 's3')
    return hmac(serviceKey, 'aws4_request')
  }

  function authorizationHeaders(method, url, body, extraHeaders = {}) {
    const timestamp = amzTimestamp(new Date())
    const payloadHash = sha256(body)
    const headers = {
      host: url.host,
      'x-amz-content-sha256': payloadHash,
      'x-amz-date': timestamp,
      ...extraHeaders,
    }
    const signedNames = Object.keys(headers).map((name) => name.toLowerCase()).sort()
    const canonicalHeaders = signedNames
      .map((name) => name + ':' + String(headers[name]).trim().replace(/\s+/g, ' ') + '\n')
      .join('')
    const canonicalRequest = [
      method,
      url.pathname,
      canonicalQuery(url.searchParams),
      canonicalHeaders,
      signedNames.join(';'),
      payloadHash,
    ].join('\n')
    const dateStamp = timestamp.slice(0, 8)
    const scope = dateStamp + '/' + REGION + '/s3/aws4_request'
    const stringToSign = ['AWS4-HMAC-SHA256', timestamp, scope, sha256(canonicalRequest)].join('\n')
    const signature = hmac(signingKey(dateStamp), stringToSign, 'hex')
    return {
      ...headers,
      Authorization: 'AWS4-HMAC-SHA256 Credential=' + ACCESS_KEY + '/' + scope
        + ', SignedHeaders=' + signedNames.join(';') + ', Signature=' + signature,
    }
  }

  async function request(method, pathName, body = Buffer.alloc(0), extraHeaders = {}) {
    const url = new URL(pathName, S3_URL)
    const headers = authorizationHeaders(method, url, body, extraHeaders)
    const response = await fetch(url, { method, headers, body: body.length === 0 ? undefined : body })
    const responseText = await response.text()
    if (!response.ok && !(method === 'PUT' && response.status === 409 && responseText.includes('BucketAlreadyOwnedByYou'))) {
      throw new Error(method + ' ' + url.pathname + url.search + ' failed (' + response.status + '): ' + responseText)
    }
    return response
  }

  function presignedGetUrl(bucket, key, expiresIn, publicOrigin) {
    const now = new Date()
    const timestamp = amzTimestamp(now)
    const dateStamp = timestamp.slice(0, 8)
    const scope = dateStamp + '/' + REGION + '/s3/aws4_request'
    const url = new URL('/' + bucket + '/' + key, publicOrigin)
    url.searchParams.set('X-Amz-Algorithm', 'AWS4-HMAC-SHA256')
    url.searchParams.set('X-Amz-Credential', ACCESS_KEY + '/' + scope)
    url.searchParams.set('X-Amz-Date', timestamp)
    url.searchParams.set('X-Amz-Expires', String(expiresIn))
    url.searchParams.set('X-Amz-SignedHeaders', 'host')
    const canonicalRequest = [
      'GET', url.pathname, canonicalQuery(url.searchParams), 'host:' + url.host + '\n', 'host', 'UNSIGNED-PAYLOAD',
    ].join('\n')
    const stringToSign = ['AWS4-HMAC-SHA256', timestamp, scope, sha256(canonicalRequest)].join('\n')
    const signature = hmac(signingKey(dateStamp), stringToSign, 'hex')
    url.searchParams.set('X-Amz-Signature', signature)
    return { url: url.toString(), issuedAt: now.getTime(), expiresAt: now.getTime() + expiresIn * 1000 }
  }

  return { request, presignedGetUrl }
}

// makeS3Video は RustFS のブラウザ判定で使う MP4 fixture を生成する。
export function makeS3Video(tempDir, { fileName, duration, tone = 660, constrained = true }) {
  const file = path.join(tempDir, fileName)
  const args = [
    '-hide_banner', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=24',
    '-f', 'lavfi', '-i', 'sine=frequency=' + tone + ':sample_rate=44100',
    '-t', String(duration),
    '-c:v', 'libx264', '-profile:v', 'baseline', '-level:v', '3.0', '-pix_fmt', 'yuv420p',
    '-preset', 'veryfast', '-b:v', '1000k',
  ]
  if (constrained) args.push('-maxrate', '1000k', '-bufsize', '2000k')
  args.push('-g', '48', '-keyint_min', '48', '-sc_threshold', '0', '-c:a', 'aac', '-b:a', '96k', '-movflags', '+faststart', file)
  execFileSync('ffmpeg', args, { stdio: 'ignore' })
  return file
}

export async function listenFixtureServer(server, port) {
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, '127.0.0.1', resolve)
  })
  return 'http://127.0.0.1:' + server.address().port
}

/** createS3ThrottleProxyServer は S3 fixture 向けの帯域制限付き HTTP プロキシを作る。 */
export function createS3ThrottleProxyServer({ buckets, bytesPerSecond, shouldThrottle = () => true, onRequest = () => {}, onResponse = () => {}, onError = () => {} }) {
  /** throttle は前のチャンクの送り出し予定から送信間隔を計算する。 */
  function throttle() {
    let nextAt = Date.now()
    return new Transform({
      transform(chunk, _encoding, done) {
        nextAt = Math.max(Date.now(), nextAt) + chunk.length / bytesPerSecond * 1000
        setTimeout(() => done(null, chunk), nextAt - Date.now())
      },
    })
  }

  const s3Host = S3_URL.hostname
  const s3Port = Number(S3_URL.port || (S3_URL.protocol === 'https:' ? 443 : 80))
  return createServer((req, res) => {
    const url = new URL(req.url, 'http://' + (req.headers.host ?? '127.0.0.1'))
    const bucket = url.pathname.split('/')[1]
    if (!buckets.includes(bucket)) {
      res.writeHead(404).end('unknown bucket')
      return
    }
    const record = onRequest({ request: req, url, bucket })
    const headers = { ...req.headers, host: req.headers.host }
    delete headers.connection
    delete headers['proxy-connection']
    const upstream = httpRequest({
      hostname: s3Host,
      port: s3Port,
      method: req.method,
      path: req.url,
      headers,
    }, (upstreamResponse) => {
      onResponse(record, upstreamResponse)
      if (res.destroyed) {
        upstreamResponse.destroy()
        return
      }
      res.writeHead(upstreamResponse.statusCode ?? 502, upstreamResponse.headers)
      const streams = shouldThrottle(url, req) ? [upstreamResponse, throttle(), res] : [upstreamResponse, res]
      pipeline(...streams, () => {})
    })
    upstream.on('error', (error) => {
      onError(record, error)
      if (res.destroyed) return
      if (!res.headersSent) res.writeHead(502, { 'Content-Type': 'text/plain' })
      res.end(String(error))
    })
    req.pipe(upstream)
  })
}

export async function waitForFixture(fn, label, timeout = 20000, interval = 100) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    if (await fn()) return
    await delay(interval)
  }
  throw new Error('timed out waiting for ' + label)
}

export async function waitForRustFS() {
  if (S3_URL.protocol !== 'http:') throw new Error('E2E_S3_URL must use http for the local S3 fixture')
  const healthURL = new URL('/health', S3_URL)
  await waitForFixture(async () => {
    try { return (await fetch(healthURL)).ok } catch { return false }
  }, 'RustFS at ' + healthURL, 30000, 250)
}

export async function startStreamerFixture({ binary, repoRoot, bucket, key, size, endpoint = S3_URL.origin, extraEnv = {} }) {
  const child = spawn(binary, [], {
    cwd: repoRoot,
    stdio: 'inherit',
    env: {
      ...process.env,
      AWS_ACCESS_KEY_ID: ACCESS_KEY,
      AWS_SECRET_ACCESS_KEY: SECRET_KEY,
      AWS_REGION: REGION,
      ROKUBAN_S3_CONFORMANCE_ENDPOINT: endpoint,
      ROKUBAN_S3_CONFORMANCE_REGION: REGION,
      ROKUBAN_S3_CONFORMANCE_BUCKET: bucket,
      E2E_S3_VIDEO_KEY: key,
      E2E_S3_VIDEO_SIZE: String(size),
      ...extraEnv,
    },
  })
  let spawnError
  child.once('error', (error) => { spawnError = error })
  const healthURL = 'http://127.0.0.1:4176/__health'
  const deadline = Date.now() + 60000
  while (Date.now() < deadline) {
    if (spawnError) throw new Error('could not start the application streamer fixture: ' + spawnError.message)
    if (child.exitCode !== null) throw new Error('application streamer fixture exited early with code ' + child.exitCode)
    try {
      const response = await fetch(healthURL)
      if (response.ok) {
        const body = await response.json()
        return { child, origin: 'http://127.0.0.1:4176', recordingID: body.recordingId }
      }
    } catch {}
    await delay(250)
  }
  child.kill('SIGTERM')
  throw new Error('application streamer fixture did not become ready at ' + healthURL)
}

export async function stopStreamerFixture(child) {
  if (!child) return
  if (child.exitCode === null) child.kill('SIGTERM')
  await new Promise((resolve) => {
    if (child.exitCode !== null) resolve()
    else child.once('exit', resolve)
  })
}
