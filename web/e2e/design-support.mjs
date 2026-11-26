// デザイン（色）の受け入れ判定とスクリーンショット。**色は jsdom で測れない**ので、
// デザイン判定が視覚的契約の唯一のオラクルになる（e2e/README.md、docs/frontend/design.md）。
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` でブラウザ側から
// 丸ごと差し替える（e2e/live.mjs が HLS でやっているのと同じ手）。サーバーには
// SPA（index.html + dist の資産）を返す仕事しか残らないので、
// `pnpm preview` でも rokuban 本体でもよい。
//
//   pnpm build && pnpm preview --port 4173 &
//   E2E_URL=http://localhost:4173 node e2e/run.mjs design-screenshots
//
// 合格なら exit 0、1 つでも NG なら exit 1。判定の詳細は e2e/README.md §デザイン。
import { mkdirSync, rmSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { apiHandler, FIXED_NOW } from './design-fixtures.mjs'
import { installApiStubs } from './lib.mjs'

export const URL_BASE = process.env.E2E_URL ?? 'http://localhost:40773'
const SHOT_ROOT =
  process.env.E2E_SHOT_DIR ??
  path.join(path.dirname(fileURLToPath(import.meta.url)), 'screenshots')

/** prepareScreenshotDir は判定スクリプト専用の出力先だけを作り直す。 */
export function prepareScreenshotDir(name) {
  const OUT_DIR = path.join(SHOT_ROOT, name)
  rmSync(OUT_DIR, { recursive: true, force: true })
  mkdirSync(OUT_DIR, { recursive: true })
  return OUT_DIR
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
export const readColor = (el, input) => {
  const { prop, pseudo = null } = typeof input === 'string' ? { prop: input } : input
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

  const value = getComputedStyle(el, pseudo).getPropertyValue(prop)
  return { value, rgba: toRgba(value), backdrop, reachedOpaque }
}

export const chroma = ([r, g, b]) => Math.max(r, g, b) - Math.min(r, g, b)
/**
 * oklchChroma は `getComputedStyle()` が返す `oklch(L C H)` 文字列から C を取り出す。
 *
 * **中間の明度では `chroma()`（RGB のチャンネル差）が過大に出る。** oklch の
 * 色域は明度が両端（白 / 黒）に寄るほど圧縮されるので、同じ oklch chroma でも
 * 中間の明度（`--tone-400` など）は白・墨に近い明度（`--paper` / `--sumi`）より
 * 大きい RGB チャンネル差になる。3 値の無彩性は design-tokens.test.ts と同じ
 * 基準（oklch chroma <= 0.02）で測る --- RGB 側の閾値を緩めると、こちらの
 * 都合で 3 値本体の判定基準まで緩んでしまう
 */
export function oklchChroma(value) {
  // L は `0.705` のような比率でも `98.5%` のようなパーセントでも来る
  // （`--scan-lit` はカスタムプロパティ経由で読むため、標準プロパティより
  // 表記ゆれが出やすい）。どちらも通す
  const m = /oklch\(\s*[\d.]+%?\s+([\d.]+)/.exec(value ?? '')
  return m === null ? null : Number(m[1])
}
/** 赤が支配的か（タリー / destructive の判定）。 */
export const isRed = ([r, g, b]) => r > 100 && r - g > 60 && r - b > 60 && Math.abs(g - b) < 60
/** 琥珀か（赤 > 緑 > 青 の順に落ちる暖色）。 */
export const isAmber = ([r, g, b]) => r > g && g > b && r - b > 60 && g - b > 20

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
export function contrast(fg, bg) {
  const alpha = fg[3] / 255
  const composited = alpha >= 1 ? fg : fg.slice(0, 3).map((c, i) => c * alpha + bg[i] * (1 - alpha))
  const [hi, lo] = [relLum(composited), relLum(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

/** computedOf は指定要素の指定プロパティを `{ value, rgba }` で返す（無ければ null）。 */
export async function computedOf(locator, prop) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readColor, prop)
}

/** computedPseudoOf は指定要素の疑似要素から色を読む（無ければ null）。 */
export async function computedPseudoOf(locator, prop, pseudo) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readColor, { prop, pseudo })
}

/**
 * readCustomColor はカスタムプロパティ（`--scan-gap` / `--scan-lit`）の計算値を
 * 読む。`readColor`（標準プロパティ用）と違って祖先を遡る backdrop 合成はしない
 * --- 縞の 2 色それぞれの値そのものを見るためのもので、透過は想定しない。
 *
 * **これが無いと `background-image` に直接書いた縞の色は一切読めない。**
 * `getComputedStyle().backgroundColor` は `background-color`（= 縞の片側）
 * しか返さないので、もう片側（グラデーションの中の色）を測る手段が無いと
 * 「輝線を文字と同じ色にする」変異が判定をすり抜ける（design.md の失敗事例
 * 「判定を足したことと、それが効いていることは別」と同じ形の穴になる）。
 * `index.css` の `.scanlines` / `.tally-scanlines` は縞の両方の色を
 * `--scan-gap` / `--scan-lit` というカスタムプロパティに出し、
 * `background-color` と `background-image` の両方がそれを `var()` で参照する
 * ようにしてあるので、ここから両方読める
 */
const readCustomColor = (el, varName) => {
  const canvas = document.createElement('canvas')
  canvas.width = 1
  canvas.height = 1
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const value = getComputedStyle(el).getPropertyValue(varName).trim()
  ctx.clearRect(0, 0, 1, 1)
  ctx.fillStyle = value
  ctx.fillRect(0, 0, 1, 1)
  const d = ctx.getImageData(0, 0, 1, 1).data
  return { value, rgba: [d[0], d[1], d[2], d[3]] }
}

/** computedVar は指定要素のカスタムプロパティを `{ value, rgba }` で返す（無ければ null）。 */
export async function computedVar(locator, varName) {
  if ((await locator.count()) === 0) return null
  return locator.first().evaluate(readCustomColor, varName)
}

/** readScreenshotPixels は Chromium が描いた PNG から指定座標の実画素を読む。 */
export async function readScreenshotPixels(page, png, points) {
  return page.evaluate(
    async ({ base64, points }) => {
      const image = new Image()
      image.src = `data:image/png;base64,${base64}`
      await image.decode()
      const canvas = document.createElement('canvas')
      canvas.width = image.width
      canvas.height = image.height
      const context = canvas.getContext('2d', { willReadFrequently: true })
      if (context === null) throw new Error('2d canvas context is unavailable')
      context.drawImage(image, 0, 0)
      const pixels = context.getImageData(0, 0, canvas.width, canvas.height).data
      return points.map(([x, y]) => {
        const px = Math.floor(x)
        const py = Math.floor(y)
        if (px < 0 || py < 0 || px >= canvas.width || py >= canvas.height) {
          throw new Error(`screenshot point is outside the image: (${px}, ${py})`)
        }
        const start = (py * canvas.width + px) * 4
        return Array.from(pixels.slice(start, start + 3))
      })
    },
    { base64: png.toString('base64'), points },
  )
}

/** sameRgb は alpha を除く実画素 3 値が一致するかを見る。 */
export function sameRgb(a, b) {
  return a.slice(0, 3).every((value, index) => value === b[index])
}

// --- 実行 -------------------------------------------------------------------

/** 撮る画面。`wait` はその画面で描画完了と見なせる目印。 */
export const screens = [
  // ホームの全画面ショットは管理 mode を明示。既定「見る」は専用判定で `/` のまま確認する。
  { name: 'home', path: '/?mode=ops', wait: 'text=明日の終わり' },
  { name: 'programs', path: '/programs', wait: 'li[data-program-id], [data-testid="program-grid-now-line"]' },
  { name: 'reservations', path: '/reservations', wait: 'text=容量不足' },
  { name: 'recordings', path: '/recordings', wait: 'text=録画中' },
  { name: 'rules', path: '/rules', wait: 'text=朝ドラ' },
  { name: 'series', path: '/series', wait: 'text=作品X' },
  // 検索は初期状態で結果を持たないので、常時表示のフォーム見出しを目印にする
  // （詳細条件は初期状態で折りたたまれており、「チャンネル」は待機目印にならない）
  { name: 'search', path: '/search', wait: 'text=テキスト条件' },
  { name: 'live', path: '/live', wait: 'text=NHK総合' },
  { name: 'series-hub', path: '/recordings/12/series', wait: 'text=エピソード' },
]

/**
 * recordingDetailScreen は録画単体ページ（`/recordings/$id`）。`screens` には
 * 足さない --- 足すと① の全画面ショットが 1 画面ぶん増える。判定に必要なのは
 * path と目印だけなので、この 1 つを複数箇所（②の色判定・withBreaker の
 * ショット）から共有する。
 */
export const recordingDetailScreen = { name: 'recording-detail', path: '/recordings/12', wait: '[data-testid="recording-detail-tabs"]' }

export const viewports = [
  // 一覧の行長上限は広幅で初めて効くので、デスクトップショットは 2560px で撮る。
  { name: 'desktop', width: 2560, height: 1440 },
  { name: 'mobile', width: 360, height: 844 },
]

export const themes = ['light', 'dark']

/** screenOf は名前で `screens` を引く（並び順を変えても判定がずれないように）。 */
export function screenOf(name) {
  const found = screens.find((s) => s.name === name)
  if (!found) throw new Error(`画面 ${name} が screens に無い`)
  return found
}

/** desktop は「デスクトップでしか出ない要素」を撮る／判定するときの viewport。 */
export const desktop = viewports[0]
/** mobile は「モバイルでしか出ない要素」を撮る／判定するときの viewport。 */
export const mobile = viewports[1]
/**
 * mobileWide は issue #686 の到達距離判定専用（iPhone 標準幅 390px）。
 * 全画面スクリーンショットのループ（① / ②）には加えない --- 360px との
 * フルショット差分の価値が低く、7 画面 × 2 テーマの 2 周を 1.5 倍に増やす
 * だけになる（レビュー指摘）。
 */
export const mobileWide = { name: 'mobile-wide', width: 390, height: 844 }
/** issue #1020 の主役幅（本文幅とのサムネイル比を測る）。 */
export const homeDesktop = { name: 'home-desktop', width: 1280, height: 800 }
/** 番組ハブ配置判定専用。受け入れ条件の 400px 幅をそのまま測る。 */
export const seriesHubMobile = { name: 'series-hub-400', width: 400, height: 844 }
/** 同じ判定をデスクトップ幅でも回す。 */
export const seriesHubDesktop = { ...desktop, name: 'series-hub-desktop' }

export const homeWatchScreen = {
  name: 'home-watch',
  path: '/?mode=watch',
  wait: '[data-testid="home-primary-action"]',
}

/**
 * settleAfterOpen は、開いた直後のフォント（Geist Variable / Noto Sans JP Variable）の
 * 適用と、有限のアニメーション（入場の遷移など）の終わりを待つ。以前は 400 ms の固定待ちだった。
 * 無限のアニメーション（animate-pulse など）は finished が解決しないので対象から外す。
 * 終わらない有限のアニメーションに当たっても上限 3000 ms で打ち切る。後段の判定は
 * 描画された状態を測るので、打ち切りは NG にしない。
 */
async function settleAfterOpen(page) {
  await page.evaluate(async () => {
    await document.fonts.ready
    const finite = document.getAnimations().filter((a) => a.effect?.getComputedTiming().endTime !== Infinity)
    const settled = Promise.all(finite.map((a) => a.finished.catch(() => {})))
    await Promise.race([settled, new Promise((resolve) => setTimeout(resolve, 3000))])
    // データの到着で後から描かれる分を待つ。DOM が 150 ms 変わらなければ静かとみなす（上限 3000 ms）。
    await new Promise((resolve) => {
      let quiet
      const done = () => {
        observer.disconnect()
        clearTimeout(cap)
        clearTimeout(quiet)
        resolve()
      }
      const observer = new MutationObserver(() => {
        clearTimeout(quiet)
        quiet = setTimeout(done, 150)
      })
      const cap = setTimeout(done, 3000)
      observer.observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true })
      quiet = setTimeout(done, 150)
    })
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)))
  })
}

/** createOpen は判定スクリプトごとに色設定の変更確認を一度だけ行う open を作る。 */
export function createOpen(browser, ng) {
  let checkedColorSchemeChange = false

  /** open は 1 ページを開いてスタブ・時刻・テーマを整えるところまでやる。 */
  return async function open(viewport, theme, screen, opts = {}) {
    const {
      pointer = 'fine',
      recordingView = null,
      reservationGrouping = null,
      isMobile = pointer === 'coarse',
      installClock = false,
      ...apiOpts
    } = opts
    if (pointer !== 'fine' && pointer !== 'coarse') {
      throw new Error(`未対応のポインタプロファイル: ${pointer}`)
    }
    const context = await browser.newContext({
      viewport: { width: viewport.width, height: viewport.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
      hasTouch: pointer === 'coarse',
      isMobile,
    })
    if (recordingView !== null) {
      await context.addInitScript((view) => localStorage.setItem('rokuban:recordings:view', view), recordingView)
    }
    if (reservationGrouping !== null) {
      await context.addInitScript((value) => localStorage.setItem('rokuban:reservations:group', value), reservationGrouping)
    }
    const page = await context.newPage()
    if (installClock) {
      await page.clock.install({ time: FIXED_NOW })
    } else {
      await page.clock.setFixedTime(FIXED_NOW)
    }
    await installApiStubs(page, apiHandler(apiOpts))
    await page.goto(URL_BASE + screen.path, { waitUntil: 'domcontentloaded' })
    // ダークは `.dark` クラスで切り替わる（index.css の @custom-variant）。
    // アプリ自身が `prefers-color-scheme` を初回描画前に `html.dark` へ反映する
    // （index.html の inline script）。ここで直接付けず、context の colorScheme が
    // 起こした到達経路そのものを判定に載せる --- inline script を壊すと全ダーク
    // ショットがここで落ちる。
    const hasDark = await page.evaluate(() => document.documentElement.classList.contains('dark'))
    if ((theme === 'dark') !== hasDark) {
      ng.push(
        `${screen.name}/${theme}/${viewport.name}: html.dark が prefers-color-scheme=${theme} に追従していない（到達経路が壊れている）`,
      )
    }
    if (!checkedColorSchemeChange) {
      const opposite = theme === 'dark' ? 'light' : 'dark'
      await page.emulateMedia({ colorScheme: opposite })
      await page
        .waitForFunction(
          (dark) => document.documentElement.classList.contains('dark') === dark,
          opposite === 'dark',
          { timeout: 1500 },
        )
        .catch(() => ng.push(`prefers-color-scheme の ${theme} → ${opposite} 変更に html.dark が追従しない`))
      await page.emulateMedia({ colorScheme: theme })
      await page
        .waitForFunction(
          (dark) => document.documentElement.classList.contains('dark') === dark,
          theme === 'dark',
          { timeout: 1500 },
        )
        .catch(() => ng.push(`prefers-color-scheme の ${opposite} → ${theme} 変更に html.dark が追従しない`))
      checkedColorSchemeChange = true
    }
    if (screen.wait) {
      await page.locator(screen.wait).first().waitFor({ timeout: 15000 }).catch(() => {
        ng.push(`${screen.name}/${theme}/${viewport.name}: 目印「${screen.wait}」が出ない`)
      })
    }
    await settleAfterOpen(page)
    return { context, page }
  }
}

/**
 * MISSING_STRING_PATTERN は「唯一の視覚オラクル」が欠損データのまま撮れて
 * いないかを見る（issue #468）。`undefined` / `NaN` はレンダーの欠損値が
 * そのまま文字列化されたときに出る典型で、`[object` はオブジェクトを
 * 文字列テンプレートに直接埋め込んだときに出る（`[object Object]` 等）。
 *
 * **`null` は対象にしない。** 番組名・ルール名に偶然「null」を含む文字列が
 * 来ても単語境界だけでは区別できず、実際に偽陽性になりうる（README §デザイン
 * 「判定を足すときの規律」参照）。`undefined` / `NaN` は単語境界
 * （`\b`）で、`[object` は `[` の前が単語文字になり得ない（直前は空白か
 * 文字列先頭）ため前方一致で見る。
 */
const MISSING_STRING_PATTERN = /\b(undefined|NaN)\b|\[object\b/

/**
 * checkMissingStrings は `page.textContent('body')` に欠損文字列が
 * 混ざっていないかを見る。安い判定なので全画面に掛ける
 * （ルールの `textMatches` から `target` が抜けると
 * `rule-condition-summary.ts` の `textTargetSummaryLabels[m.target]` が
 * `undefined` を返し、「undefinedに「連続テレビ小説」を含む」がそのまま
 * ルール一覧に描かれる --- これが issue #468 で実際に見逃されていた壊れ方）。
 */
export async function checkMissingStrings(ng, page, label) {
  const text = await page.textContent('body').catch(() => null)
  if (text === null) {
    ng.push(`${label}: body のテキストが取得できず欠損文字列を判定できていない`)
    return
  }
  const found = MISSING_STRING_PATTERN.exec(text)
  if (found) {
    ng.push(`${label}: 画面に欠損文字列「${found[0]}」が混ざっている`)
  }
}

