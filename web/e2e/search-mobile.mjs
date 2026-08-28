// /search の主操作（検索）がモバイル初画面に届くことの受け入れ判定（issue #305）。
//
// jsdom は `getBoundingClientRect()` が常に 0 を返し、レイアウト・可視性・
// スクロール位置を測れない（web/e2e/README.md「jsdom が測れないもの」）。
// 「390px 幅の初画面でボタンがボトムタブに隠れない」「テキスト条件の入力が
// サービスチップより先に届く」はどちらも実レイアウトの上下関係そのものなので、
// ここでしか判定できない。
//
// 見るのは:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか
//      （badge-links.mjs / sse-refresh.mjs と同じ理由。web/e2e/README.md）
//   ① 390px 幅で「検索」ボタンの矩形がビューポート内に収まり、モバイルの
//      ボトムタブ（`nav[aria-label="主ナビゲーション"].fixed`）と重なっていないこと
//   ② テキスト条件 1 行目の値入力（`条件を追加` を押さなくても出ている。
//      issue #305 の対処本体）が、初画面で「条件を追加」を押さずに直接
//      見えており、サービスのチップ列より前（画面の上）にあること
//   ③ 1280px（デスクトップ）でも同じ点を確認する --- issue 本文は
//      「デスクトップではボタンは見えるが、キーワードは同じく一段奥」と
//      言っており、②はデスクトップでも直す対象
//   ④ 「検索」を押した後、その結果（件数行・結果 1 件目）が折り目の中に
//      見えること。①②だけだと「押しても画面が変わらず、結果を見るために
//      下までスクロールする」状態が緑で通る（レビューで実測）
//
// **①②は `page.goto` 直後、スクロールも操作も一切せずに測る** --- 「初画面」を
// 検証する判定でスクロールしてしまうと、直したい問題自体を回避してしまう。
// ④だけが「押した後」の判定なので、①②を測り終えてから操作する。
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` で丸ごと
// 差し替える（design.mjs と同じ手）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/search-mobile.mjs
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium } from 'playwright'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'

const ng = []
const log = (...a) => console.log(...a)

const services = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3273701032,
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
    hasPrograms: true,
  },
]

/**
 * matchedProgramIds は検索スタブが返す programId の集合（④で使う）。
 *
 * 検索 API（`POST /api/sites/{site}/programs/search`）は programId の配列だけを
 * 返し、画面は 1 件ごとに `GET /api/sites/{site}/programs/{id}` を叩く（実物と
 * 同じ形）。
 *
 * **件数を 20 件にしているのは、結果がスクロールの余地を作るため。** 数件だと
 * 結果の先頭へ寄せる操作がドキュメント末尾で頭打ちになり、`scroll-margin-top`
 * （`sticky` なページヘッダの下に潜らせないための余白）を落としても④が通って
 * しまう。20 件なら頭打ちにならないので、その分の判定が生きる。
 */
const matchedProgramIds = Array.from({ length: 20 }, (_, i) => 3273610240001 + i)

/** programDetail は `GET /api/sites/{site}/programs/{id}` の応答。 */
function programDetail(id, index) {
  const startAt = new Date(Date.UTC(2026, 7, 20, 12 + index, 0, 0)).toISOString()
  const endAt = new Date(Date.UTC(2026, 7, 20, 12 + index, 30, 0)).toISOString()
  return {
    programId: id,
    networkId: 32736,
    serviceId: 1024,
    eventId: 1 + index,
    startAt,
    endAt,
    durationMs: 30 * 60 * 1000,
    name: `ニュース ${index + 1}`,
    description: '',
    genres: [0],
    isFree: true,
  }
}

/** installApiStubs は /search の描画に要る `/api/**` を丸ごと差し替える。 */
async function installApiStubs(page) {
  await page.route('**/api/**', async (route) => {
    const p = new URL(route.request().url()).pathname
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

    if (p === '/api/events') return route.fulfill({ status: 204 })
    if (p === '/api/sites') return json([SITE])
    if (p === '/api/capabilities') return json({ live: false })
    if (p === `/api/sites/${SITE}/services`) return json(services)
    // `/programs/search` は `/programs/{id}` より先に見る（`search` が id に
    // 見えてしまう順序事故を避ける）。
    if (p === `/api/sites/${SITE}/programs/search`) return json(matchedProgramIds)
    const detail = /^\/api\/sites\/[^/]+\/programs\/(\d+)$/.exec(p)
    if (detail !== null) {
      const id = Number(detail[1])
      const index = matchedProgramIds.indexOf(id)
      if (index < 0) {
        return route.fulfill({ status: 404, contentType: 'application/json', body: '{}' })
      }
      return json(programDetail(id, index))
    }
    return json([])
  })
}

/**
 * verifyBundleMatches は `URL_BASE` が配っている JS bundle のファイル名が
 * ローカルの `dist/assets/` の現物と一致するかを見る（badge-links.mjs /
 * sse-refresh.mjs と同じ理由 --- 複数 worktree を並行して触っていると、
 * 別プロセスの preview が同じポートに先に居座って無関係な古いビルドを
 * 測ったまま判定が進む事故が実際にある）。
 */
function verifyBundleMatches(servedHtml) {
  const served = /assets\/(index-[^"]+\.js)/.exec(servedHtml)?.[1]
  let local
  try {
    local = readdirSync(path.join(process.cwd(), 'dist', 'assets')).find((f) =>
      /^index-.*\.js$/.test(f),
    )
  } catch {
    local = undefined
  }
  return { served, local, matches: served !== undefined && served === local }
}

log('\n=== ⓪ 前提条件 ===')
{
  const rootHtml = await fetch(URL_BASE + '/').then((r) => r.text())
  const bundle = verifyBundleMatches(rootHtml)
  if (!bundle.matches) {
    log(`NG  ⓪ 配っている bundle（${bundle.served ?? '不明'}）が dist/assets/（${bundle.local ?? '不明'}）と違う`)
    log('    別プロセス・古いビルドを測っている。以降の判定に意味が無いので打ち切る')
    process.exit(1)
  }
  log(`OK  ⓪ 配っている bundle は自分の dist（${bundle.served}）`)
}

const browser = await chromium.launch()

/**
 * bottomNavBox はモバイルのボトムタブの矩形を返す（無ければ null）。
 *
 * **`aria-label="主ナビゲーション"` の `<nav>` は 2 本ある**（デスクトップの
 * サイドバーとモバイルのボトムタブ。`app-shell.test.tsx` が `getAllByRole` で
 * 扱っている）。`.last()` で当てるのは `AppShell` の DOM 順に依存した当て方で、
 * 順が入れ替わると 390px でも `hidden md:flex` のサイドバー側を掴んで矩形が
 * null になり、**重なり判定が黙って消えて全体は green のまま**になる
 * （レビュー指摘。①でボタンの本数を `!== 1` で厳格化したのと同じ理由）。
 * ボトムタブだけが持つ `fixed` で一意に指し、390px（`md` 未満）で矩形が
 * 取れないことは NG として報告する。
 */
async function bottomNavBox(page, viewport, mark, label) {
  const nav = page.locator('nav[aria-label="主ナビゲーション"].fixed')
  const count = await nav.count()
  if (count !== 1) {
    ng.push(`${mark}@${label}: ボトムタブ（nav.fixed）がちょうど 1 本ではない（${count} 本）`)
    return null
  }
  const box = await nav.boundingBox()
  if (box === null || box.width === 0) {
    // `md` 未満ではボトムタブが必ず出ている（`md:hidden` は 768px 以上で消す）。
    // 矩形が取れないなら判定できていないので、黙ってスキップせず NG にする。
    if (viewport.width < 768) {
      ng.push(
        `${mark}@${label}: ボトムタブの矩形が取れない` +
          `（md 未満では必ず出ているはず。重なり判定ができていない）`,
      )
    } else {
      log(`  ${mark}@${label} ボトムタブは md 以上のため非表示（重なり判定は対象外）`)
    }
    return null
  }
  return box
}

/**
 * checkViewport は 1 viewport ぶんの判定（①②④）をまとめて行う。
 *
 * **①②を測り終えるまでスクロールも操作もしない** --- 「初画面（スクロール前）で
 * 主操作に届くか」を見る判定でスクロールすると、直したい問題そのものを回避して
 * しまう。④は定義上「押した後」なので、①②の後にだけ操作する。
 */
async function checkViewport(viewport) {
  const context = await browser.newContext({ viewport })
  const page = await context.newPage()
  await installApiStubs(page)
  await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })

  // サービスチップの描画を待つ（`useListServices` の解決後に出る）。ここで
  // 待つのはレイアウトの安定を待つためで、以降は一切操作・スクロールしない。
  await page.getByRole('button', { name: 'NHK総合' }).waitFor({ timeout: 15000 })
  await page.waitForTimeout(200)

  const label = `${viewport.width}x${viewport.height}`

  // --- ① 「検索」ボタンがビューポート内・ボトムタブの上に見えている ---
  const searchButton = page.getByRole('button', { name: '検索', exact: true })
  const buttonCount = await searchButton.count()
  // 「1 本だけ」を要求する。0 本なら判定対象が無いし、2 本以上なら `.first()` が
  // 黙って別の要素を測り、測っていないものを緑で報告してしまう（レビュー指摘）。
  if (buttonCount !== 1) {
    ng.push(`①@${label}: 「検索」ボタンがちょうど 1 本ではない（${buttonCount} 本）`)
  } else {
    const buttonBox = await searchButton.boundingBox()
    if (buttonBox === null) {
      ng.push(`①@${label}: 「検索」ボタンの矩形が取れない（非表示扱い）`)
    } else {
      log(`  ①@${label} 「検索」ボタンの矩形: top=${Math.round(buttonBox.y)} bottom=${Math.round(buttonBox.y + buttonBox.height)}`)
      if (buttonBox.y < 0 || buttonBox.y + buttonBox.height > viewport.height) {
        ng.push(
          `①@${label}: 「検索」ボタンが初画面（スクロール無し）のビューポート内に` +
            `収まっていない（top=${buttonBox.y}, bottom=${buttonBox.y + buttonBox.height}, viewport高さ=${viewport.height}）`,
        )
      }

      // ボトムタブの上端より下にボタンの下端がめり込んでいないことも見る。
      const navBox = await bottomNavBox(page, viewport, '①', label)
      if (navBox !== null) {
        log(`  ①@${label} ボトムタブの矩形: top=${Math.round(navBox.y)}`)
        if (buttonBox.y + buttonBox.height > navBox.y) {
          ng.push(
            `①@${label}: 「検索」ボタン（下端=${buttonBox.y + buttonBox.height}）が` +
              `ボトムタブ（上端=${navBox.y}）に隠れている`,
          )
        }
      }
    }
  }

  // --- ② テキスト条件の入力欄が「条件を追加」を押さずに見えており、
  //        サービスチップ列より前に来る ---
  const textInput = page.getByLabel('テキスト条件 1 の値')
  const textInputCount = await textInput.count()
  if (textInputCount !== 1) {
    ng.push(
      `②@${label}: テキスト条件の入力欄が「条件を追加」を押さずには見えない` +
        `（初画面に打つ場所が無い。見つかった数=${textInputCount}）`,
    )
  } else {
    const textBox = await textInput.boundingBox()
    const chipBox = await page.getByRole('button', { name: 'NHK総合' }).boundingBox()
    if (textBox === null || chipBox === null) {
      ng.push(`②@${label}: テキスト条件欄またはサービスチップの矩形が取れない`)
    } else {
      log(
        `  ②@${label} テキスト条件欄 top=${Math.round(textBox.y)} / サービスチップ top=${Math.round(chipBox.y)}`,
      )
      if (textBox.y >= chipBox.y) {
        ng.push(
          `②@${label}: テキスト条件の入力欄（top=${textBox.y}）がサービスチップ列` +
            `（top=${chipBox.y}）より後ろに来ている`,
        )
      }
    }
  }

  // --- ④ 押した結果（件数行・結果 1 件目）が折り目の中に出る ---
  // ①②はここまで一切操作せずに測っている。④は「押した後」の判定なので、
  // ここから先だけ操作する（この順序は動かさないこと）。
  await checkSubmitFeedback(page, viewport, label)

  await context.close()
}

/**
 * checkSubmitFeedback は「検索を押した結果が折り目の中に見えるか」を判定する（④）。
 *
 * ①②（主操作が初画面に届くか）だけでは足りない --- 主操作をカラムの上端へ
 * 動かしても総スクロール量は変わらないので、**押しても画面が変わらず、結果を
 * 見るために下までスクロールする**状態が①②とも OK のまま成立する（レビューで
 * 実測: クリック後も `window.scrollY = 0`、件数行は y=1179 で折り目 844 の
 * 335px 下、折り目の中には条件フォームしか無い）。受け入れの本体は「初画面から
 * 検索して結果に届くか」なので、そこまで判定を伸ばす。
 *
 * 見るのは、テキスト条件に打って「検索」を押した後:
 * - 件数行（`N 件（番組 ID 順）`）の矩形がビューポート内にあり、ページヘッダ
 *   （`sticky`）の下に潜っていないこと（`scroll-margin-top` の付け忘れはここで出る）
 * - ボトムタブに隠れていないこと
 * - 結果の 1 件目の上端も折り目の中にあること（件数行だけ見えて結果が全部
 *   下、という状態を通さない）
 */
async function checkSubmitFeedback(page, viewport, label) {
  await page.getByLabel('テキスト条件 1 の値').fill('ニュース')
  await page.getByRole('button', { name: '検索', exact: true }).click()

  const countRow = page.getByText(/件（番組 ID 順）/)
  try {
    await countRow.waitFor({ timeout: 15000 })
  } catch {
    ng.push(`④@${label}: 「検索」を押しても件数行が出ない（検索スタブが届いていない）`)
    return
  }
  // 結果 1 件目の中身（skeleton → 本物）が届くのを待つ。skeleton と本物で行の
  // 高さが違うので、届く前に測ると別のレイアウトを測ってしまう。
  const firstRow = page.locator('[data-testid="search-results"] > li').first()
  await firstRow.getByText(/^ニュース \d+$/).waitFor({ timeout: 15000 })
  await page.waitForTimeout(200)

  const scrollY = await page.evaluate(() => window.scrollY)
  const countBox = await countRow.boundingBox()
  const firstRowBox = await firstRow.boundingBox()
  // ページヘッダは `sticky`。件数行がこの下に潜っていたら「見えている」とは言えない。
  const headerBox = await page.locator('header:has(h1)').first().boundingBox()
  if (countBox === null || firstRowBox === null || headerBox === null) {
    ng.push(`④@${label}: 件数行・結果 1 件目・ページヘッダのいずれかの矩形が取れない`)
    return
  }

  log(
    `  ④@${label} クリック後 scrollY=${Math.round(scrollY)} / 件数行 top=${Math.round(countBox.y)}` +
      ` / 結果 1 件目 top=${Math.round(firstRowBox.y)} / ヘッダ下端=${Math.round(headerBox.y + headerBox.height)}`,
  )

  if (countBox.y < 0 || countBox.y + countBox.height > viewport.height) {
    ng.push(
      `④@${label}: 「検索」を押した後も件数行が折り目の外（top=${countBox.y}, ` +
        `bottom=${countBox.y + countBox.height}, viewport高さ=${viewport.height}）。` +
        `押した結果が画面に出ていない`,
    )
  }
  if (countBox.y < headerBox.y + headerBox.height) {
    ng.push(
      `④@${label}: 件数行（top=${countBox.y}）が sticky なページヘッダ` +
        `（下端=${headerBox.y + headerBox.height}）の下に潜っている`,
    )
  }
  if (firstRowBox.y > viewport.height) {
    ng.push(
      `④@${label}: 結果 1 件目（top=${firstRowBox.y}）が折り目の外（viewport高さ=${viewport.height}）`,
    )
  }

  const navBox = await bottomNavBox(page, viewport, '④', label)
  if (navBox !== null) {
    if (countBox.y + countBox.height > navBox.y) {
      ng.push(
        `④@${label}: 件数行（下端=${countBox.y + countBox.height}）がボトムタブ` +
          `（上端=${navBox.y}）に隠れている`,
      )
    }
    if (firstRowBox.y > navBox.y) {
      ng.push(
        `④@${label}: 結果 1 件目（top=${firstRowBox.y}）がボトムタブ（上端=${navBox.y}）より下`,
      )
    }
  }
}

log('\n=== ① ② ④ モバイル（390x844） ===')
await checkViewport({ width: 390, height: 844 })

log('\n=== ③ デスクトップ（1280x900）でも②④を確認 ===')
await checkViewport({ width: 1280, height: 900 })

await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
