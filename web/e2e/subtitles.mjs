// ARIB 字幕（issue #430）の受け入れ判定。jsdom では測れないものだけをここで見る
// （e2e/README.md）: `<track>` が実ブラウザで cue を読み込むこと、hls.js が
// master playlist の EXT-X-MEDIA subtitles rendition から実際に字幕トラックを
// 作ること。どちらも Vitest の `vi.mock` によるフェイクの配線検査では見えない
// （「配線が呼ばれること」までしか分からない。jsdom の <video> は実際の
// TextTrack 読み込みも hls.js の実処理も行わない）。
//
// **mirakc も実チューナーも DB も要らない。** VOD は録画詳細ページの
// `/api/**` を丸ごと `page.route` で差し替え（design.mjs と同じ手）、ライブは
// `live.mjs` と違って `resolveServiceId` で実サーバーの DB へ問い合わせる経路を
// 使わず、`/api/sites` / `/api/sites/{site}/services` も自前で差し替える
// （このスクリプトは preview サーバー1つだけで完結する）。
//
// ライブのプレイリスト/セグメント/字幕フィクスチャは実 ffmpeg で生成した本物の
// HLS（H.264/AAC + WebVTT rendition）を使う --- 中身がでたらめな .ts だと
// hls.js の demux が fatal error を出して destroy() され、字幕トラックの有無を
// 見る前に状態が消えてしまう（実際にこのスクリプトを書く過程で踏んだ）。
//
//   cd web && pnpm build
//   pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:subtitles
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { ListRecordingsResponseItem, ListServicesResponseItem } from '../src/api/zod.ts'
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
const ng = []
const skipped = []

const recording = {
  id: 1,
  site: SITE,
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: '字幕付き録画',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  keepOriginal: 'always', cmDetection: { state: 'disabled' },
  sizeBytes: 500_000_000,
  encodedAssets: [{ profile: 'h264', sizeBytes: 400_000_000 }],
  createdAt: '2026-01-02T12:30:00Z',
}

const NETWORK_ID = 1
const SERVICE_ID = 9101
const COMPOSITE_ID = NETWORK_ID * 100_000 + SERVICE_ID
const liveService = {
  id: COMPOSITE_ID,
  networkId: NETWORK_ID,
  serviceId: SERVICE_ID,
  name: 'テスト局',
  channelType: 'GR',
  channel: '99',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: false,
}

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(
  [
    ['recording', ListRecordingsResponseItem, recording],
    ['service', ListServicesResponseItem, liveService],
  ],
  ng,
)

await verifyBundleMatchesOrExit(URL_BASE, ng)

const engine = process.env.E2E_BROWSER ?? 'chromium'
const browser = await launchBrowser(engine)
log(`\n=== 実ブラウザ: ${engine} ===`)

// ============================================================
// ① VOD: UI の字幕ボタンで track.mode が showing になり、実ブラウザで cue を読み込み、
//    操作バーと重ならない負の VTTCue.line が適用される。
// ============================================================
log('\n=== ① VOD: <track> が実ブラウザで WebVTT の cue を読み込む ===')
{
  const vtt = 'WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nHello\n\n00:00:02.000 --> 00:00:04.000\nWorld\n'

  const page = await browser.newPage()
  await installApiStubs(page, async ({ path: p, url, json, route }) => {
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: true })
    if (p === '/api/breakers') return json([])
    if (p === '/api/events') return sseKeepAlive(route)
    if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(p)) return route.fulfill({ status: 404 })
    if (p === '/api/recordings' && route.request().method() === 'GET') return json([recording])
    if (/^\/api\/recordings\/\d+$/.test(p)) return json(recording)
    if (/^\/api\/media\/recordings\/\d+\/file$/.test(p)) {
      if (url.searchParams.get('track') === 'subtitles') {
        return route.fulfill({ status: 200, contentType: 'text/vtt; charset=utf-8', body: vtt })
      }
      // 動画本体はダミーバイト列でよい --- <track> の cue 読み込みは
      // <video> 自身のデコード成否と独立か（下の実測で確認する）。
      return route.fulfill({ status: 200, contentType: 'video/mp4', body: Buffer.alloc(1024) })
    }
    return json([])
  })

  await page.goto(`${URL_BASE}/recordings/1`, { waitUntil: 'domcontentloaded' })
  await page.locator('video').waitFor({ timeout: 15000 })
  await page.locator('[data-testid="player-controls"] button[aria-label="字幕"]').click()

  const result = await page.evaluate(async () => {
    const video = document.querySelector('video')
    const track = Array.from(video?.textTracks ?? []).find((item) => item.kind === 'subtitles')
    if (!track) return { trackFound: false }
    // mode を UI から showing にした後、空リストを読み込み済みと誤認しないよう
    // cues が入るまで待つ。cue.line はコントロール行の高さを反映している。
    const deadline = Date.now() + 5000
    while ((track.cues?.length ?? 0) === 0 && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 50))
    }
    return {
      trackFound: true,
      kind: track.kind,
      mode: track.mode,
      cueCount: track.cues?.length ?? null,
      cueLine: track.cues?.[0] && 'line' in track.cues[0] ? track.cues[0].line : null,
    }
  })

  if (!result.trackFound) {
    ng.push('① VOD: <video> に <track> が見つからない')
  } else if (result.kind !== 'subtitles') {
    ng.push(`① VOD: track.kind = ${result.kind}, want subtitles`)
  } else if (result.mode !== 'showing') {
    ng.push(`① VOD: track.mode = ${result.mode}, want showing from player settings`)
  } else if (!(result.cueCount > 0)) {
    ng.push(`① VOD: textTracks[0].cues.length = ${result.cueCount}, want > 0`)
  } else if (typeof result.cueLine !== 'number' || result.cueLine >= 0) {
    ng.push(`① VOD: cue.line = ${result.cueLine}, want a raised numeric line`)
  } else {
    log(`  OK: cues = ${result.cueCount}, cue.line = ${result.cueLine}`)
  }
  await page.close()
}

// ============================================================
// ② ライブ: master playlist の字幕 rendition から hls.js が
//    subtitleTracks を 1 本以上見せる。
// ============================================================
log('\n=== ② ライブ: HLS 字幕 track・キー操作・cue 位置 ===')
{
  const FIXTURE_DIR = path.join(os.tmpdir(), 'rokuban-e2e-subtitle-live-fixture')
  const built = ensureCaptionFixture(FIXTURE_DIR)
  if (!built) {
    skipped.push('② ライブ字幕 rendition: ffmpeg が無いためフィクスチャを生成できず測れない')
  } else {
    const page = await browser.newPage()
    await installApiStubs(page, async ({ path: p, json }) => {
      if (p === '/api/sites') return json([SITE])
      if (p === '/api/capabilities') return json({ live: true })
      if (p === '/api/breakers') return json([])
      if (p === '/api/events') return json([]) // このページは events を使わない
      if (p === `/api/sites/${SITE}/services`) {
        return json([liveService])
      }
      return json([])
    })

    const liveBase = `/api/sites/${SITE}/networks/${NETWORK_ID}/services/${SERVICE_ID}/live`
    await page.route(`**${liveBase}/leave`, (route) => route.fulfill({ status: 204 }))
    await page.route(`**${liveBase}/playlist.m3u8*`, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: readFileSync(path.join(FIXTURE_DIR, 'playlist.m3u8')),
      }),
    )
    // variant / 字幕 playlist は `.../live/{name}` で配信される
    // （internal/streamer/live.go の Segment、captions 有効時のみ）。
    await page.route(`**${liveBase}/*.m3u8`, (route) => {
      const name = new URL(route.request().url()).pathname.split('/').pop()
      const file = path.join(FIXTURE_DIR, name)
      if (!existsSync(file)) return route.fulfill({ status: 404 })
      return route.fulfill({
        status: 200,
        contentType: 'application/vnd.apple.mpegurl',
        body: readFileSync(file),
      })
    })
    await page.route(`**${liveBase}/segments/*`, (route) => {
      const name = new URL(route.request().url()).pathname.split('/').pop()
      // 実 ffmpeg は VTT セグメントを `segments/` ではなく出力ディレクトリの
      // 直下に書く（`-hls_base_url segments/` は playlist の参照 URI にだけ
      // 付き、`-hls_subtitle_path` の実体には効かない。internal/streamer の
      // Segment ハンドラも同じ非対称を踏まえて .vtt/.m3u8 は dir 直下、.ts は
      // segments/ 配下から読む --- ここも同じ形にする）。
      const file = name.endsWith('.vtt')
        ? path.join(FIXTURE_DIR, name)
        : path.join(FIXTURE_DIR, 'segments', name)
      if (!existsSync(file)) return route.fulfill({ status: 404 })
      const contentType = name.endsWith('.vtt') ? 'text/vtt; charset=utf-8' : 'video/mp2t'
      return route.fulfill({ status: 200, contentType, body: readFileSync(file) })
    })

    await page.goto(`${URL_BASE}/live?service=${COMPOSITE_ID}`, { waitUntil: 'networkidle' })
    await page.getByRole('button', { name: /再生/ }).click()

    // hls.js は React ref にしか保持されていない（window には出ていない）ので、
    // 字幕トラックは <video>.textTracks 経由で見る。track.kind / cue.line はブラウザが
    // HLS rendition を解釈した結果で、hls.js の内部状態には依存しない。
    let result
    try {
      await page.locator('video').waitFor({ timeout: 15000 })
      await page.locator('video').hover()
      const subtitleButton = page.getByTestId('player-controls').getByRole('button', { name: '字幕' })
      if (await subtitleButton.getAttribute('aria-pressed') !== 'true') await subtitleButton.click()
      result = await page.evaluate(async () => {
        const video = document.querySelector('video')
        const deadline = Date.now() + 10000
        let subtitleTracks = []
        while (Date.now() < deadline) {
          subtitleTracks = Array.from(video.textTracks).filter((t) => t.kind === 'subtitles')
          if (subtitleTracks.some((track) => (track.cues?.length ?? 0) > 0)) break
          await new Promise((r) => setTimeout(r, 100))
        }
        return {
          videoFound: true,
          subtitleTrackCount: subtitleTracks.length,
          tracks: subtitleTracks.map((track) => ({
            kind: track.kind,
            label: track.label,
            mode: track.mode,
            cueCount: track.cues?.length ?? 0,
            cueLine: track.cues?.[0] && 'line' in track.cues[0] ? track.cues[0].line : null,
          })),
        }
      })
    } catch (err) {
      result = { videoFound: false, error: String(err) }
    }

    if (!result.videoFound) {
      ng.push(`② ライブ: <video> が現れない（${result.error}）`)
    } else if (!(result.subtitleTrackCount > 0)) {
      ng.push(`② ライブ: video.textTracks に subtitles 種別が 0 本（hls.js が master の EXT-X-MEDIA subtitles rendition を反映していない）`)
    } else {
      log(`  HLS 字幕 tracks: ${JSON.stringify(result.tracks)}`)
      const cueLine = result.tracks.find((track) => track.cueCount > 0)?.cueLine
      if (result.tracks.every((track) => track.cueCount === 0) && engine === 'webkit') {
        log('  WebKit の native HLS は cue を JS から公開しないため line 判定は未計測')
      } else if (typeof cueLine !== 'number' || cueLine >= 0) {
        ng.push(`② ライブ: HLS 字幕 cue が操作バーの上へ移動していない (${JSON.stringify(result.tracks)})`)
      }

      const liveVideo = page.locator('video')
      await liveVideo.evaluate((video) => {
        video.pause()
        video.muted = false
        video.focus()
      })
      await page.keyboard.press('Space')
      const spaceStarted = await page.waitForFunction(
        () => document.querySelector('video')?.paused === false,
        undefined,
        { timeout: 3000 },
      ).then(() => true).catch(() => false)
      if (!spaceStarted) ng.push('② ライブ: video にフォーカス中の Space で再生が始まらない')
      await liveVideo.evaluate((video) => video.focus())
      await page.keyboard.press('m')
      if (!(await liveVideo.evaluate((video) => video.muted))) {
        ng.push('② ライブ: video にフォーカス中の M でミュートが切り替わらない')
      }
      await liveVideo.evaluate((video) => video.focus())
      await page.keyboard.press('f')
      const fullscreenEntered = await page.waitForFunction(
        () => document.fullscreenElement !== null,
        undefined,
        { timeout: 3000 },
      ).then(() => true).catch(() => false)
      const fullscreenFrame = await page.evaluate(() => {
        const full = document.fullscreenElement
        return full !== null && full !== document.querySelector('video') &&
          full.contains(document.querySelector('video')) &&
          full.querySelector('[data-testid="player-controls"]') !== null
      })
      if (!fullscreenEntered || !fullscreenFrame) {
        ng.push('② ライブ: F が video だけでなく操作バーを含む player frame を全画面にしない')
      }
      if (fullscreenEntered) {
        await page.keyboard.press('Escape')
        await page.waitForFunction(() => document.fullscreenElement === null, undefined, { timeout: 3000 }).catch(() => {})
      }
    }
    await page.close()
  }
}

log('\n=== 測れなかった項目 ===')
if (skipped.length === 0) log('  なし')
else skipped.forEach((s) => log('  SKIP: ' + s))

await finish(ng, browser)

/**
 * ensureCaptionFixture は libaribcaption 抜きでも作れる字幕付き HLS フィクスチャ
 * （testsrc + sine の映像/音声 + SRT 由来の WebVTT rendition）を生成する。
 * ARIB 字幕そのもの（libaribcaption によるデコード）はこの e2e では検証しない
 * --- 見ているのはブラウザ側（hls.js）が master playlist の EXT-X-MEDIA
 * subtitles rendition をどう扱うかだけで、字幕データの生成元（ARIB か SRT か）
 * とは無関係（internal/streamer 側で libaribcaption が無い環境でも同じ理由で
 * ARIB 字幕の実デコードは検証できないのと対をなす制約）。
 */
function ensureCaptionFixture(fixtureDir) {
  const masterPath = path.join(fixtureDir, 'playlist.m3u8')
  if (existsSync(masterPath)) {
    log(`フィクスチャは既にある（${fixtureDir}）`)
    return true
  }
  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return false
  }

  mkdirSync(path.join(fixtureDir, 'segments'), { recursive: true })
  const srtPath = path.join(fixtureDir, 'caption.srt')
  execFileSync('sh', [
    '-c',
    `cat > '${srtPath}' <<'EOF'
1
00:00:00,000 --> 00:00:05,000
Hello world
EOF`,
  ])

  log(`フィクスチャを生成中... (${fixtureDir})`)
  execFileSync('ffmpeg', [
    '-hide_banner', '-nostats', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc=size=640x360:rate=25',
    '-f', 'lavfi', '-i', 'sine=frequency=440',
    '-i', srtPath,
    '-t', '6',
    '-map', '0:v:0', '-map', '1:a:0', '-map', '2:s:0',
    '-c:v', 'libx264', '-profile:v', 'baseline', '-level', '3.0', '-pix_fmt', 'yuv420p', '-preset', 'veryfast',
    '-c:a', 'aac', '-b:a', '64k',
    '-c:s', 'webvtt',
    '-var_stream_map', 'v:0,a:0,s:0,sgroup:subs',
    '-master_pl_name', 'playlist.m3u8',
    '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0', '-hls_flags', 'independent_segments',
    '-hls_base_url', 'segments/',
    '-hls_segment_filename', path.join(fixtureDir, 'segments', '%v_seg%05d.ts'),
    '-hls_subtitle_path', path.join(fixtureDir, 'subtitles_%v.m3u8'),
    path.join(fixtureDir, 'playlist_%v.m3u8'),
  ])
  // 字幕 VTT セグメントは ffmpeg が独自の命名（例: playlist_00.vtt）で
  // 書き出す。字幕 playlist が参照する相対名は `segments/<basename>` なので、
  // 配信側（page.route の `**/live/segments/*`）が同じディレクトリを見れば
  // そのまま解決できる --- リネームは不要。
  return true
}
