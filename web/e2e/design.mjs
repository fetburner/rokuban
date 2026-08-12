// デザイン（色）の受け入れ判定とスクリーンショット。**色は jsdom で測れない**ので、
// ここが視覚的契約の唯一のオラクルになる（e2e/README.md、docs/frontend/design.md）。
//
// やること:
//   ① 主要画面 × ライト / ダーク × デスクトップ / モバイルを撮って
//      e2e/screenshots/ に置く（人が見るための成果物。追跡しない）
//   ② 撮ったページの上で色を実測して合否を出す（機械判定）。**この PR が変えた
//      状態色ぜんぶ**を覆う --- 1 箇所でも判定の外に置くと、そこだけ既定値へ
//      静かに戻っても全部緑のまま通る:
//      - 面（body / ヘッダ / ナビ / 一覧の行）の地が無彩か
//      - 録画中バッジがタリーレッドの「塗り」か / 失敗バッジが destructive の淡い地か
//      - チューナー不足・ルールの「条件なし」が琥珀か
//      - 番組リストの時刻に信号色が付いて**いない**か
//      - 現在時刻の線と札がタリーレッドか / 容量超過の帯の罫線が琥珀か
//      - 上記すべての WCAG コントラスト（文字 4.5 / 面と線 3）
//   ③ 和文が実際に Noto Sans JP で、英数字が実際に Geist で描画されているか
//      （CDP `CSS.getPlatformFontsForNode`）と、和文まじりの文字列でも
//      tabular-nums が実際に等幅を作っているか（DOM の実測幅）
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` でブラウザ側から
// 丸ごと差し替える（e2e/live.mjs が HLS でやっているのと同じ手）。サーバーには
// SPA（index.html + dist の資産）を返す仕事しか残らないので、
// `pnpm preview` でも rokuban 本体でもよい。
//
//   pnpm build && pnpm preview --port 4173 &
//   E2E_URL=http://localhost:4173 pnpm e2e:design
//
// 合格なら exit 0、1 つでも NG なら exit 1。判定の詳細は e2e/README.md §デザイン。
import { mkdirSync, rmSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const OUT_DIR =
  process.env.E2E_SHOT_DIR ??
  path.join(path.dirname(fileURLToPath(import.meta.url)), 'screenshots')

/**
 * 時刻を固定する。番組表・「いま」の線・録画の日時がショットごとに動くと、
 * 前回との差が「実装を変えたから」なのか「時間が経ったから」なのか分からなくなる。
 * `clock.setFixedTime` は Date だけを固定してタイマーは動かす（`pauseAt` と違い、
 * React Query や debounce を止めない）。
 */
const FIXED_NOW = new Date('2026-08-12T21:34:00+09:00')

const ng = []
const log = (...a) => console.log(...a)

// --- スタブ（API の応答） ---------------------------------------------------

const SITE = 'default'
const HOUR = 3_600_000

const services = [
  { networkId: 32736, serviceId: 1024, name: 'NHK総合', channelType: 'GR', channel: '27', remoteControlKeyId: 1, hasLogoData: false, hasPrograms: true },
  { networkId: 32737, serviceId: 1032, name: 'NHKEテレ', channelType: 'GR', channel: '26', remoteControlKeyId: 2, hasLogoData: false, hasPrograms: true },
  { networkId: 32738, serviceId: 1040, name: 'テレビ大阪', channelType: 'GR', channel: '18', remoteControlKeyId: 7, hasLogoData: false, hasPrograms: true },
  { networkId: 4, serviceId: 101, name: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', remoteControlKeyId: 0, hasLogoData: false, hasPrograms: true },
]

/** 番組名は固定の輪番。ジャンルの淡色が並ぶ様子を見たいので lv1 も回す。 */
const titles = [
  ['ニュース７', 0], ['大相撲中継', 1], ['あさイチ', 2], ['連続テレビ小説', 3],
  ['クラシック音楽館', 4], ['ブラタモリ', 5], ['日曜洋画劇場', 6], ['アニメ劇場', 7],
]

/**
 * programsFor は要求された窓（start / end）を実際に埋める番組を作る。
 * 窓を無視して固定配列を返すと、画面が窓をどう決めているかに依存して
 * 「たまたま空」のショットが撮れてしまう。
 */
function programsFor(startISO, endISO, serviceIds) {
  const start = Date.parse(startISO)
  const end = Date.parse(endISO)
  const targets = serviceIds?.length
    ? services.filter((s) => serviceIds.includes(String(s.serviceId)))
    : services
  const out = []
  // 30 分境界に丸めた「窓より 1 コマ前」から並べる。放送中の番組（開始が窓より前）
  // を必ず 1 つ含めるため --- ここが空だと ON AIR の判定が撮れない。
  const slot = 1800_000
  for (const service of targets) {
    let t = Math.floor(start / slot) * slot - slot
    let i = service.serviceId % titles.length
    while (t < end) {
      const [name, genre] = titles[i % titles.length]
      const duration = (i % 3 === 0 ? 2 : 1) * slot
      out.push({
        programId: Math.floor(t / 1000) * 100 + (service.serviceId % 100),
        networkId: service.networkId,
        serviceId: service.serviceId,
        eventId: i + 1,
        startAt: new Date(t).toISOString(),
        endAt: new Date(t + duration).toISOString(),
        durationMs: duration,
        name: `${name}`,
        description: '副調整室の計器盤。地は無彩 3 値、色は信号のみ。',
        genres: [genre],
        isFree: i % 5 !== 0,
      })
      t += duration
      i++
    }
  }
  return out
}

const nowMs = FIXED_NOW.getTime()
const iso = (ms) => new Date(ms).toISOString()

const reservations = [
  { id: 1, site: SITE, programId: 9001, source: 'rule', state: 'active', title: '連続テレビ小説', startAt: iso(nowMs + HOUR), durationMs: 900_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 2, site: SITE, programId: 9002, source: 'manual', state: 'active', title: '大相撲中継', startAt: iso(nowMs + 2 * HOUR), durationMs: 5_400_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 3, site: SITE, programId: 9003, source: 'rule', state: 'detached', title: 'クラシック音楽館', startAt: iso(nowMs + 5 * HOUR), durationMs: 3_600_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
  { id: 4, site: SITE, programId: 9004, source: 'rule', state: 'orphaned', title: '日曜洋画劇場', startAt: iso(nowMs + 26 * HOUR), durationMs: 7_200_000, createdAt: iso(nowMs - HOUR), updatedAt: iso(nowMs - HOUR), skip: false },
]

/** 予約 2 の時間帯に重ねる。琥珀の警告バッジ・帯を必ず 1 つ出すため。 */
const overages = [
  { site: SITE, startAt: iso(nowMs + 2 * HOUR), endAt: iso(nowMs + 3 * HOUR), shortfall: 1, jammedTypes: ['BS'] },
]

const recordings = [
  { id: 11, site: SITE, source: 'rule', serviceName: 'NHK総合', channelType: 'GR', channel: '27', networkId: 32736, serviceId: 1024, eventId: 11, title: 'ニュース７', startAt: iso(nowMs - 600_000), durationMs: 1_800_000, status: 'recording', createdAt: iso(nowMs - 600_000), startedAt: iso(nowMs - 600_000) },
  { id: 12, site: SITE, source: 'manual', serviceName: 'ＮＨＫＢＳ', channelType: 'BS', channel: 'BS15_0', networkId: 4, serviceId: 101, eventId: 12, title: 'クラシック音楽館', startAt: iso(nowMs - 26 * HOUR), durationMs: 5_400_000, status: 'finished', sizeBytes: 8_123_456_789, createdAt: iso(nowMs - 26 * HOUR), dropSummary: { drops: 12, errors: 0, scrambled: 3 } },
  { id: 13, site: SITE, source: 'rule', serviceName: 'テレビ大阪', channelType: 'GR', channel: '18', networkId: 32738, serviceId: 1040, eventId: 13, title: 'アニメ劇場', startAt: iso(nowMs - 50 * HOUR), durationMs: 1_800_000, status: 'failed', createdAt: iso(nowMs - 50 * HOUR) },
  { id: 14, site: SITE, source: 'rule', serviceName: 'NHKEテレ', channelType: 'GR', channel: '26', networkId: 32737, serviceId: 1032, eventId: 14, title: '連続テレビ小説', startAt: iso(nowMs - 74 * HOUR), durationMs: 900_000, status: 'finished', sizeBytes: 1_234_567_890, createdAt: iso(nowMs - 74 * HOUR) },
]

const rules = [
  { id: 1, name: '朝ドラ', enabled: true, priority: 10, keepOriginal: 'always', textMatches: [{ field: 'name', kind: 'contains', value: '連続テレビ小説' }], createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
  { id: 2, name: '（条件なし）', enabled: false, priority: 20, keepOriginal: 'until_encoded', createdAt: iso(nowMs - 100 * HOUR), updatedAt: iso(nowMs - 100 * HOUR) },
]

const breakers = [
  { site: SITE, name: 'ruler_deletes', trippedAt: iso(nowMs - 3 * HOUR), pending: 42, threshold: 20, detail: { total: 42, programs: [{ programId: 9101, title: '大相撲中継' }, { programId: 9102, title: 'ブラタモリ' }] } },
]

/**
 * installApiStubs は `/api/**` をすべてブラウザ側で差し替える。
 *
 * `withBreaker` でサーキットブレーカーのバナー（destructive の帯）を出し分ける。
 * バナーは全ページに居座る要素なので、既定では出さない --- 出したままだと
 * どのショットもバナー込みになり、ページ本体の地の判定に混ざる。
 */
async function installApiStubs(page, { withBreaker = false } = {}) {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    // SSE は 204 で「つなぎ直さずに諦めさせる」。text/event-stream を返すと
    // 接続が開いたままになり networkidle に到達しない
    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/breakers') return json(withBreaker ? breakers : [])
    if (p === '/api/encode-profiles') return json([{ name: 'hevc-1080p', container: 'mp4' }])
    if (p === '/api/rules') return json(rules)
    if (p === '/api/reservations') return json(reservations)
    if (p === '/api/capacity/overages') return json(overages)
    if (p === '/api/recordings') return json(recordings)
    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(p)) return json([])
    // サムネイルは 404 に落として実装側のプレースホルダを撮る（画像を作らない）
    if (/^\/api\/recordings\/\d+\/thumbnail$/.test(p)) return route.fulfill({ status: 404 })
    if (p === `/api/sites/${SITE}/services`) return json(services)
    if (p === `/api/sites/${SITE}/programs`) {
      return json(
        programsFor(
          url.searchParams.get('start') ?? iso(nowMs),
          url.searchParams.get('end') ?? iso(nowMs + 6 * HOUR),
          url.searchParams.getAll('serviceId'),
        ),
      )
    }
    if (/\/overlaps$/.test(p)) return json({ count: 0, reservations: [] })
    if (/\/programs\/\d+$/.test(p)) return json({ extended: {}, audios: [] })
    if (/\/reservation$/.test(p)) return route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' })
    return json([])
  })
}

// --- 色の読み取り -----------------------------------------------------------

/**
 * ページの中で色を sRGB のバイト列に落とすスクリプト。
 *
 * **`getComputedStyle()` の戻り値をそのまま正規表現で読んではいけない。**
 * トークンが oklch なので Chromium は計算値も `oklch(0.56 0.215 27)` のまま返す
 * （`rgb(...)` に落ちるという思い込みで書いた最初の版は、全部の判定が
 * 「色が読めない = null」で素通りした）。canvas の `fillStyle` も同じ文字列を
 * 返すので、1px 塗って `getImageData` で実際の画素を採る。
 */
const readColor = (el, prop) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const toRgba = (v) => {
    ctx.clearRect(0, 0, 1, 1)
    ctx.fillStyle = v
    ctx.fillRect(0, 0, 1, 1)
    const d = ctx.getImageData(0, 0, 1, 1).data
    return [d[0], d[1], d[2], d[3]]
  }

  // backdrop: この要素の**文字や罫線が実際に乗っている面**。自分の背景から
  // 祖先へ遡り、不透明な面に当たるまで重ねて合成する。
  //
  // **要素自身の background-color だけを見てはいけない。** 淡い地を持つのが
  // 外側のバッジで、文字を持つのが内側の span、という組み方は普通にある
  // （容量超過バッジがそう）。内側だけ見ると背景は透明なので、合成が恒等関数に
  // なり「地の上での比」を測ってしまう --- まさにこの判定が防ぐはずだった誤り。
  const layers = []
  let reachedOpaque = false
  for (let node = el; node; node = node.parentElement) {
    const c = toRgba(getComputedStyle(node).backgroundColor)
    if (c[3] === 0) continue
    layers.push(c)
    if (c[3] >= 255) {
      reachedOpaque = true
      break
    }
  }
  // 不透明な面に到達できなければ白を仮定するしかないが、それは**測定ではなく
  // 捏造**。`reachedOpaque` を返して呼び出し側で落とす（遡りが 1 段で止まる
  // regression は、ライトでは白 ≒ 紙白なので比がほとんど変わらず素通りする）
  let backdrop = [255, 255, 255, 255]
  for (let i = layers.length - 1; i >= 0; i--) {
    const a = layers[i][3] / 255
    backdrop = [
      layers[i][0] * a + backdrop[0] * (1 - a),
      layers[i][1] * a + backdrop[1] * (1 - a),
      layers[i][2] * a + backdrop[2] * (1 - a),
      255,
    ]
  }

  const value = getComputedStyle(el).getPropertyValue(prop)
  return { value, rgba: toRgba(value), backdrop, reachedOpaque }
}

const chroma = ([r, g, b]) => Math.max(r, g, b) - Math.min(r, g, b)
/** 赤が支配的か（タリー / destructive の判定）。 */
const isRed = ([r, g, b]) => r > 100 && r - g > 60 && r - b > 60 && Math.abs(g - b) < 60
/** 琥珀か（赤 > 緑 > 青 の順に落ちる暖色）。 */
const isAmber = ([r, g, b]) => r > g && g > b && r - b > 60 && g - b > 20

/** relLum は sRGB バイト列の相対輝度（WCAG 2.x）。 */
function relLum([r, g, b]) {
  const f = (v) => {
    const c = v / 255
    return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)
  }
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
}

/**
 * contrast は 2 色の WCAG コントラスト比。
 *
 * **前景に半透明が来たら合成してから測る。** 背景側は `readColor` の
 * `backdrop`（祖先まで遡って合成した実効面）を渡すこと。要素自身の
 * `background-color` を渡すと、淡い地が親にある構造で恒等になる。
 */
function contrast(fg, bg) {
  const alpha = fg[3] / 255
  const composited = alpha >= 1 ? fg : fg.slice(0, 3).map((c, i) => c * alpha + bg[i] * (1 - alpha))
  const [hi, lo] = [relLum(composited), relLum(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

/** computedOf は指定要素の指定プロパティを `{ value, rgba }` で返す（無ければ null）。 */
async function computedOf(locator, prop) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readColor, prop)
}

// --- 実行 -------------------------------------------------------------------

/** 撮る画面。`wait` はその画面で描画完了と見なせる目印。 */
const screens = [
  { name: 'programs', path: '/', wait: 'li[data-program-id], [data-testid="program-grid-now-line"]' },
  { name: 'reservations', path: '/reservations', wait: 'text=チューナー不足' },
  { name: 'recordings', path: '/recordings', wait: 'text=録画中' },
  { name: 'rules', path: '/rules', wait: 'text=朝ドラ' },
  // 検索は初期状態で結果を持たないので、フォームが立ち上がったことを目印にする
  // （何も待たないと、クエリが解決する前の空のフォームを撮ってしまう）
  { name: 'search', path: '/search', wait: 'text=チャンネル' },
  { name: 'live', path: '/live', wait: 'text=NHK総合' },
]

const viewports = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'mobile', width: 390, height: 844 },
]

const themes = ['light', 'dark']

/** screenOf は名前で `screens` を引く（並び順を変えても判定がずれないように）。 */
function screenOf(name) {
  const found = screens.find((s) => s.name === name)
  if (!found) throw new Error(`画面 ${name} が screens に無い`)
  return found
}

/** desktop は「デスクトップでしか出ない要素」を撮る／判定するときの viewport。 */
const desktop = viewports[0]

rmSync(OUT_DIR, { recursive: true, force: true })
mkdirSync(OUT_DIR, { recursive: true })

const browser = await chromium.launch()

/** open は 1 ページを開いてスタブ・時刻・テーマを整えるところまでやる。 */
async function open(viewport, theme, screen, opts = {}) {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    locale: 'ja-JP',
    timezoneId: 'Asia/Tokyo',
    colorScheme: theme,
    deviceScaleFactor: 2,
  })
  const page = await context.newPage()
  await page.clock.setFixedTime(FIXED_NOW)
  await installApiStubs(page, opts)
  await page.goto(URL_BASE + screen.path, { waitUntil: 'domcontentloaded' })
  // ダークは `.dark` クラスで切り替わる（index.css の @custom-variant）。
  // アプリ自身に切り替え手段が無いので、ここで直接付ける（README §デザイン）。
  if (theme === 'dark') await page.evaluate(() => document.documentElement.classList.add('dark'))
  if (screen.wait) {
    await page.locator(screen.wait).first().waitFor({ timeout: 15000 }).catch(() => {
      ng.push(`${screen.name}/${theme}/${viewport.name}: 目印「${screen.wait}」が出ない`)
    })
  }
  // フォント（Geist Variable / Noto Sans JP Variable）の適用とレイアウト確定を待つ。
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(400)
  return { context, page }
}

log(`URL      : ${URL_BASE}`)
log(`出力先   : ${OUT_DIR}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- ① スクリーンショット ---
log('\n=== ① スクリーンショット ===')
for (const viewport of viewports) {
  for (const theme of themes) {
    for (const screen of screens) {
      const { context, page } = await open(viewport, theme, screen)
      const file = path.join(OUT_DIR, `${screen.name}-${theme}-${viewport.name}.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
      await context.close()
    }
  }
}
// 番組表グリッド（`lg` 以上でしか出ない。現在時刻線・容量超過の帯・ジャンル淡色が
// 一度に並ぶ画面）とブレーカー発動中（destructive の帯）は別途 1 枚ずつ。
for (const theme of themes) {
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) > 0) {
      await grid.first().click()
      await page.waitForTimeout(1200)
      const file = path.join(OUT_DIR, `programs-grid-${theme}-desktop.png`)
      await page.screenshot({ path: file })
      log(`  ${path.basename(file)}`)
    } else {
      log(`  （programs-grid-${theme}: 表示形式の切り替えが出ていないので撮らない）`)
    }
    await context.close()
  }
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'), { withBreaker: true })
    const file = path.join(OUT_DIR, `breaker-${theme}-desktop.png`)
    await page.screenshot({ path: file })
    log(`  ${path.basename(file)}`)
    await context.close()
  }
}

// --- ② 色の機械判定 ---
//
// 判定は「この PR が変えた状態色ぜんぶ」を覆う。1 箇所でも判定の外に置くと、
// そこだけ既定値へ静かに戻っても全部緑のまま通る。
log('\n=== ② 色の判定 ===')

/** 小さい文字（バッジ・ラベル）に要求する WCAG 比。 */
const minTextContrast = 4.5
/** 面・線（非テキスト）に要求する WCAG 比。 */
const minUiContrast = 3

/**
 * 下限を満たさないと分かっていて、いま直さないと決めた組み合わせ。
 *
 * **黙って下限を下げない。** ここに載せたものは合否には数えないが、必ず
 * 「既知の不足」として出力する（CLAUDE.md の「no silent caps」に相当）。
 * 空にできたらこの仕組みごと消す。
 */
const knownGaps = new Map([
  [
    'light/失敗バッジの文字 / destructive の淡い地',
    'destructive は shadcn 既定のまま（この PR の対象外）。' +
      '明度を下げるとタリーレッドと見分けが付かなくなるので、直すなら色相ごと動かす判断が要る' +
      '（実測値は上の表に出る。ここには書かない --- 2 通りの数字を持たないため）',
  ],
])

/** contrasts は測ったコントラストを表として溜める（合否とは別に、数値を人に見せる）。 */
const contrasts = []
function checkContrast(theme, label, fg, measured, floor) {
  // measured は `readColor` の戻り。背景側は必ずその `backdrop` を使う
  if (measured.reachedOpaque === false) {
    ng.push(`[${theme}] ${label}: 不透明な面まで遡れず、比を測れていない`)
    return null
  }
  const bg = measured.backdrop
  const ratio = contrast(fg, bg)
  const gap = knownGaps.get(`${theme}/${label}`)
  contrasts.push({ theme, label, ratio, floor, gap })
  if (ratio < floor && gap === undefined) {
    ng.push(`[${theme}] ${label} のコントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
}

for (const theme of themes) {
  // --- 録画一覧: 録画中 = タリーの塗り / 失敗 = destructive の淡い地 / 地は無彩 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    // 一覧の行の中に限る（状態フィルタのチップにも同じ文言があるため）
    const badge = page.locator('ul span', { hasText: /^録画中$/ })
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(badge, 'color')
    log(`  [${theme}] 録画中バッジ 地=${bg?.value} ${bg?.rgba} / 文字=${fg?.value} ${fg?.rgba}`)
    if (bg === null) ng.push(`[${theme}] 録画中バッジが見つからない`)
    else if (bg.rgba[3] < 200) {
      // 淡い地（destructive の流儀 `bg-*/10`）に戻されたらここで落ちる
      ng.push(`[${theme}] 録画中バッジが塗りでない（不透明度 ${bg.rgba[3]}/255。${bg.value}）`)
    } else if (!isRed(bg.rgba)) {
      ng.push(`[${theme}] 録画中バッジの地がタリーレッドでない（${bg.value} = ${bg.rgba}）`)
    }
    if (fg !== null && chroma(fg.rgba) > 30) {
      ng.push(`[${theme}] 録画中バッジの文字に色が付いている（塗り + 無彩の文字であるべき。${fg.value}）`)
    }
    if (bg !== null && fg !== null && bg.rgba[3] >= 200) {
      // 塗りは不透明なので `bg.rgba` でも同値になるが、**背景側は必ず
      // `fg.backdrop` を渡す**。「どこかは自分の背景、どこかは合成後」と
      // 混ざっていると、次に構造が変わったときにどちらが正しいか分からなくなる
      checkContrast(theme, '録画中バッジの文字 / タリーの塗り', fg.rgba, fg, minTextContrast)
    }

    // 失敗バッジは destructive の「文字 + 淡い地」のまま
    const failed = page.locator('ul span', { hasText: /^失敗$/ })
    const failedBg = await computedOf(failed, 'background-color')
    const failedFg = await computedOf(failed, 'color')
    if (failedFg === null || failedBg === null) {
      ng.push(`[${theme}] 失敗バッジが見つからない`)
    } else {
      if (failedBg.rgba[3] > 200) {
        ng.push(`[${theme}] 失敗バッジが塗りになっている（destructive は文字 + 淡い地。${failedBg.value}）`)
      }
      if (!isRed(failedFg.rgba)) {
        ng.push(`[${theme}] 失敗バッジの文字が赤でない（${failedFg.value} = ${failedFg.rgba}）`)
      }
      checkContrast(
        theme,
        '失敗バッジの文字 / destructive の淡い地',
        failedFg.rgba,
        failedFg,
        minTextContrast,
      )
    }

    // 地は無彩。body だけを見ると「body に bg-background が当たっているか」しか
    // 言えないので、実際に面を持つ要素を回す。
    //
    // **測れなかったら NG にする。** セレクタが 0 件・面が透明のときに黙って
    // continue すると、「4 面を見ている」と書いてあるのに実際は 2 面しか
    // 見ていない、という状態が緑のまま続く（実際そうなっていた）
    for (const [name, selector] of [
      ['body', 'body'],
      ['ヘッダ', 'header'],
      // ナビは 2 つある（モバイルのボトムタブと md 以上のサイドバー）。
      // `.first()` だと背景を持たない方を掴むので両方回す
      ['ナビ', 'nav[aria-label="主ナビゲーション"]'],
      ['一覧の行', 'li a, li > div'],
    ]) {
      const nodes = await page.locator(selector).all()
      const surfaces = []
      for (const node of nodes) {
        const measured = await node.evaluate(readColor, 'background-color')
        surfaces.push(measured.backdrop)
      }
      if (surfaces.length === 0) {
        ng.push(`[${theme}] ${name}（${selector}）が 0 件で、地を判定できていない`)
        continue
      }
      const worst = surfaces.reduce((a, b) => (chroma(a) >= chroma(b) ? a : b))
      log(`  [${theme}] ${name} の地（${surfaces.length} 件中いちばん彩度が高いもの）= ${worst}`)
      if (chroma(worst) > 8) {
        ng.push(`[${theme}] ${name} の地が無彩でない（チャンネル差 ${chroma(worst)}。${worst}）`)
      }
    }
    await context.close()
  }

  // --- 予約一覧: チューナー不足 = 琥珀（淡い地の上で読めるか） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('reservations'))
    // **淡い地を持つのは外側のバッジ、文字を持つのは内側の span。**
    // 内側だけを掴むと背景が透明になり、合成が恒等になって「地の上での比」を
    // 測ってしまう（`^チューナー不足` で引くと、外側は sr-only の文が先頭に来て
    // 一致せず、内側だけが残る）。外側から引いて、文字色は子から採る
    const badge = page.locator('ul span').filter({ hasText: /チューナー不足/ }).first()
    const label = badge.locator('span[aria-hidden="true"]')
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(label, 'color')
    log(`  [${theme}] チューナー不足バッジ 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null || bg === null) {
      ng.push(`[${theme}] チューナー不足バッジが見つからない`)
    } else if (bg.rgba[3] <= 8) {
      // 外側を掴めていない = 合成が効いていない。素通りさせず落とす
      ng.push(`[${theme}] チューナー不足バッジの地が透明（淡い地を持つ要素を掴めていない）`)
    } else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] チューナー不足バッジが琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      // **`backdrop` の遡りが本当に効いているかをここで検査する。** この文字は
      // 「外側バッジの淡い地」の上に乗っており、遡りが 1 段で止まったり
      // 外側を飛ばしたりすると `backdrop` はページの地と一致してしまう。
      // そのとき比は甘い方へ 0.5〜0.7 動くので、一致 = 判定が壊れている
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (sameAsGround) {
        ng.push(
          `[${theme}] 文字が乗る面がページの地と同じ（${fg.backdrop}）` +
            ' --- 淡い地の合成が効いていない',
        )
      }
      checkContrast(theme, 'チューナー不足の文字 / 琥珀の淡い地', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 番組リスト: 放送中の行は色を使わない（太さだけ） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const airing = page.locator('[data-testid="program-row-time"]')
    const count = await airing.count()
    let colored = 0
    for (let i = 0; i < count; i++) {
      const c = await airing.nth(i).evaluate(readColor, 'color')
      if (chroma(c.rgba) > 30) colored++
    }
    log(`  [${theme}] 番組リストの時刻 ${count} 件中、色付き ${colored} 件`)
    if (count === 0) ng.push(`[${theme}] 番組リストの時刻が見つからない`)
    // リストの ON AIR は希少ではない（チャンネル数ぶん同時に点く）ので、
    // タリーにしてはならない。`text-tally` に戻したらここで落ちる
    if (colored > 0) {
      ng.push(`[${theme}] 番組リストの時刻に信号色が付いている（${colored} 件。太さで示す規律）`)
    }
    await context.close()
  }

  // --- 番組表グリッド: 現在時刻の線と札 = タリー / 容量超過の帯 = 琥珀 ---
  // （グリッドは `lg` 以上でしか出ないのでデスクトップのみ）
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) === 0) {
      ng.push(`[${theme}] 表示形式の切り替えが出ていないのでグリッドを判定できない`)
    } else {
      await grid.first().click()
      await page.locator('[data-testid="program-grid-now-line"]').waitFor({ timeout: 10000 })
      await page.waitForTimeout(400)

      const line = await computedOf(page.locator('[data-testid="program-grid-now-line"]'), 'border-top-color')
      log(`  [${theme}] 現在時刻線 = ${line?.value} ${line?.rgba}`)
      if (line === null) ng.push(`[${theme}] 現在時刻線が見つからない`)
      else if (!isRed(line.rgba)) {
        ng.push(`[${theme}] 現在時刻線がタリーレッドでない（${line.value} = ${line.rgba}）`)
      }

      // 現在時刻の札は「塗り」。11px の赤い文字はダークの地で AA に届かないので、
      // タリーは塗りにしか使わない（design.md「タリーは塗り」）
      const chip = page.locator('[data-testid="program-grid-now-label"] span')
      const chipBg = await computedOf(chip, 'background-color')
      const chipFg = await computedOf(chip, 'color')
      log(`  [${theme}] 現在時刻の札 地=${chipBg?.value} ${chipBg?.rgba} / 文字=${chipFg?.rgba}`)
      if (chipBg === null || chipFg === null) ng.push(`[${theme}] 現在時刻の札が見つからない`)
      else {
        if (chipBg.rgba[3] < 200 || !isRed(chipBg.rgba)) {
          ng.push(`[${theme}] 現在時刻の札がタリーの塗りでない（${chipBg.value}）`)
        } else {
          checkContrast(theme, '現在時刻の札の文字 / タリーの塗り', chipFg.rgba, chipFg, minTextContrast)
        }
      }

      // 容量超過の帯。罫線が区間の境界を伝えるので、線が琥珀であることを見る。
      //
      // 帯が重なっているのは番組セル（ジャンル淡色）で、**セルは帯の祖先ではなく
      // 兄弟**なので `backdrop` では拾えない。そこでグリッドのセルを**全件**集めて、
      // 罫線に対していちばん不利な面に対して測る（どのセルと実際に重なっているかは
      // 判定しない --- 全件は安全側の過大集合で、見落とす方向には倒れない）。
      // 帯自身の `backdrop` も候補に入れる: `background-clip` の既定は `border-box`
      // なので、罫線は自分の淡い地の上に描かれる
      const band = page.locator('[data-testid="capacity-band"]')
      const bandBorder = await computedOf(band, 'border-top-color')
      log(`  [${theme}] 容量超過の帯の罫線 = ${bandBorder?.value} ${bandBorder?.rgba}`)
      if (bandBorder === null) ng.push(`[${theme}] 容量超過の帯が見つからない`)
      else {
        if (!isAmber(bandBorder.rgba)) {
          ng.push(`[${theme}] 容量超過の帯の罫線が琥珀でない（${bandBorder.value} = ${bandBorder.rgba}）`)
        }
        const cells = await page.locator('[data-testid="program-grid-cell"]').all()
        const surfaces = [bandBorder.backdrop]
        for (const cell of cells) {
          surfaces.push((await cell.evaluate(readColor, 'background-color')).backdrop)
        }
        // 罫線の色に対していちばんコントラストが低くなる面を選ぶ
        const worst = surfaces.reduce((a, b) =>
          contrast(bandBorder.rgba, a) <= contrast(bandBorder.rgba, b) ? a : b,
        )
        log(`  [${theme}] 帯が重なる面 ${surfaces.length} 種のうち最も不利なもの = ${worst}`)
        checkContrast(theme, '容量超過の帯の罫線 / 最も不利な面', bandBorder.rgba, { backdrop: worst }, minUiContrast)
      }
    }
    await context.close()
  }

  // --- ルール一覧: 「条件なし」の警告 = 琥珀 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('rules'))
    const warn = page.locator('span', { hasText: /^条件なし（すべての番組にマッチ）$/ })
    const fg = await computedOf(warn, 'color')
    log(`  [${theme}] ルールの「条件なし」= ${fg?.value} ${fg?.rgba}`)
    if (fg === null) ng.push(`[${theme}] ルールの「条件なし」警告が見つからない`)
    else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] ルールの「条件なし」が琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      checkContrast(theme, '「条件なし」の文字 / 乗っている面', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }
}

// --- ③ フォントの実描画判定 ---
//
// **`getComputedStyle().fontFamily` は指定した文字列を返すだけで、ブラウザが
// 実際にどのフォントを選んで描画したかは別**（docs/frontend/stack.md「フォント
// は英数字と和文で 2 書体を使い分ける」）。CDP の `CSS.getPlatformFontsForNode`
// で実際に使われたフォントを見る。フォントファイルが unicode-range で分割
// されていて、かつ Noto Sans JP の import が消えても `--font-sans` の
// フォールバック先（システムフォント）が和文をレンダリングできてしまうため、
// **この判定を外すと「Noto Sans JP を削除して和文がシステムフォントに戻る」
// 事故がスクリーンショット上は気付かれないまま緑で通り続ける**。
log('\n=== ③ フォントの判定 ===')

/**
 * platformFontsOf は CDP 経由で selector に一致するノードの実使用フォントを返す。
 *
 * **`cdp` は呼び出し元が `DOM.enable` / `CSS.enable` を送信済みのセッションで
 * あること。** `CSS.enable` を呼ばずに `CSS.getPlatformFontsForNode` を呼ぶと
 * **空配列ではなく protocol error で throw する**
 * （`Protocol error (CSS.getPlatformFontsForNode): CSS agent was not enabled`。
 * 実測で確認済み）。セッションを作るタイミングはナビゲーションの前後どちらでも
 * 結果は変わらない（両方実測済み。以前このファイルに「ナビゲーション後に
 * セッションを作ると空配列が返る」という誤った記述があったが、それは次の罠を
 * 誤って帰属したものだった）。
 *
 * **本当の罠は selector の選び方。** `main` や `body` のような「直接はテキストを
 * 持たずブロック要素だけを子に持つ」要素を渡すと常に空配列が返る（throw ではない）。
 * `CSS.getPlatformFontsForNode` はノード自身のインラインレイアウト（実際に
 * テキストランを持つ層）に紐付いたフォント使用だけを返し、ブロックの子孫を
 * 再帰集約しない。実際にテキストを直接持つ要素（番組リストの行
 * `li[data-program-id]` 等）を渡す必要がある。
 */
async function platformFontsOf(cdp, selector) {
  const { root } = await cdp.send('DOM.getDocument')
  const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: root.nodeId, selector })
  if (!nodeId) return null
  const { fonts } = await cdp.send('CSS.getPlatformFontsForNode', { nodeId })
  return fonts.map((f) => `${f.familyName} x${f.glyphCount}`)
}

{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  // このブロックでしか CDP を使わないので、ここでセッションを作って有効化する
  // （`open()` は全画面 × テーマ × ビューポートで呼ばれるので、そちらに置くと
  // 使わない呼び出しでも毎回セッションを作ることになる）。
  const cdp = await context.newCDPSession(page)
  await cdp.send('DOM.enable')
  await cdp.send('CSS.enable')

  // 番組リストの行は時刻（Geist が担当）と番組名（Noto Sans JP が担当）を
  // 同じ行に持つので、1 要素で両方の実使用フォントが確認できる
  const fonts = await platformFontsOf(cdp, 'li[data-program-id]')
  log(`  実使用フォント（番組リストの行） = ${fonts?.join(', ') ?? '(取れず)'}`)
  if (fonts === null) {
    ng.push('フォント判定: li[data-program-id] が見つからない')
  } else {
    if (!fonts.some((f) => f.includes('Noto Sans JP'))) {
      ng.push(`和文が Noto Sans JP で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    if (!fonts.some((f) => f.includes('Geist'))) {
      ng.push(`英数字が Geist で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    // 「1 グリフだけ Noto Sans JP、残りはシステムフォント」という部分的な退行は
    // 上の `some` だけでは検出できない（1 件でも Noto があれば真になる）。
    // 和文システムフォント（--font-sans のフォールバック候補）が実使用に
    // 一切現れないことも見て、検出力を上げる。
    const systemJpFonts = fonts.filter((f) => /Hiragino|Yu Gothic|Meiryo/.test(f))
    if (systemJpFonts.length > 0) {
      ng.push(`和文の一部がシステムフォントに落ちている（実使用: ${fonts.join(', ')}）`)
    }
  }

  // tabular-nums が和文まじりの文字列でも実際に等幅を作っているか。
  // canvas 2D の `font` ショートハンドには font-variant-numeric を渡せないので、
  // 実要素を DOM に挿して getBoundingClientRect で幅を測る（実描画の幅そのもの）。
  // `normal` 側も測って、判定が「たまたま両方同じ幅」ではなく tabular-nums の
  // 効果そのものを見ていることを確認する。
  const widths = await page.evaluate(() => {
    function width(text, variant) {
      const el = document.createElement('span')
      el.style.position = 'absolute'
      el.style.visibility = 'hidden'
      el.style.whiteSpace = 'pre'
      el.style.fontVariantNumeric = variant
      el.textContent = text
      document.body.appendChild(el)
      const w = el.getBoundingClientRect().width
      el.remove()
      return w
    }
    return {
      tabularA: width('第11話', 'tabular-nums'),
      tabularB: width('第88話', 'tabular-nums'),
      normalA: width('第11話', 'normal'),
      normalB: width('第88話', 'normal'),
    }
  })
  log(
    `  第11話/第88話 幅（tabular-nums） = ${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}`,
  )
  log(
    `  第11話/第88話 幅（normal）       = ${widths.normalA.toFixed(2)} / ${widths.normalB.toFixed(2)}`,
  )
  if (Math.abs(widths.tabularA - widths.tabularB) > 0.5) {
    ng.push(
      `tabular-nums が和文まじりの文字列で等幅を作っていない（${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}）`,
    )
  }
  if (Math.abs(widths.normalA - widths.normalB) < 0.5) {
    ng.push(
      'tabular-nums 無指定でも同じ幅になっている（この判定が tabular-nums の効果を検出できていない）',
    )
  }

  await context.close()
}

// 数値は docs に転記しない（転記した瞬間に二重管理になる）。docs は
// 「ここで測る」とだけ言い、実際の数値はこの出力が権威。
log('\n=== 測ったコントラスト ===')
for (const { theme, label, ratio, floor, gap } of contrasts) {
  const mark = ratio >= floor ? ' ' : gap !== undefined ? '△' : '×'
  log(`  ${mark} [${theme}] ${label}: ${ratio.toFixed(2)}（下限 ${floor}）`)
}
const gaps = contrasts.filter((c) => c.ratio < c.floor && c.gap !== undefined)
if (gaps.length > 0) {
  log('\n  既知の不足（合否には数えていない）:')
  for (const { theme, label, gap } of gaps) log(`    △ [${theme}] ${label} --- ${gap}`)
}
// 下限を満たすようになった gap は畳めるので、そのことも言う。
// 言わないと knownGaps が「一度入れたら誰も見ない置き場」になる
const stale = contrasts.filter((c) => c.ratio >= c.floor && c.gap !== undefined)
for (const { theme, label, ratio } of stale) {
  log(`\n  knownGaps に載っているが下限を満たしている（${ratio.toFixed(2)}）: [${theme}] ${label}`)
  log('    → knownGaps から消せる')
}

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

await browser.close()
process.exit(ng.length === 0 ? 0 : 1)
