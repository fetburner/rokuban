// ライブの音声切替（二重音声の主 / 副。issue #870）の受け入れ判定。jsdom では
// 測れないものだけをここで見る（e2e/README.md）: 実ブラウザがアプリのセレクタに
// 従って代替音声レンディションを本当に切り替えること、そして**前に聴いたトラックへ
// ライブの窓より後で戻っても止まらないこと**。後者は、ライブの playlist に
// EXT-X-PROGRAM-DATE-TIME が無いと hls.js が止まる（`internal/streamer/live.go` の
// hlsFlags）。窓がスライドした後でだけ起きるので、**ffmpeg に実際のライブ HLS を
// 書かせ続ける**。
//
// **mirakc も DB も要らない。** `/api/**` とライブの HLS は page.route で差し替える
// （subtitles.mjs と同じ手）。ffmpeg の引数は `BuildLiveFFmpegArgs` の既定経路を
// 1 プロファイルぶん写したもの。Go 側の引数が本当にこの形の出力（音声 3 本 +
// PDT）になることは `TestBuildLiveFFmpegArgs_RealFFmpegAudioRenditions` が
// 実 ffmpeg で固定する。入力は L = 440Hz / R = 880Hz のステレオで、二重音声を
// 既定でデコードした形（L = 主 / R = 副）の代わり。
//
// 判定:
//   ① Chromium（hls.js）: 主 → 副 → 標準 → 主 と選び、WebAudio で左右の周波数が
//      期待値（主 440/440・副 880/880・標準 440/880）に届くこと、再生が止まらない
//      こと、プレイリストを取り直さないこと（切替はトラックの選択だけ）
//   ② WebKit（ネイティブ HLS）: 副を選ぶと video.audioTracks の 3 本目が有効になり、
//      副のレンディションを取りに行くこと。WebKit はネイティブ HLS の音を WebAudio に
//      流さないので、鳴っている音そのものは測れない
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:live-audio
import { execFileSync, spawn } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { ListServicesResponseItem } from '../src/api/zod.ts'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'
const NETWORK_ID = 1
const SERVICE_ID = 9201
const COMPOSITE_ID = NETWORK_ID * 100_000 + SERVICE_ID
const PROFILE = 'hd'
// 1 つのトラックを聴き続ける時間。ライブの窓（hls_list_size 6 × hls_time 2 = 12 秒）より長くする
const DWELL_MS = 15_000
const ng = []
const skipped = []

const liveService = {
  id: COMPOSITE_ID,
  networkId: NETWORK_ID,
  serviceId: SERVICE_ID,
  name: '二重音声テスト局',
  channelType: 'GR',
  channel: '99',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: false,
}

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit([['service', ListServicesResponseItem, liveService]], ng)
await verifyBundleMatchesOrExit(URL_BASE, ng)

let ffmpegAvailable = true
try {
  execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
} catch {
  ffmpegAvailable = false
}
if (!ffmpegAvailable) {
  skipped.push('①② ffmpeg が無いためライブ HLS を生成できず測れない')
  log('\n=== 測れなかった項目 ===')
  skipped.forEach((s) => log('  SKIP: ' + s))
  await finish(ng, null)
}

const liveDir = mkdtempSync(path.join(os.tmpdir(), 'rokuban-e2e-live-audio-'))
const input = ensureStereoInput(path.join(os.tmpdir(), 'rokuban-e2e-live-audio-input.ts'))
const ffmpeg = startLiveFFmpeg(input, liveDir)
try {
  await waitForFile(path.join(liveDir, `${PROFILE}.m3u8`), 20_000)

  log('\n=== ① Chromium（hls.js）: セレクタに従って音声が替わり、戻る切替でも止まらない ===')
  {
    const browser = await launchBrowser('chromium', {
      args: ['--autoplay-policy=no-user-gesture-required'],
    })
    const page = await openLivePage(browser)
    const masterFetches = countRequests(page, /\/live\/playlist\.m3u8/)
    await startPlaybackWithAnalyser(page)

    // 期待値は (L, R) の周波数。標準へ戻る・主へ戻るの 2 回が「一度聴いたトラックへ
    // 戻る切替」で、PDT が無いとここで止まる。**各トラックをライブの窓
    // （6 セグメント × 2 秒 = 12 秒）より長く聴いてから次へ進む** --- すぐ戻ると
    // hls.js が持っている古いトラックの playlist がまだ今の窓と重なっていて、PDT が
    // 無くても揃えられてしまう（実測: 1 秒ずつで切り替えると PDT 無しでも通った）
    const steps = [
      ['main', '主音声', [440, 440]],
      ['sub', '副音声', [880, 880]],
      ['', '標準', [440, 880]],
      ['main', '主音声（戻る）', [440, 440]],
    ]
    const before = masterFetches.count
    for (const [value, label, [wantL, wantR]] of steps) {
      await page.getByLabel('音声').selectOption(value)
      const got = await waitForChannels(page, wantL, wantR, 12_000)
      if (!got.ok) {
        ng.push(
          `① ${label}: 12 秒待っても L/R = ${got.last.L}/${got.last.R} Hz（want ${wantL}/${wantR}）、` +
            `currentTime ${got.startTime} → ${got.last.t}${got.stalled ? '（再生が止まっている）' : ''}`,
        )
        continue
      }
      await new Promise((r) => setTimeout(r, DWELL_MS))
      const held = await page.evaluate(() => window.__measure())
      if (Math.abs(held.L - wantL) > 12 || Math.abs(held.R - wantR) > 12 || held.t - got.last.t < DWELL_MS / 2000) {
        ng.push(`① ${label}: 切り替わった後 ${DWELL_MS} ms で L/R = ${held.L}/${held.R} Hz、currentTime ${got.last.t} → ${held.t}（保てていない）`)
        continue
      }
      log(`  OK: ${label} → L/R = ${got.last.L}/${got.last.R} Hz（${got.elapsed} ms）、${DWELL_MS} ms 保持`)
    }
    if (masterFetches.count !== before) {
      ng.push(`① 音声の切替でプレイリストを取り直した（${masterFetches.count - before} 回）。切替はトラックの選択だけのはず`)
    } else {
      log('  OK: 切替の間にプレイリストの取り直しは無い')
    }
    await browser.close()
  }

  log('\n=== ② WebKit（ネイティブ HLS）: 副を選ぶと 3 本目のトラックが有効になり、副を取りに行く ===')
  {
    let browser
    try {
      browser = await launchBrowser('webkit')
    } catch (err) {
      skipped.push(`② WebKit を起動できない（pnpm exec playwright install webkit）: ${err}`)
    }
    if (browser) {
      const page = await openLivePage(browser)
      // 副のレンディションは var_stream_map の 4 本目（v:0 / 標準 / 主 / 副）
      const subFetches = countRequests(page, new RegExp(`/live/${PROFILE}\\.3\\.m3u8`))
      await page.getByRole('button', { name: /再生/ }).click()
      await waitForVisibleVideo(page)
      await page.evaluate(() => document.querySelector('video').play().catch(() => {}))
      const tracks = await waitForNativeTracks(page, 15_000)
      if (tracks === null) {
        ng.push('② video.audioTracks が 3 本にならない（ネイティブ経路が代替音声を読んでいない）')
      } else {
        await page.getByLabel('音声').selectOption('sub')
        let lastEnabled = []
        const enabled = await waitFor(async () => {
          lastEnabled = await page.evaluate(() =>
            Array.from(document.querySelector('video').audioTracks).map((t) => t.enabled),
          )
          return JSON.stringify(lastEnabled) === JSON.stringify([false, false, true]) ? lastEnabled : null
        }, 10_000)
        const fetched = await waitFor(async () => (subFetches.count > 0 ? subFetches.count : null), 10_000)
        if (enabled === null) {
          ng.push(`② 副を選んでも audioTracks の 3 本目だけが有効にならない（10 秒後 ${JSON.stringify(lastEnabled)}）`)
        }
        else if (fetched === null) ng.push(`② 副を選んでも ${PROFILE}.3.m3u8 を取りに行かない`)
        else log(`  OK: audioTracks.enabled = [false,false,true]、${PROFILE}.3.m3u8 を ${fetched} 回取得`)
      }
      await browser.close()
    }
  }
} finally {
  ffmpeg.kill('SIGKILL')
  rmSync(liveDir, { recursive: true, force: true })
}

log('\n=== 測れなかった項目 ===')
if (skipped.length === 0) log('  なし')
else skipped.forEach((s) => log('  SKIP: ' + s))

await finish(ng, null)

async function openLivePage(browser) {
  const page = await browser.newPage()
  await installApiStubs(page, async ({ path: p, json }) => {
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/events') return json([])
    if (p === `/api/sites/${SITE}/services`) return json([liveService])
    return json([])
  })
  const liveBase = `/api/sites/${SITE}/networks/${NETWORK_ID}/services/${SERVICE_ID}/live`
  const serve = (route, file, contentType) => {
    if (!existsSync(file)) return route.fulfill({ status: 404 })
    return route.fulfill({ status: 200, contentType, body: readFileSync(file), headers: { 'cache-control': 'no-store' } })
  }
  await page.route(`**${liveBase}/leave`, (route) => route.fulfill({ status: 204 }))
  await page.route(`**${liveBase}/*.m3u8`, (route) => {
    const name = new URL(route.request().url()).pathname.split('/').pop()
    return serve(route, path.join(liveDir, name), 'application/vnd.apple.mpegurl')
  })
  // streamer の Playlist と同じく、?profile= のプロファイルの master を返す。
  // **上の `*.m3u8` より後に登録する** --- Playwright は後から登録したルートを先に試す
  await page.route(`**${liveBase}/playlist.m3u8*`, (route) =>
    serve(route, path.join(liveDir, `${PROFILE}.m3u8`), 'application/vnd.apple.mpegurl'),
  )
  await page.route(`**${liveBase}/segments/*`, (route) => {
    const name = new URL(route.request().url()).pathname.split('/').pop()
    return serve(route, path.join(liveDir, 'segments', name), 'video/mp2t')
  })
  await page.goto(`${URL_BASE}/live?service=${COMPOSITE_ID}`, { waitUntil: 'networkidle' })
  return page
}

function countRequests(page, pattern) {
  const counter = { count: 0 }
  page.on('request', (req) => {
    if (pattern.test(new URL(req.url()).pathname)) counter.count++
  })
  return counter
}

// startPlaybackWithAnalyser は再生を始め、<video> の音を左右別の AnalyserNode に通す。
async function startPlaybackWithAnalyser(page) {
  await page.getByRole('button', { name: /再生/ }).click()
  await waitForVisibleVideo(page)
  await page.evaluate(async () => {
    const video = document.querySelector('video')
    const ctx = new AudioContext()
    const src = ctx.createMediaElementSource(video)
    const split = ctx.createChannelSplitter(2)
    const l = ctx.createAnalyser()
    const r = ctx.createAnalyser()
    l.fftSize = r.fftSize = 8192
    src.connect(split)
    split.connect(l, 0)
    split.connect(r, 1)
    src.connect(ctx.destination)
    await ctx.resume()
    const peak = (an) => {
      const b = new Float32Array(an.frequencyBinCount)
      an.getFloatFrequencyData(b)
      let m = -Infinity
      let at = 0
      for (let i = 1; i < b.length; i++) if (b[i] > m) [m, at] = [b[i], i]
      return Math.round((at * ctx.sampleRate) / an.fftSize)
    }
    window.__measure = () => ({ L: peak(l), R: peak(r), t: Number(video.currentTime.toFixed(1)) })
    await video.play()
  })
  // 標準（440/880）が鳴り始めるまで待ってから切り替える
  const got = await waitForChannels(page, 440, 880, 20_000)
  if (!got.ok) throw new Error(`再生が始まらない: ${JSON.stringify(got.last)}`)
}

// waitForVisibleVideo はプレイヤーが読み込みを終えるのを待つ。失敗したらプレイヤーの
// 表示（エラー文言）を添えて落とす --- タイムアウトだけでは原因が分からない。
async function waitForVisibleVideo(page) {
  try {
    await page.locator('video').waitFor({ timeout: 15000 })
  } catch (err) {
    const text = await page.locator('main').innerText().catch(() => '')
    throw new Error(`プレイヤーが再生状態にならない。画面: ${text.slice(0, 300)}`, { cause: err })
  }
}

async function waitForChannels(page, wantL, wantR, timeoutMs) {
  const start = Date.now()
  const first = await page.evaluate(() => window.__measure())
  let last = first
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate(() => window.__measure())
    // FFT のビン幅は 48000 / 8192 ≈ 5.9Hz
    if (Math.abs(last.L - wantL) <= 12 && Math.abs(last.R - wantR) <= 12) {
      return { ok: true, last, elapsed: Date.now() - start }
    }
    await new Promise((r) => setTimeout(r, 250))
  }
  return { ok: false, last, startTime: first.t, stalled: last.t - first.t < timeoutMs / 2000 }
}

async function waitForNativeTracks(page, timeoutMs) {
  return waitFor(async () => {
    const n = await page.evaluate(() => document.querySelector('video').audioTracks?.length ?? 0)
    return n === 3 ? n : null
  }, timeoutMs)
}

async function waitFor(probe, timeoutMs) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    const v = await probe()
    if (v !== null) return v
    await new Promise((r) => setTimeout(r, 200))
  }
  return null
}

async function waitForFile(file, timeoutMs) {
  const ok = await waitFor(async () => (existsSync(file) ? true : null), timeoutMs)
  if (ok === null) throw new Error(`${file} が ${timeoutMs} ms 以内に現れない（ffmpeg の出力を確認する）`)
}

// ensureStereoInput は L = 440Hz / R = 880Hz のステレオ AAC + 映像の TS を作る（キャッシュ）。
function ensureStereoInput(file) {
  if (existsSync(file)) return file
  log(`入力 TS を生成中... (${file})`)
  execFileSync('ffmpeg', [
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=30',
    '-f', 'lavfi', '-i', 'sine=f=440:r=48000',
    '-f', 'lavfi', '-i', 'sine=f=880:r=48000',
    '-filter_complex', '[1:a][2:a]join=inputs=2:channel_layout=stereo[a]',
    '-map', '0:v', '-map', '[a]', '-t', '20',
    '-c:v', 'mpeg2video', '-b:v', '2M', '-c:a', 'aac', '-f', 'mpegts', file,
  ])
  return file
}

// startLiveFFmpeg は BuildLiveFFmpegArgs の既定経路（1 プロファイル）と同じ形のライブ
// HLS を書き続ける。-re と -stream_loop で実時間のライブにする。
function startLiveFFmpeg(inputFile, dir) {
  mkdirSync(path.join(dir, 'segments'), { recursive: true })
  const args = [
    '-hide_banner', '-nostats', '-loglevel', 'error',
    '-re', '-stream_loop', '-1', '-i', inputFile,
    '-map', '0:v:0', '-map', '0:a:0', '-map', '0:a:0', '-map', '0:a:0',
    '-c:v', 'libx264', '-c:a', 'aac',
    '-filter:a:1', 'pan=stereo|c0=c0|c1=c0', '-filter:a:2', 'pan=stereo|c0=c1|c1=c1',
    '-preset', 'veryfast', '-force_key_frames', 'expr:gte(t,n_forced*2)',
    '-var_stream_map', 'v:0,agroup:aud a:0,agroup:aud,default:yes a:1,agroup:aud a:2,agroup:aud',
    '-master_pl_name', `${PROFILE}.m3u8`,
    '-f', 'hls', '-hls_time', '2', '-hls_list_size', '6',
    '-hls_flags', 'delete_segments+temp_file+program_date_time',
    '-hls_segment_filename', path.join(dir, 'segments', `${PROFILE}.%v_seg%05d.ts`),
    '-hls_base_url', 'segments/',
    path.join(dir, `${PROFILE}.%v.m3u8`),
  ]
  const proc = spawn('ffmpeg', args, { stdio: ['ignore', 'ignore', 'inherit'] })
  proc.on('exit', (code, signal) => {
    if (signal !== 'SIGKILL') log(`ffmpeg が終了した（code=${code} signal=${signal}）`)
  })
  return proc
}
