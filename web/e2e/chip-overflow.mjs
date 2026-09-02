// 長い局名 + 補助ラベルを載せたサービスチップが、狭い画面でページ全体を横スクロール
// させないことの受け入れ判定（issue #306）。レジストリを 2 サイトにして、長い
// site 名の「サイト」チップ（issue #531）も同じ `Chip` を使うことを合わせて見る。
//
// jsdom（`pnpm test`）はレイアウトを計算しないので `scrollWidth` / `clientWidth` は
// 常に 0 で、「はみ出している」は原理的に測れない。`Chip` は flex 直下に置かれる
// 共有プリミティブで、`shrink-0` を持つため flex-basis が内容の最大幅を要求する ---
// 名前だけのときは収まっていたチップに補助ラベルを足すと、そのぶん最大幅が伸びて
// ページ全体が横に伸びる。ここがその唯一の判定手段。
//
// 合格なら exit 0、1 つでも NG なら exit 1。
//
// 使い方（Go サーバーも Postgres も要らない。API は全部スタブする）:
//
//   cd web && pnpm build && pnpm exec vite preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 pnpm e2e:chip-overflow
import { finish, launchBrowser, log, sseKeepAlive, verifyBundleMatchesOrExit } from './lib.mjs'

const BASE = process.env.E2E_URL ?? 'http://localhost:4173'
/** 判定するビューポート幅。実機で最も狭い層（iPhone SE = 320px）に合わせる。 */
const width = 320

const ng = []
/** ok は真偽の判定を 1 件記録する。落ちても続行して全部の NG を出す。 */
const ok = (label, pass, detail) => {
  if (pass) log(`OK  ${label}${detail === undefined ? '' : `: ${detail}`}`)
  else {
    log(`NG  ${label}${detail === undefined ? '' : `: ${detail}`}`)
    ng.push(label)
  }
}

// ⓪ 配っている bundle が dist/ の現物と一致するか（e2e/lib.mjs 参照）。
await verifyBundleMatchesOrExit(BASE, ng)

/**
 * 同じ長い名前を持つ 2 件。名前が重複するので `serviceDisambiguator` が補助ラベル
 * （`地上波 5 ・ 27` / `地上波 5 ・ 95`）を付ける = 判定したい「名前 + ラベル」の状態。
 */
const longName = '瀬戸内海放送デジタルテレビジョン臨時サブチャンネル'

/**
 * レジストリを 2 サイトにする（issue #531: `<ConditionFields>` のサイトチップは
 * レジストリと下書きの和集合が 2 つ以上のときだけ描画するため、単一サイトかつ
 * 下書きが空のスタブでは判定対象自体が存在しない）。片方は長い site 名にして、`Chip` が横幅を広げないことを
 * サービスチップと同じ 320px で確認する。
 */
const longSiteName = 'とても長いサイト名のダミーマイラック録画拠点識別子テスト用'
const siteNames = ['tokyo', longSiteName]
const services = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: longName,
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 5,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3273601025,
    networkId: 32736,
    serviceId: 1025,
    name: longName,
    channelType: 'GR',
    channel: '95',
    remoteControlKeyId: 5,
    hasLogoData: false,
    hasPrograms: true,
  },
]

const browser = await launchBrowser()

/** openStubbed は `/api/**` を丸ごと差し替えたページを狭いビューポートで開く。 */
async function openStubbed(pathname, label) {
  const p = await browser.newPage({ viewport: { width, height: 640 } })
  p.on('pageerror', (e) => {
    log(`NG  ページ例外（${label}）:`, e.message)
    ng.push(`pageerror（${label}）`)
  })
  await p.route('**/api/**', async (route) => {
    const requested = new URL(route.request().url()).pathname
    if (requested === '/api/events') return sseKeepAlive(route)
    const body =
      requested === '/api/capabilities'
        ? '{"encode":false,"live":false,"storage":false}'
        : requested === '/api/version'
          ? '{"version":"e2e"}'
          : requested === '/api/sites'
            ? JSON.stringify(siteNames)
            : /\/services$/.test(requested)
              ? JSON.stringify(services)
              : '[]'
    await route.fulfill({ status: 200, headers: { 'content-type': 'application/json' }, body })
  })
  await p.clock.setFixedTime(new Date('2026-08-14T12:00:00+09:00'))
  await p.goto(BASE + pathname, { waitUntil: 'networkidle' })
  return p
}

/** documentScroll はページ全体の横スクロール量（>0 なら横スクロールが出ている）。 */
const documentScroll = (p) =>
  p.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
  }))

// ① 検索の条件フォーム: 長い局名 + 補助ラベルのチップがあってもページ全体が
//    横スクロールしない。`Chip` から `max-w-full` を外すと落ちる（測定値は README）。
log(`\n=== ① /search（${width}px・長い局名 + 補助ラベル）===`)
const searchPage = await openStubbed('/search', '検索')
const chips = searchPage.locator('div[role="group"][aria-label="チャンネル"] button')
await chips.first().waitFor()
const chipCount = await chips.count()
ok('① チップが 2 件描かれている', chipCount === 2, `${chipCount} 件`)

const chipText = await chips.first().textContent()
ok(
  '① 補助ラベルが付いている（測る対象が「名前 + ラベル」であること）',
  chipText === `${longName}（地上波 5 ・ 27）`,
  JSON.stringify(chipText),
)

// サイトチップ（issue #531）。レジストリを 2 サイトにしたことで
// `<ConditionFields>` に「サイト」の節が出る。長い site 名でも同じ `Chip`
// （`max-w-full`）を使うので、①のサービスチップと同じ判定を通す。
const siteGroup = searchPage.locator('div[role="group"][aria-label="サイト"]')
await siteGroup.waitFor()
const siteChips = siteGroup.locator('button')
ok('① サイトのチップが 2 件描かれている', (await siteChips.count()) === 2, `${await siteChips.count()} 件`)

const doc = await documentScroll(searchPage)
ok(
  '① ページ全体が横スクロールしない',
  doc.scrollWidth <= doc.clientWidth,
  `scrollWidth ${doc.scrollWidth} / clientWidth ${doc.clientWidth}`,
)

// ② はみ出しを「チップの中で折り返す」ことで解いていること（内容を切り落として
//    いない）。チップの箱がビューポートに収まり、かつ箱の中で内容もあふれていない。
const box = await chips.first().evaluate((el) => {
  const r = el.getBoundingClientRect()
  const cs = getComputedStyle(el)
  const lineHeight = parseFloat(cs.lineHeight)
  const inner = el.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
  return {
    right: r.right,
    width: r.width,
    scrollWidth: el.scrollWidth,
    clientWidth: el.clientWidth,
    lines: Math.round(inner / lineHeight),
  }
})
ok(
  '② チップの箱がビューポートに収まる',
  box.right <= width,
  `right ${box.right.toFixed(1)} / viewport ${width}`,
)
ok(
  '② チップの中で内容があふれていない（切り落としでも隠しでもない）',
  box.scrollWidth <= box.clientWidth,
  `scrollWidth ${box.scrollWidth} / clientWidth ${box.clientWidth}`,
)
ok('② チップ自身が 2 行以上に折り返している', box.lines >= 2, `${box.lines} 行`)

// ③ `Chip` は共有プリミティブなので、録画一覧の絞り込みにある短いピル
//    （状態・種別）にも同じクラスが波及する。丸ピルの中で折り返らないこと ---
//    ここは①の対策が他の画面の見た目を変えていないことの逆方向の判定。
log(`\n=== ③ /recordings の絞り込み（${width}px・短いピル）===`)
const recordingsPage = await openStubbed('/recordings', '録画一覧')
await recordingsPage.getByRole('button', { name: '絞り込み' }).click()
const popup = recordingsPage.getByRole('dialog', { name: '絞り込み' })
await popup.waitFor()

for (const groupName of ['状態', '種別', 'ジャンル']) {
  const pills = popup.locator(`div[role="group"][aria-label="${groupName}"] button`)
  const n = await pills.count()
  const lines = await pills.evaluateAll((els) =>
    els.map((el) => {
      const cs = getComputedStyle(el)
      const inner = el.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
      return Math.round(inner / parseFloat(cs.lineHeight))
    }),
  )
  ok(`③ ${groupName} のピルがある`, n > 0, `${n} 件`)
  ok(
    `③ ${groupName} のピルが全部 1 行`,
    lines.length > 0 && lines.every((l) => l === 1),
    `行数 ${JSON.stringify(lines)}`,
  )
}

const docRecordings = await documentScroll(recordingsPage)
ok(
  '③ 絞り込みを開いてもページ全体が横スクロールしない',
  docRecordings.scrollWidth <= docRecordings.clientWidth,
  `scrollWidth ${docRecordings.scrollWidth} / clientWidth ${docRecordings.clientWidth}`,
)

const popupBox = await popup.evaluate((el) => {
  const r = el.getBoundingClientRect()
  return { left: r.left, right: r.right }
})
ok(
  '③ ポップオーバーがビューポートに収まる',
  popupBox.left >= 0 && popupBox.right <= width,
  `left ${popupBox.left.toFixed(1)} / right ${popupBox.right.toFixed(1)} / viewport ${width}`,
)

await finish(ng, browser)
