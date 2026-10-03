// 録画詳細の「次のエピソード」まわりを実ブラウザ・実尺の動画で判定する（issue #1018）。
//
// jsdom は再生位置も全画面もレイアウトも持たないので、終端カードが実際に出るか・読めるか、
// 自動で次の回へ移ったときに再生が始まり全画面が保たれるか・履歴が積まれるか、番組外区間が
// 破線として描かれるかはここでしか測れない。フィクスチャは chapters.mjs と同じ ffmpeg 製の
// 120 秒 WebM（Range に応じる）。ffmpeg が無い環境ではこの判定だけを skip として終了する。
//
// 見るのは次の 8 点:
//   ① 番組枠の外を録った部分が、実際に破線（暗い地に明暗が交互）で描かれ、位置が割合と一致する
//   ② 終端に達すると終端カードが映像の上に出る。ボタン 3 つが同じ行にあり、文字が読める
//      （文字色と地色のコントラスト比 4.5 以上。白地に白文字を通さない）
//   ③ 「取り消す」で自動遷移が止まる
//   ④ 取り消さなければ次の回へ自動で移る。移った先は再生を始め、全画面は保たれ、履歴が積まれる
//   ⑤ 最後の回のカードは「もう一度見る」と「この回をごみ箱へ」だけ
//   ⑥ 棚: 本数と合計サイズ、新しい順、過去の回の行を押すとその録画へ移る（スマホには出ない）
//   ⑦ シリーズへの導線が各状態（エンコード版・原本のみ・録画中・ごみ箱）でちょうど 1 つ見える。
//      1280 では棚の見出し（シリーズ名）がリンクで、タイトル下の行は出ない（棚の無いごみ箱は除く）。
//      400px ではタイトル下にリンクがあり、「シリーズ」の見出し語を持つ
//   ⑧ バーの「次のエピソード」が見える（デスクトップは日付つき、スマホはアイコンだけ）
//   ⑨ 次の回へ移ると、前の回のチャプター編集の下書き・開閉と選んだ画質が持ち越されない
//      （ページを作り直さない移動なので、録画ごとの状態は id の変化で戻す）。映像の src と
//      版タブの「再生中」が同じ画質を指す
//   ⑩ 棚のサムネイルの下端に視聴の進み線（視聴済みは全幅、途中は保存位置の割合、未視聴は無し）
//
//   cd web && corepack pnpm build
//   corepack pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 E2E_SHOT_DIR=/tmp/shots corepack pnpm e2e:recording-next-episode
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, statSync } from 'node:fs'
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
const SHOT_DIR = process.env.E2E_SHOT_DIR
if (SHOT_DIR) mkdirSync(SHOT_DIR, { recursive: true })
const ng = []

const base = {
  site: 'default',
  ruleId: 5,
  source: 'rule',
  serviceName: 'ＮＨＫ総合１・岡山',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'ニュース７',
  description: '国内外の主要なニュースを、現場からの中継や解説を交えて詳しくお伝えします。',
  series: 'ニュース７',
  durationMs: 120_000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'detected', ranges: [] },
  sizeBytes: 3_200_000_000,
  encodedAssets: [{ profile: 'h264-720p', sizeBytes: 572_000_000 }],
  encodeProfiles: ['h264-720p'],
}
// 番組は 120 秒。録画は前後 6 秒ずつ長い（132 秒）。
const ep1 = {
  ...base,
  id: 1,
  startAt: '2026-09-29T10:00:00.000Z',
  startedAt: '2026-09-29T09:59:54.000Z',
  endedAt: '2026-09-29T10:02:06.000Z',
  createdAt: '2026-09-29T10:05:00.000Z',
}
const ep2 = { ...base, id: 2, startAt: '2026-09-30T10:00:00.000Z', createdAt: '2026-09-30T10:05:00.000Z' }
const ep10 = { ...base, id: 10, startAt: '2026-09-28T10:00:00.000Z', createdAt: '2026-09-28T10:05:00.000Z', watchedAt: '2026-09-28T12:00:00.000Z' }
const originalOnly = { ...base, id: 3, title: '原本だけの回', startAt: '2026-10-01T10:00:00.000Z', createdAt: '2026-10-01T10:05:00.000Z', encodedAssets: [], encodeProfiles: [], resumePositionMs: 60_000 }
const inProgress = {
  ...base,
  id: 4,
  title: '録画中の回',
  startAt: '2026-10-02T10:00:00.000Z',
  createdAt: '2026-10-02T10:05:00.000Z',
  status: 'recording',
  sizeBytes: undefined,
  encodedAssets: [],
  encodeProfiles: [],
  ingest: { state: 'pending' },
}
const trashed = { ...base, id: 5, title: '録画した番組', series: undefined, ruleId: undefined, source: 'manual', startAt: '2026-09-27T10:00:00.000Z', createdAt: '2026-09-27T10:05:00.000Z', deletedAt: '2026-10-02T10:40:00.000Z', encodedAssets: undefined }
const trashedSeries = { ...base, id: 6, title: 'ごみ箱の回', startAt: '2026-09-26T10:00:00.000Z', createdAt: '2026-09-26T10:05:00.000Z', deletedAt: '2026-10-02T10:40:00.000Z' }
const all = [ep1, ep2, ep10, originalOnly, inProgress, trashed, trashedSeries]

/** seriesEntries は画面に見えているシリーズ画面へのリンクと、その置き場を返す。 */
async function seriesEntries(page) {
  return page.evaluate(() =>
    [...document.querySelectorAll('a[href$="/series"]')]
      .filter((a) => {
        const r = a.getBoundingClientRect()
        return r.width > 0 && r.height > 0
      })
      .map((a) => {
        const r = a.getBoundingClientRect()
        const row = a.closest('[data-testid="recording-series-links"]')
        return {
          place: a.closest('[data-testid="recording-series-shelf"] h3') ? 'shelf-heading' : row ? 'title-row' : 'other',
          text: a.textContent.trim(),
          // 見出し語は行の中のリンク以外の部分にあること（リンクの文字はシリーズ名そのもの）。
          rowLabel: row ? row.textContent.replace(a.textContent, '').trim() : '',
          underlined: getComputedStyle(a).textDecorationLine.includes('underline'),
          x: r.x,
          right: r.right,
        }
      }),
  )
}
const seriesStates = [[1, 'エンコード版'], [3, '原本のみ'], [4, '録画中'], [6, 'ごみ箱']]
const rule = {
  id: 5,
  name: 'ニュース７（平日）',
  enabled: true,
  priority: 0,
  keepOriginal: 'always',
  createdAt: '2026-01-01T00:00:00.000Z',
  updatedAt: '2026-01-01T00:00:00.000Z',
}

function ensureFixture() {
  const fixtureDir = path.join(os.tmpdir(), 'rokuban-e2e-chapters')
  const videoPath = path.join(fixtureDir, 'clip.webm')
  if (existsSync(videoPath) && statSync(videoPath).size > 0) return videoPath
  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  } catch {
    return undefined
  }
  mkdirSync(fixtureDir, { recursive: true })
  log(`判定用の動画フィクスチャを生成中... (${videoPath})`)
  execFileSync(
    'ffmpeg',
    ['-y', '-f', 'lavfi', '-i', 'testsrc=size=160x90:rate=2', '-t', '120', '-c:v', 'libvpx', '-b:v', '30k', '-pix_fmt', 'yuv420p', videoPath],
    { stdio: 'ignore' },
  )
  return existsSync(videoPath) ? videoPath : undefined
}

function rangeResponse(route, bytes, contentType) {
  const range = /bytes=(\d+)-(\d*)/.exec(route.request().headers().range ?? '')
  if (!range) {
    return route.fulfill({ status: 200, contentType, body: bytes, headers: { 'Accept-Ranges': 'bytes' } })
  }
  const start = Number(range[1])
  const end = range[2] ? Number(range[2]) : bytes.length - 1
  return route.fulfill({
    status: 206,
    contentType,
    body: bytes.subarray(start, end + 1),
    headers: { 'Accept-Ranges': 'bytes', 'Content-Range': `bytes ${start}-${end}/${bytes.length}` },
  })
}

let videoBytes = Buffer.alloc(0)
const thumbnailSVG =
  '<svg xmlns="http://www.w3.org/2000/svg" width="160" height="90"><rect width="160" height="90" fill="#263650"/><path d="M0 70 60 25l36 32 22-20 42 34v19H0Z" fill="#485d7c"/></svg>'

async function apiHandler({ path: apiPath, url, json, route }) {
  const method = route.request().method()
  if (apiPath === '/api/sites') return json(['default'])
  if (apiPath === '/api/capabilities') return json({ live: false, cmDetect: true })
  if (apiPath === '/api/breakers') return json([])
  if (apiPath === '/api/encode-profiles') return json([])
  if (apiPath === '/api/rules') return json([rule])
  if (apiPath === '/api/events') return sseKeepAlive(route)
  if (apiPath === '/api/recordings' && method === 'GET') {
    if (!url.searchParams.has('seriesOf')) return json(all)
    const series = [ep10, ep1, ep2, originalOnly, inProgress].filter((item) => item.series === 'ニュース７')
    const from = url.searchParams.get('from')
    const rows = series
      .filter((item) => from === null || Date.parse(item.startAt) >= Date.parse(from))
      .sort((a, b) => Date.parse(a.startAt) - Date.parse(b.startAt))
    return json(url.searchParams.get('order') === 'asc' ? rows : rows.reverse())
  }
  const recordingMatch = /^\/api\/recordings\/(\d+)$/.exec(apiPath)
  if (recordingMatch && method === 'GET') {
    const item = all.find((candidate) => candidate.id === Number(recordingMatch[1]))
    return item ? json(item) : json({ error: 'not found' }, 404)
  }
  if (/^\/api\/recordings\/\d+\/chapters$/.test(apiPath)) {
    return json({ source: 'auto', version: 'v1', detectionPending: false, spans: [] })
  }
  if (/^\/api\/recordings\/\d+\/drop-stats$/.test(apiPath)) return json([])
  if (/^\/api\/media\/recordings\/\d+\/file$/.test(apiPath)) return rangeResponse(route, videoBytes, 'video/webm')
  if (/^\/api\/media\/recordings\/\d+\/thumbnail$/.test(apiPath)) {
    return route.fulfill({ status: 200, contentType: 'image/svg+xml', body: thumbnailSVG })
  }
  if (/^\/api\/media\/recordings\/\d+\/(seek-tiles|subtitles)/.test(apiPath)) return route.fulfill({ status: 404 })
  return json([])
}

log(`URL: ${URL_BASE}`)
await validateFixturesOrExit(
  [
    ['ep1', ListRecordingsResponseItem, ep1],
    ['originalOnly', ListRecordingsResponseItem, originalOnly],
    ['inProgress', ListRecordingsResponseItem, inProgress],
    ['trashedSeries', ListRecordingsResponseItem, trashedSeries],
  ],
  ng,
)
await verifyBundleMatchesOrExit(URL_BASE, ng)

const videoPath = ensureFixture()
if (videoPath === undefined) {
  log('  ffmpeg が無いため、次のエピソードの実ブラウザ判定は測れない（skip）')
  await finish(ng)
}
videoBytes = readFileSync(videoPath)

const browser = await launchBrowser('chromium', { args: ['--autoplay-policy=no-user-gesture-required'] })

/** コントラスト比。背景は黒（映像）→ カードの幕 → ボタンの地の順に重ねて求める。 */
const contrastScript = () => {
  // computed style は oklab / color(srgb …) で返ることがあるので、canvas に塗って rgba へ正規化する。
  const ctx = document.createElement('canvas').getContext('2d', { willReadFrequently: true })
  const parse = (value) => {
    ctx.clearRect(0, 0, 1, 1)
    ctx.fillStyle = value
    ctx.fillRect(0, 0, 1, 1)
    const [r, g, b, a] = ctx.getImageData(0, 0, 1, 1).data
    return { r, g, b, a: a / 255 }
  }
  const over = (top, bottom) => ({
    r: top.r * top.a + bottom.r * (1 - top.a),
    g: top.g * top.a + bottom.g * (1 - top.a),
    b: top.b * top.a + bottom.b * (1 - top.a),
    a: 1,
  })
  const lum = ({ r, g, b }) => {
    const f = (c) => {
      const s = c / 255
      return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
    }
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
  }
  const card = document.querySelector('[data-testid="recording-end-card"]')
  const cardBg = over(parse(getComputedStyle(card).backgroundColor), { r: 0, g: 0, b: 0, a: 1 })
  return [...card.querySelectorAll('button')].map((button) => {
    const style = getComputedStyle(button)
    const bg = over(parse(style.backgroundColor), cardBg)
    const fg = over(parse(style.color), bg)
    const [hi, lo] = [lum(fg), lum(bg)].sort((x, y) => y - x)
    const rect = button.getBoundingClientRect()
    return { name: button.textContent.trim(), ratio: (hi + 0.05) / (lo + 0.05), top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right }
  })
}

async function newPage(width, height, colorScheme = 'light') {
  const context = await browser.newContext({ viewport: { width, height }, locale: 'ja-JP', timezoneId: 'Asia/Tokyo', colorScheme })
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  return { context, page }
}

async function openRecording(page, id) {
  await page.goto(`${URL_BASE}/recordings/${id}`, { waitUntil: 'domcontentloaded' })
  await page.evaluate(() => localStorage.clear())
  await page.locator('video').waitFor({ timeout: 15000 })
  await page.waitForFunction(() => {
    const v = document.querySelector('video')
    return v && Number.isFinite(v.duration) && v.duration > 0
  }, undefined, { timeout: 15000 })
}

/** 終端の 1.5 秒手前から再生して、終端カードが出るのを待つ。 */
async function playToEnd(page) {
  await page.locator('video').evaluate((v) => {
    v.muted = true
    v.currentTime = v.duration - 1.5
  })
  await page.waitForFunction(() => !document.querySelector('video')?.seeking, undefined, { timeout: 5000 }).catch(() => {})
  await page.locator('video').evaluate((v) => v.play())
  await page.locator('[data-testid="recording-end-card"]').waitFor({ state: 'visible', timeout: 10000 })
}

async function shot(page, name) {
  // 画面内のスクロール容器を先頭へ戻す（直前の操作でスクロールしていると見出しが切れる）。
  await page.evaluate(() => {
    for (let el = document.querySelector('[data-testid="recording-player-frame"]'); el; el = el.parentElement) el.scrollTop = 0
  })
  if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, name), animations: 'disabled' })
}

// 画素の読み取り用。スクリーンショットは別ページの canvas に復号して、横一列の明暗の切り替わりを数える。
const decoder = await (await browser.newContext()).newPage()
async function transitionsAlong(png) {
  return decoder.evaluate(async (b64) => {
    const img = new Image()
    img.src = `data:image/png;base64,${b64}`
    await img.decode()
    const canvas = document.createElement('canvas')
    canvas.width = img.width
    canvas.height = img.height
    const ctx = canvas.getContext('2d')
    ctx.drawImage(img, 0, 0)
    const y = Math.floor(img.height / 2)
    const row = ctx.getImageData(0, y, img.width, 1).data
    let changes = 0
    let bright = row[0] + row[1] + row[2] > 384
    for (let x = 1; x < img.width; x += 1) {
      const nowBright = row[x * 4] + row[x * 4 + 1] + row[x * 4 + 2] > 384
      if (nowBright !== bright) changes += 1
      bright = nowBright
    }
    return changes
  }, png.toString('base64'))
}

// ===== デスクトップ =====
{
  const { context, page } = await newPage(1280, 800)
  await openRecording(page, 1)

  log('\n=== ① 番組外区間は破線で描かれ、位置が割合と一致する ===')
  const scrubBox = await page.locator('[data-testid="seek-scrub"]').boundingBox()
  const before = page.locator('[data-testid="recorded-before-program"]')
  const after = page.locator('[data-testid="recorded-after-program"]')
  if ((await before.count()) !== 1 || (await after.count()) !== 1) {
    ng.push('① 番組外区間の要素が前後 1 つずつ無い')
  } else {
    const b = await before.boundingBox()
    const a = await after.boundingBox()
    const wantBeforeWidth = scrubBox.width * (6 / 132)
    const wantAfterLeft = scrubBox.x + scrubBox.width * (126 / 132)
    if (Math.abs(b.width - wantBeforeWidth) > 2) ng.push(`① 手前の区間の幅が違う（実際 ${b.width.toFixed(1)} / 期待 ${wantBeforeWidth.toFixed(1)}）`)
    if (Math.abs(a.x - wantAfterLeft) > 2) ng.push(`① 後ろの区間の左端が違う（実際 ${a.x.toFixed(1)} / 期待 ${wantAfterLeft.toFixed(1)}）`)
    // 破線は中央の 1 行に明暗が交互に出る。両端の縦線だけ（border-x）なら切り替わりは 2 回以内。
    const clip = { x: a.x, y: a.y, width: a.width, height: a.height }
    const changes = await transitionsAlong(await page.screenshot({ clip, animations: 'disabled' }))
    if (changes < 4) ng.push(`① 後ろの区間が破線になっていない（中央の 1 行の明暗の切り替わり ${changes} 回。幅 ${a.width.toFixed(1)}px）`)
    log(`  手前 w=${b.width.toFixed(1)} / 後ろ x=${a.x.toFixed(1)} w=${a.width.toFixed(1)} / 後ろの明暗切替 ${changes} 回`)
  }
  await shot(page, 'v3-desktop-bar-outside-program.png')

  log('\n=== ⑧ バーの次のエピソード（デスクトップは日付つき） ===')
  const next = page.locator('[data-testid="next-episode-link"]')
  const nextText = ((await next.textContent()) ?? '').trim()
  if (nextText !== '次: 9/30(水)') ng.push(`⑧ バーの次のエピソードが「次: 9/30(水)」でない（${nextText}）`)
  if ((await next.getAttribute('href')) !== '/recordings/2') ng.push('⑧ 次のエピソードの宛先が /recordings/2 でない')

  log('\n=== ⑥ 棚 ===')
  const summary = ((await page.locator('[data-testid="series-shelf-summary"]').textContent()) ?? '').trim()
  // 録画 5 件のうちこのシリーズ（series=ニュース７）は全部。原本 3.2 GB + エンコード 572 MB を持つ回が 3 本。
  if (!/^5 本 · /.test(summary)) ng.push(`⑥ 棚の見出しが「5 本 · …」でない（${summary}）`)
  const hrefs = await page.locator('[data-testid="recording-series-shelf"] li a').evaluateAll((links) => links.map((l) => l.getAttribute('href')))
  if (JSON.stringify(hrefs) !== JSON.stringify(['/recordings/4', '/recordings/3', '/recordings/2', '/recordings/1', '/recordings/10'])) {
    ng.push(`⑥ 棚の並びが新しい順（過去の回を含む）でない（${JSON.stringify(hrefs)}）`)
  }

  log('\n=== ⑩ 棚の進み線 ===')
  // 行ごとに、サムネイルの幅に対する線の塗りの幅（%）と、線がサムネイルの下端に載っているかを測る。
  const lines = await page.locator('[data-testid="recording-series-shelf"] li a').evaluateAll((links) =>
    links.map((link) => {
      const thumb = link.querySelector('img')?.parentElement?.getBoundingClientRect()
      const line = link.querySelector('[data-testid="series-shelf-progress-line"]')
      const fill = line?.firstElementChild?.getBoundingClientRect()
      const track = line?.getBoundingClientRect()
      return {
        href: link.getAttribute('href'),
        percent: fill && thumb ? Math.round((fill.width / thumb.width) * 100) : null,
        height: track?.height ?? 0,
        atBottom: track && thumb ? Math.abs(track.bottom - thumb.bottom) <= 1 : false,
      }
    }),
  )
  log(`  ${JSON.stringify(lines)}`)
  const want = { '/recordings/10': 100, '/recordings/3': 50, '/recordings/1': null, '/recordings/2': null, '/recordings/4': null }
  for (const line of lines) {
    if (line.percent !== want[line.href]) ng.push(`⑩ 棚の ${line.href} の進み線が ${line.percent}%（期待 ${want[line.href]}）`)
    if (line.percent !== null && (!line.atBottom || line.height < 3)) ng.push(`⑩ 棚の ${line.href} の進み線がサムネイルの下端に見えない（高さ ${line.height}px）`)
  }

  await page.evaluate(() => {
    window.__frame = document.querySelector('[data-testid="recording-player-frame"]')
  })
  await shot(page, 'v3-desktop-versions.png')
  await page.locator('[data-testid="recording-series-shelf"] a[href="/recordings/10"]').click()
  await page.waitForURL('**/recordings/10', { timeout: 5000 }).catch(() => ng.push('⑥ 棚の過去の回の行を押しても /recordings/10 へ移らない'))
  const sameFrame = await page.evaluate(() => window.__frame === document.querySelector('[data-testid="recording-player-frame"]')).catch(() => false)
  if (!sameFrame) ng.push('⑥ 棚から移るとプレイヤーの枠が作り直された（全画面が解除される形）')
  await page.goBack()
  await page.waitForURL('**/recordings/1', { timeout: 5000 })
  await page.locator('video').waitFor()

  log('\n=== ② 終端カードが出て、ボタンが読める ===')
  await playToEnd(page)
  const cardText = ((await page.locator('[data-testid="recording-end-card"]').textContent()) ?? '').replace(/\s+/g, ' ')
  if (!/次のエピソード · [0-3] 秒後に再生/.test(cardText)) ng.push(`② カードの文言が「次のエピソード · N 秒後に再生」でない（${cardText}）`)
  if (!/9\/30\(水\) 19:00/.test(cardText)) ng.push(`② カードに次の回の放送日時（9/30(水) 19:00）が無い（${cardText}）`)
  const buttons = await page.evaluate(contrastScript)
  const names = buttons.map((b) => b.name)
  if (JSON.stringify(names) !== JSON.stringify(['今すぐ再生', '取り消す', 'この回をごみ箱へ'])) ng.push(`② ボタンが 3 つ（今すぐ再生 / 取り消す / この回をごみ箱へ）でない（${JSON.stringify(names)}）`)
  for (const b of buttons) {
    if (b.ratio < 4.5) ng.push(`② 「${b.name}」の文字と地のコントラスト比が ${b.ratio.toFixed(2)}（4.5 未満で読めない）`)
  }
  if (buttons.some((b) => Math.abs(b.top - buttons[0].top) > 2)) ng.push('② ボタン 3 つが同じ行に並んでいない（デスクトップ）')
  const ring = await page.locator('[data-testid="end-card-countdown-ring"]').boundingBox()
  const thumb = await page.locator('[data-testid="recording-end-card"] img').boundingBox().catch(() => null)
  if (!ring || ring.width < 20) ng.push('② カウントダウンの輪が見えない')
  if (!thumb || thumb.width < 100) ng.push('② 次の回のサムネイルが見えない')
  await shot(page, 'v3-desktop-end-card.png')

  log('\n=== ③ 取り消すと自動遷移が止まる ===')
  await page.getByRole('button', { name: '取り消す' }).click()
  await page.waitForTimeout(4000)
  if (new URL(page.url()).pathname !== '/recordings/1') ng.push(`③ 取り消したのに ${new URL(page.url()).pathname} へ移った`)
  if ((await page.locator('[data-testid="recording-end-card"]').count()) !== 0) ng.push('③ 取り消してもカードが残っている')

  log('\n=== ④ 自動で次の回へ移る（再生・全画面・履歴） ===')
  await page.getByRole('button', { name: '全画面表示' }).click()
  const fullscreenOK = await page
    .waitForFunction(() => document.fullscreenElement?.getAttribute('data-testid') === 'recording-player-frame', undefined, { timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  if (!fullscreenOK) {
    log('  この環境では全画面に入れない（④ の全画面保持は測れない）')
  }
  await page.evaluate(() => {
    window.__frame = document.querySelector('[data-testid="recording-player-frame"]')
  })
  await playToEnd(page)
  await page.waitForURL('**/recordings/2', { timeout: 10000 }).catch(() => ng.push('④ 3 秒待っても次の回へ自動で移らない'))
  if (fullscreenOK) {
    const kept = await page.evaluate(() => document.fullscreenElement !== null && document.fullscreenElement === window.__frame)
    if (!kept) ng.push('④ 自動で次の回へ移ると全画面が解除された')
  }
  // 移った先は再生を始める（再生位置が進む）。
  const playing = await page
    .waitForFunction(() => {
      const v = document.querySelector('video')
      return v && !v.paused && v.currentTime > 0.5
    }, undefined, { timeout: 8000 })
    .then(() => true)
    .catch(() => false)
  if (!playing) ng.push('④ 自動で移った先が再生を始めない')
  if (fullscreenOK) await page.evaluate(() => document.fullscreenElement && document.exitFullscreen()).catch(() => {})
  await page.goBack()
  await page.waitForURL('**/recordings/1', { timeout: 5000 }).catch(() => ng.push('④ 自動遷移が履歴に積まれていない（「戻る」で /recordings/1 に戻れない）'))

  await context.close()
}

{
  // ⑨ 録画ごとの状態の持ち越し。2 つの回にカット版とチャプター（CM 1 区間）を持たせる。
  const { context, page } = await newPage(1280, 800)
  log('\n=== ⑨ 次の回へ前の回の下書きと画質を持ち越さない ===')
  const withCut = (item) => ({
    ...item,
    encodedAssets: [
      { profile: 'h264-720p', sizeBytes: 572_000_000 },
      { profile: 'h264-cut', sizeBytes: 450_000_000, cut: true, keepRanges: [{ startMs: 0, endMs: 120_000 }] },
    ],
    encodeProfiles: ['h264-720p', 'h264-cut'],
  })
  await page.route((url) => /^\/api\/recordings\/[12]$/.test(url.pathname), (route) => {
    const id = Number(new URL(route.request().url()).pathname.split('/').pop())
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(withCut(id === 1 ? ep1 : ep2)) })
  })
  await page.route((url) => /^\/api\/recordings\/\d+\/chapters$/.test(url.pathname), (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ source: 'auto', version: 'v1', detectionPending: false, spans: [{ startMs: 30_000, endMs: 45_000, label: 'CM', cut: true }] }),
    }),
  )
  // 2 話を開いてから棚で 1 話へ移り（ページを作り直さない移動）、1 話で編集モードに入って下書きを作る。
  await openRecording(page, 2)
  await page.locator('[data-testid="recording-series-shelf"] a[href="/recordings/1"]').click()
  await page.waitForURL('**/recordings/1', { timeout: 5000 })
  await page.waitForFunction(() => document.querySelector('video')?.getAttribute('src')?.includes('/recordings/1/'), undefined, { timeout: 5000 })
  const enterEditMode = async () => {
    await page.locator('[data-testid="recording-player-frame"]').hover()
    await page.getByRole('button', { name: '再生設定' }).click()
    await page.getByRole('menuitem', { name: 'チャプターを直す' }).click()
    await page.locator('[data-testid="chapter-edit-layout"]').waitFor({ timeout: 10000 })
  }
  // 下書きが綺麗なまま別の回へ移ると、移動先は編集モードで開かない（確認バーも出ない）。
  await enterEditMode()
  await page.goBack()
  await page.waitForURL('**/recordings/2', { timeout: 5000 })
  await page.waitForTimeout(500)
  if ((await page.locator('[data-testid="chapter-edit-layout"]').count()) > 0) {
    ng.push('⑨ 下書きが綺麗なまま移った 2 話が編集モードのまま')
    await finish(ng) // 以降は編集モードで始まらない前提なので続けられない。
  }
  await page.locator('[data-testid="recording-series-shelf"] a[href="/recordings/1"]').click()
  await page.waitForURL('**/recordings/1', { timeout: 5000 })
  await page.waitForFunction(() => document.querySelector('video')?.getAttribute('src')?.includes('/recordings/1/'), undefined, { timeout: 5000 })
  await enterEditMode()
  const label = page.locator('[data-testid="chapter-edit-layout"] input[aria-label="ラベル"]').first()
  await label.fill('前の回の下書き')
  if (!(await page.locator('body').textContent()).includes('未保存の変更があります')) {
    ng.push('⑨ 前提: 1 話の下書きが「未保存の変更」にならない')
  }
  // 画質はまだ選ばない（カット版では編集器そのものが消え、下書きも消えるので、持ち越しを測れない）。
  // 履歴を戻って 2 話へ移る。未保存なので画面内の確認バーが出る（捨てて移る）。
  // 待たないと 6 回中 2〜3 回は戻る操作がブロックされなかった（測定）。原因は表示更新直後でブロッカーの登録が
  // 済んでいないためと推測している（未検証）。
  await page.waitForTimeout(300)
  await page.evaluate(() => history.back())
  await page.getByRole('button', { name: '変更を捨てる' }).click({ timeout: 5000 })
  await page.waitForURL('**/recordings/2', { timeout: 5000 })
  await page.waitForFunction(() => document.querySelector('video')?.getAttribute('src')?.includes('/recordings/2/'), undefined, { timeout: 5000 })
    .catch(() => {})
  const draft = await page.evaluate(() => ({
    editing: document.querySelector('[data-testid="chapter-edit-layout"]') !== null,
    labels: [...document.querySelectorAll('input[aria-label="ラベル"]')].map((input) => input.value),
    text: document.body.textContent ?? '',
  }))
  log(`  2 話のチャプター編集: editing=${draft.editing} labels=${JSON.stringify(draft.labels)}`)
  if (draft.editing) ng.push('⑨ 1 話で入った編集モードが 2 話でも続いている')
  if (draft.labels.includes('前の回の下書き')) ng.push(`⑨ 1 話の下書きが 2 話に漏れた（${JSON.stringify(draft.labels)}）`)
  if (/未保存の変更があります|サーバー側の内容が変わりました/.test(draft.text)) ng.push('⑨ 2 話に前の回の未保存・競合の表示が出ている')

  // 2 話でカット版を選び、棚で 1 話へ移る。1 話は既定の画質に戻り、版タブの「再生中」もそれを指す。
  await page.locator('[data-testid="recording-player-frame"]').hover()
  await page.getByRole('button', { name: '再生設定' }).click()
  await page.getByRole('menuitem', { name: '画質' }).click()
  await page.getByRole('menuitemradio').filter({ hasText: 'h264-cut' }).click()
  await page.waitForFunction(() => document.querySelector('video')?.getAttribute('src')?.endsWith('/recordings/2/file?profile=h264-cut'), undefined, { timeout: 5000 })
    .catch(() => ng.push('⑨ 前提: 2 話でカット版に切り替わらない'))
  await page.keyboard.press('Escape')
  await page.locator('[data-testid="recording-series-shelf"] a[href="/recordings/1"]').click()
  await page.waitForURL('**/recordings/1', { timeout: 5000 })
  await page.waitForFunction(() => document.querySelector('video')?.getAttribute('src')?.includes('/recordings/1/'), undefined, { timeout: 5000 })
    .catch(() => {})
  const src = await page.evaluate(() => document.querySelector('video')?.getAttribute('src'))
  log(`  1 話へ移った後の映像: ${src}`)
  if (src !== '/api/media/recordings/1/file?profile=h264-720p') ng.push(`⑨ 移った先の映像が既定の画質でない（${src}）`)
  await page.getByRole('tab', { name: '版' }).click()
  const playingRows = await page.locator('[data-testid="recording-version-row"]').evaluateAll((rows) =>
    rows.filter((row) => row.textContent.includes('再生中')).map((row) => row.textContent),
  )
  if (playingRows.length !== 1 || !playingRows[0].startsWith('h264-720p')) {
    ng.push(`⑨ 版タブの「再生中」が映像の画質（h264-720p）と一致しない（${JSON.stringify(playingRows)}）`)
  }
  await shot(page, 'v3-desktop-after-next-reset.png')
  await context.close()
}

{
  const { context, page } = await newPage(1280, 800)
  log('\n=== ⑦ シリーズへの導線（1280、各状態） ===')
  for (const [id, label] of seriesStates) {
    await page.goto(`${URL_BASE}/recordings/${id}`, { waitUntil: 'domcontentloaded' })
    await page.locator('a[href$="/series"]').first().waitFor({ timeout: 10000 }).catch(() => {})
    // 棚の見出しは棚の問い合わせの後に出るので、棚がある状態では見出しのリンクを待つ。
    if (id !== 6) await page.locator('[data-testid="recording-series-shelf"] h3 a').waitFor({ timeout: 10000 }).catch(() => {})
    const entries = await seriesEntries(page)
    log(`  ${label}: ${JSON.stringify(entries)}`)
    if (entries.length !== 1) {
      ng.push(`⑦ ${label}（1280）で見えるシリーズへのリンクが ${entries.length} 個（期待 1。${JSON.stringify(entries.map((e) => e.place))}）`)
      continue
    }
    const [entry] = entries
    const want = id === 6 ? 'title-row' : 'shelf-heading'
    if (entry.place !== want) ng.push(`⑦ ${label}（1280）でシリーズへのリンクの置き場が ${entry.place}（期待 ${want}）`)
    if (want === 'shelf-heading' && !/›$/.test(entry.text)) ng.push(`⑦ ${label}（1280）で棚の見出しのリンクに「›」が無い（${entry.text}）`)
    if (!entry.underlined) ng.push(`⑦ ${label}（1280）でシリーズへのリンクがリンクの見た目（下線）でない`)
  }
  await page.goto(`${URL_BASE}/recordings/1`, { waitUntil: 'domcontentloaded' })
  await page.locator('[data-testid="recording-series-shelf"] h3 a').waitFor({ timeout: 10000 }).catch(() => {})
  await shot(page, 'v5-light-desktop-series.png')
  if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, 'v5-light-desktop-series-full.png'), fullPage: true, animations: 'disabled' })
  await context.close()
}

{
  // 最後の回: 次の回が再生できない（原本のみ・録画中）ときが「最後」。ep2 の後に再生できる行が無い構成にする。
  const { context, page } = await newPage(1280, 800)
  log('\n=== ⑤ 最後の回のカード ===')
  await page.route((url) => url.pathname === '/api/recordings' && url.searchParams.has('seriesOf'), async (route) => {
    const url = new URL(route.request().url())
    const only = [ep10, ep1, ep2].sort((a, b) => Date.parse(a.startAt) - Date.parse(b.startAt))
    const from = url.searchParams.get('from')
    const rows = only.filter((item) => from === null || Date.parse(item.startAt) >= Date.parse(from))
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(url.searchParams.get('order') === 'asc' ? rows : rows.reverse()) })
  })
  await openRecording(page, 2)
  await playToEnd(page)
  const cardText = ((await page.locator('[data-testid="recording-end-card"]').textContent()) ?? '').replace(/\s+/g, ' ')
  const names = (await page.evaluate(contrastScript)).map((b) => b.name)
  if (JSON.stringify(names) !== JSON.stringify(['もう一度見る', 'この回をごみ箱へ'])) ng.push(`⑤ 最後の回のカードのボタンが「もう一度見る / この回をごみ箱へ」でない（${JSON.stringify(names)}）`)
  if (/秒後に再生/.test(cardText)) ng.push('⑤ 最後の回のカードに自動再生の文言がある')
  for (const b of await page.evaluate(contrastScript)) {
    if (b.ratio < 4.5) ng.push(`⑤ 「${b.name}」の文字と地のコントラスト比が ${b.ratio.toFixed(2)}（4.5 未満で読めない）`)
  }
  await page.waitForTimeout(4000)
  if (new URL(page.url()).pathname !== '/recordings/2') ng.push('⑤ 最後の回なのに別の録画へ移った')
  await shot(page, 'v3-desktop-end-card-last.png')
  await context.close()
}

// ===== スマホ（400px） =====
{
  const { context, page } = await newPage(400, 800)
  await openRecording(page, 1)

  log('\n=== ⑧ スマホではアイコンだけのバー、棚は出ない ===')
  const next = page.locator('[data-testid="next-episode-link"]')
  const box = await next.boundingBox()
  if (!box || box.x < 0 || box.x + box.width > 400) ng.push('⑧ スマホでバーの次のエピソードが画面内に見えない')
  if (((await next.innerText()) ?? '').trim() !== '') ng.push('⑧ スマホのバーの次のエピソードに文字が出ている（アイコンだけのはず）')
  if (await page.locator('[data-testid="recording-series-shelf"]').isVisible().catch(() => false)) ng.push('⑥ スマホに棚が出ている')

  log('\n=== ⑦ シリーズへの導線（400px、各状態） ===')
  for (const [id, label] of seriesStates) {
    await page.goto(`${URL_BASE}/recordings/${id}`, { waitUntil: 'domcontentloaded' })
    await page.locator('a[href$="/series"]').first().waitFor({ timeout: 10000 }).catch(() => {})
    const entries = await seriesEntries(page)
    log(`  ${label}: ${JSON.stringify(entries)}`)
    if (entries.length !== 1) {
      ng.push(`⑦ ${label}（400px）で見えるシリーズへのリンクが ${entries.length} 個（期待 1）`)
      continue
    }
    const [entry] = entries
    if (entry.place !== 'title-row') ng.push(`⑦ ${label}（400px）でシリーズへのリンクがタイトル下に無い（${entry.place}）`)
    if (!entry.rowLabel.includes('シリーズ')) ng.push(`⑦ ${label}（400px）でタイトル下のリンクに「シリーズ」の見出し語が無い（「${entry.rowLabel}」）`)
    if (!entry.underlined) ng.push(`⑦ ${label}（400px）でタイトル下のシリーズ名がリンクの見た目（下線）でない`)
    if (entry.x < 0 || entry.right > 400) ng.push(`⑦ ${label}（400px）でシリーズへのリンクが画面からはみ出す`)
  }
  await openRecording(page, 1)
  await shot(page, 'v3-phone-programme.png')
  await shot(page, 'v5-light-phone-series.png')

  log('\n=== ② スマホの終端カード ===')
  await playToEnd(page)
  const frame = await page.locator('[data-testid="recording-player-frame"]').boundingBox()
  const buttons = await page.evaluate(contrastScript)
  for (const b of buttons) {
    if (b.ratio < 4.5) ng.push(`② スマホ: 「${b.name}」のコントラスト比が ${b.ratio.toFixed(2)}`)
    if (b.left < frame.x - 0.5 || b.right > frame.x + frame.width + 0.5 || b.bottom > frame.y + frame.height + 0.5) {
      ng.push(`② スマホ: 「${b.name}」が映像の枠からはみ出している`)
    }
  }
  await shot(page, 'v3-phone-end-card.png')
  await context.close()
}

// ===== 証跡のスクリーンショット（E2E_SHOT_DIR があるときだけ。判定には使わない） =====
if (SHOT_DIR) {
  for (const scheme of ['light', 'dark']) {
    for (const vp of [
      { name: 'desktop', width: 1280, height: 800 },
      { name: 'phone', width: 400, height: 800 },
    ]) {
      const { context, page } = await newPage(vp.width, vp.height, scheme)
      const out = (state) => `v3-${scheme}-${vp.name}-${state}.png`
      await openRecording(page, 1)
      if (vp.name === 'desktop') {
        // 棚（進み線）まで入るよう、ページ全体も撮る。
        await page.locator('[data-testid="recording-series-shelf"] img').first().waitFor()
        await page.screenshot({ path: path.join(SHOT_DIR, out('full')), fullPage: true, animations: 'disabled' })
        await page.locator('[data-testid="recording-series-shelf"]').screenshot({ path: path.join(SHOT_DIR, out('shelf')), animations: 'disabled' })
        await page.getByRole('button', { name: '録画のその他の操作' }).click()
        await page.getByRole('menu').waitFor()
        await shot(page, out('detail-menu'))
        await page.keyboard.press('Escape')
      } else {
        await shot(page, out('detail'))
        await page.getByRole('tab', { name: '版' }).click()
        await shot(page, out('detail-versions'))
      }
      await playToEnd(page)
      await shot(page, out('end-card'))
      for (const [id, state] of [[3, 'original-only'], [4, 'recording'], [5, 'trash']]) {
        await page.goto(`${URL_BASE}/recordings/${id}`, { waitUntil: 'domcontentloaded' })
        await page.getByRole('tab').first().waitFor({ timeout: 10000 })
        await shot(page, out(state))
      }
      await context.close()
    }
    // 最後の回のカード（次が再生できない構成）。
    const { context, page } = await newPage(1280, 800, scheme)
    await page.route((url) => url.pathname === '/api/recordings' && url.searchParams.has('seriesOf'), async (route) => {
      const url = new URL(route.request().url())
      const only = [ep10, ep1, ep2].sort((a, b) => Date.parse(a.startAt) - Date.parse(b.startAt))
      const from = url.searchParams.get('from')
      const rows = only.filter((item) => from === null || Date.parse(item.startAt) >= Date.parse(from))
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(url.searchParams.get('order') === 'asc' ? rows : rows.reverse()) })
    })
    await openRecording(page, 2)
    await playToEnd(page)
    await shot(page, `v3-${scheme}-desktop-end-card-last.png`)
    await context.close()
  }
}

await decoder.context().close()
await finish(ng, browser)
