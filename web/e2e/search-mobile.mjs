// /search の主操作（検索）がモバイル初画面に届くことの受け入れ判定（issue #305）。
//
// jsdom は `getBoundingClientRect()` が常に 0 を返し、レイアウト・可視性・
// スクロール位置を測れない（web/e2e/README.md「jsdom が測れないもの」）。
// 「390px 幅の初画面でボタンがボトムタブに隠れない」「キーワード値が select の
// 下に専用行で広がる」はどちらも実レイアウトの上下関係そのものなので、
// ここでしか判定できない。
//
// 見るのは:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか
//      （badge-links.mjs / sse-refresh.mjs と同じ理由。web/e2e/README.md）
//   ① 360px / 390px 幅で「検索」ボタンの矩形がビューポート内に収まり、モバイルの
//      ボトムタブ（`nav[aria-label="主ナビゲーション"].fixed`）と重なっていないこと
//   ② モバイルではテキスト条件 1 行目の対象・モードが同じ行にあり、値入力が
//      その下の専用行をほぼ使うこと。現在の `w-28 shrink-0` 横並びへ戻す変異で
//      失敗する（デスクトップは従来どおり一行レイアウトを確認する）
//   ③ URL から復元した詳細条件が閉じたまま要約され、キーボードで開閉・
//      日本語の入力修正・要約からの解除ができること
//   ④ 1280px（デスクトップ）でも入力欄の存在と、「検索」を押した後の結果（件数行・結果 1 件目）が折り目の中に
//      見えること。①②だけだと「押しても画面が変わらず、結果を見るために
//      下までスクロールする」状態が緑で通る（レビューで実測）
//   ⑤ 検索結果の予約ボタンがモバイルでも 44px の標的として出て、キーボードの
//      Enter で単発予約へ進めること
//   ⑥ 長いサービス名と有料表示を持つ結果行で、メタ行が 1 行に収まり、行が
//      不要に 2 行ぶん高くならないこと（issue #712）
//
// **①②は `page.goto` 直後、スクロールも操作も一切せずに測る** --- 「初画面」を
// 検証する判定でスクロールしてしまうと、直したい問題自体を回避してしまう。
// ④⑤だけが「押した後」の判定なので、①②を測り終えてから操作する。
// **⑤は④の測定より後に置く**（⑤の Tab がスクロールを起こす。理由と実測値は
// `checkResultReservation` のコメント）。
//
// **mirakc も実チューナーも DB も要らない。** API は `page.route` で丸ごと
// 差し替える（design.mjs と同じ手）。
//
//   pnpm build && pnpm preview --port 4173 --strictPort &
//   E2E_URL=http://localhost:4173 node e2e/search-mobile.mjs
//
// 合格なら exit 0、1 つでも NG なら exit 1。
import { finish, installApiStubs, launchBrowser, log, verifyBundleMatchesOrExit } from './lib.mjs'

const URL_BASE = process.env.E2E_URL ?? 'http://localhost:4173'
const SITE = 'default'

const ng = []

const services = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'ＮＨＫＢＳプレミアム４Ｋ',
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

const restoredCondition = {
  genres: [0],
  services: [{ networkId: 32736, serviceId: 1024 }],
}

/**
 * matchedProgramIds は検索スタブが返す programId の集合（④で使う）。
 *
 * 検索 API（`POST /api/programs/search`）は `{site, programId}` の
 * フラットな配列を返し、画面は 1 件ごとに `GET /api/sites/{site}/programs/{id}` を
 * 叩く（実物と同じ形）。
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
    isFree: false,
  }
}

/** apiHandler は /search の描画に要る `/api/**` の応答を作る。 */
async function apiHandler({ path: p, json, route }) {
  if (p === '/api/sites') return json([SITE])
  if (p === '/api/capabilities') return json({ live: false })
  if (p === `/api/sites/${SITE}/services`) return json(services)
  if (p === '/api/reservations') return json([])
  const intent = /^\/api\/sites\/([^/]+)\/programs\/(\d+)\/intent$/.exec(p)
  if (intent !== null && route.request().method() === 'PUT') {
    return route.fulfill({ status: 204 })
  }
  if (p === '/api/programs/search')
    return json(matchedProgramIds.map((programId) => ({ site: SITE, programId })))
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
}

log('\n=== ⓪ 前提条件 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()

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

/** firstResultRow は検索結果の 1 件目の行（④の矩形測定と⑤の操作が同じ行を見る）。 */
function firstResultRow(page) {
  return page.locator('[data-testid="search-results"] > li').first()
}

/**
 * checkViewport は 1 viewport ぶんの判定（①②④⑤）をまとめて行う。
 *
 * **①②を測り終えるまでスクロールも操作もしない** --- 「初画面（スクロール前）で
 * 主操作に届くか」を見る判定でスクロールすると、直したい問題そのものを回避して
 * しまう。④⑤は定義上「押した後」なので、①②の後にだけ操作する。
 */
async function checkViewport(viewport) {
  const context = await browser.newContext({ viewport })
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  await page.goto(URL_BASE + '/search', { waitUntil: 'domcontentloaded' })

  // 検索画面は詳細条件を初期状態で閉じるため、開閉ボタンの描画を待つ。ここで
  // 待つのはレイアウトの安定を待つためで、以降は④まで操作・スクロールしない。
  await page.getByRole('button', { name: '詳細条件を表示', exact: true }).waitFor({ timeout: 15000 })
  // サイト一覧取得中は「検索」ボタンの直上に role=status の行が出る
  // （`pages/search.tsx` の `registryPending`）。取得が終わるとこの行が DOM から
  // 消え、ボタンがその分だけ上へ動く。固定 200ms 待ちだと、環境によってはこの
  // 行がまだ残っている間に①の矩形を測ってしまい、レイアウト確定前の値を見る
  // （レビュー指摘）。「消えた」ことそのものを待つ。
  await page
    .getByText('サイト一覧を取得中…')
    .waitFor({ state: 'hidden', timeout: 15000 })

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

  // --- ② 対象・モードと値をモバイルで分け、値に専用行の幅を使う ---
  const textInput = page.getByLabel('テキスト条件 1 の値')
  const textTarget = page.getByLabel('テキスト条件 1 の対象')
  const textMode = page.getByLabel('テキスト条件 1 のモード')
  const textInputCount = await textInput.count()
  const targetCount = await textTarget.count()
  const modeCount = await textMode.count()
  if (textInputCount !== 1 || targetCount !== 1 || modeCount !== 1) {
    ng.push(
      `②@${label}: テキスト条件の対象・モード・値が初画面に揃っていない` +
        `（対象=${targetCount}, モード=${modeCount}, 値=${textInputCount}）`,
    )
  } else {
    const textBox = await textInput.boundingBox()
    const targetBox = await textTarget.boundingBox()
    const modeBox = await textMode.boundingBox()
    const formBox = await page.getByRole('form', { name: '検索条件' }).boundingBox()
    if (textBox === null || targetBox === null || modeBox === null || formBox === null) {
      ng.push(`②@${label}: テキスト条件または検索フォームの矩形が取れない`)
    } else {
      const selectBottom = Math.max(targetBox.y + targetBox.height, modeBox.y + modeBox.height)
      const formContentWidth = formBox.width - 32
      log(
        `  ②@${label} 対象 top=${Math.round(targetBox.y)} / モード top=${Math.round(modeBox.y)}` +
          ` / 値 top=${Math.round(textBox.y)} width=${Math.round(textBox.width)}` +
          ` / フォーム内幅=${Math.round(formContentWidth)}`,
      )
      if (viewport.width < 640 && textBox.y <= selectBottom) {
        ng.push(
          `②@${label}: キーワード入力欄（top=${textBox.y}）が対象・モード行の下にない` +
            `（select 行の下端=${selectBottom}）`,
        )
      }
      if (viewport.width < 640 && Math.abs(targetBox.y - modeBox.y) > 1) {
        ng.push(
          `②@${label}: 対象とモードが同じ行にない（対象 top=${targetBox.y}, モード top=${modeBox.y}）`,
        )
      }
      if (viewport.width < 640 && textBox.width < formContentWidth * 0.8) {
        ng.push(
          `②@${label}: キーワード入力欄が専用行の幅を使っていない` +
            `（入力幅=${textBox.width}, フォーム内幅=${formContentWidth}）`,
        )
      }
      // デスクトップ（640px 以上）は対象・モード・値が同じ行の従来レイアウト
      // （`sm:flex-row`）のまま。モバイルの③つの assertion は全部 640px 未満に
      // ガードされていたため、`sm:flex-row` を落とす変異（この分割が持ち込む
      // 回帰そのもの）が全ビューポートで通ってしまっていた（レビュー指摘）。
      // 対象・モード・値の top がほぼ一致する（= 同じ行にある）ことを見る。
      if (
        viewport.width >= 640 &&
        (Math.abs(targetBox.y - modeBox.y) > 1 || Math.abs(targetBox.y - textBox.y) > 1)
      ) {
        ng.push(
          `②@${label}: デスクトップで対象・モード・値が同じ行にない` +
            `（対象 top=${targetBox.y}, モード top=${modeBox.y}, 値 top=${textBox.y}）`,
        )
      }
    }
  }

  // --- ④ 押した結果（件数行・結果 1 件目）が折り目の中に出る ---
  // ①②はここまで一切操作せずに測っている。④は「押した後」の判定なので、
  // ここから先だけ操作する（この順序は動かさないこと）。
  await checkSubmitFeedback(page, viewport, label)

  // --- ⑤ 結果行の予約導線 ---
  // **④を測り終えた後にだけ操作する**（`checkResultReservation` のコメント）。
  // `checkSubmitFeedback` の中に置くと、その早期 return で⑤が黙って消えるため
  // ここから呼ぶ。
  await checkResultReservation(page, label)

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
  const firstRow = firstResultRow(page)
  await firstRow.getByText(/^ニュース \d+$/).waitFor({ timeout: 15000 })
  await page.waitForTimeout(200)

  await checkSearchResultMeta(page, label)

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

/**
 * checkSearchResultMeta は検索結果のメタ行が 1 行で描画されることを判定する（⑥）。
 *
 * 長いサービス名と「有料」を同時に持つ fixture を使う。メタ行に `flex-wrap` が
 * 残っているとサービス名が縮む前に折り返し、`getBoundingClientRect()` の高さが
 * computed style の 1 行ぶんを超える。jsdom はレイアウトを計算しないため、実際の
 * Chromium でしかこの差を検出できない。
 */
async function checkSearchResultMeta(page, label) {
  const firstRow = firstResultRow(page)
  const meta = firstRow.getByTestId('search-result-meta')
  const count = await meta.count()
  if (count !== 1) {
    ng.push(`⑥@${label}: 結果 1 件目のメタ行がちょうど 1 本ではない（${count} 本）`)
    return
  }

  const rowBox = await firstRow.boundingBox()
  const metaBox = await meta.boundingBox()
  const metrics = await meta.evaluate((element) => {
    const style = getComputedStyle(element)
    return {
      flexWrap: style.flexWrap,
      lineHeight: Number.parseFloat(style.lineHeight),
      text: element.textContent ?? '',
    }
  })

  if (rowBox === null || metaBox === null) {
    ng.push(`⑥@${label}: 結果行またはメタ行の矩形が取れない`)
    return
  }

  log(
    `  ⑥@${label} 結果行 height=${Math.round(rowBox.height)} / ` +
      `メタ行 height=${Math.round(metaBox.height)} line-height=${metrics.lineHeight}` +
      ` / flex-wrap=${metrics.flexWrap}`,
  )

  if (!metrics.text.includes('ＮＨＫＢＳプレミアム４Ｋ') || !metrics.text.includes('有料')) {
    ng.push(`⑥@${label}: 長いサービス名と「有料」の fixture が結果行に出ていない`)
  }
  if (metrics.flexWrap !== 'nowrap') {
    ng.push(`⑥@${label}: 結果行のメタ行が折り返し禁止になっていない（${metrics.flexWrap}）`)
  }
  if (!Number.isFinite(metrics.lineHeight)) {
    ng.push(`⑥@${label}: メタ行の line-height を取得できない`)
  } else if (metaBox.height > metrics.lineHeight + 1) {
    ng.push(
      `⑥@${label}: メタ行が 1 行に収まっていない（height=${metaBox.height}, ` +
        `line-height=${metrics.lineHeight}）`,
    )
  }
}

/**
 * 検索結果の予約導線を、実ブラウザの Tab + Enter で確認する（⑤）。
 *
 * 予約ボタンは検索結果の行本体とは別のタブ停止可能な要素であり、モバイルでも
 * 44px の標的を持つ。jsdom では実際の Tab 移動・ボタン矩形を測れないため、ここで
 * 「探し直さず、その場の行から」操作できることを固定する。
 *
 * **④を測り終えた後にだけ呼ぶこと。** ここで押す `Tab` は要素にフォーカスを移し、
 * フォーカスはブラウザ既定のスクロールを起こす。④より前に置くと、④が捕まえる
 * べき「押しても結果が折り目の外」の回帰を⑤自身が回避してしまう
 * （実測: 送信後の `scrollIntoView` を落として結果 1 件目を折り目の下へ出す変異で、
 * ⑤を④の前に置くと `scrollY` が 0 のはずが 458 になり、件数行 816・結果 1 件目 848
 * が 358・390 まで引き上げられて④が「すべて期待どおり」で通った）。
 */
async function checkResultReservation(page, label) {
  const firstRow = firstResultRow(page)
  const reserveButton = firstRow.getByRole('button', { name: '予約', exact: true })
  const count = await reserveButton.count()
  if (count !== 1) {
    ng.push(`⑤@${label}: 結果 1 件目の「予約」ボタンがちょうど 1 本ではない（${count} 本）`)
    return
  }

  const box = await reserveButton.boundingBox()
  if (box === null) {
    ng.push(`⑤@${label}: 結果 1 件目の「予約」ボタンの矩形が取れない`)
    return
  }
  log(
    `  ⑤@${label} 予約ボタンの矩形: width=${Math.round(box.width)} height=${Math.round(box.height)}`,
  )
  if (box.width < 44 || box.height < 44) {
    ng.push(
      `⑤@${label}: 結果行の予約ボタンが 44px の標的に満たない` +
        `（width=${box.width}, height=${box.height}）`,
    )
  }

  // 検索の決着後は結果セクションへフォーカスしている。そこから Tab で結果行の
  // 予約ボタンへ入り、Enter で操作する --- locator.click() だけではキーボード
  // 到達性を確認できない。
  await page.keyboard.press('Tab')
  const focusedByTab = await reserveButton.evaluate((element) => document.activeElement === element)
  if (!focusedByTab) {
    ng.push(`⑤@${label}: Tab で結果行の「予約」ボタンへ到達できない`)
    await reserveButton.focus()
  }
  await page.keyboard.press('Enter')

  const cancelButton = firstRow.getByRole('button', { name: '取消', exact: true })
  try {
    await cancelButton.waitFor({ timeout: 15000 })
  } catch {
    ng.push(`⑤@${label}: 結果行の「予約」を Enter で押しても「取消」状態にならない`)
  }
}

/** 復元した詳細条件の要約・開閉・キーボード入力を実ブラウザで確認する。 */
async function checkRestoredDetails() {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 } })
  const page = await context.newPage()
  await installApiStubs(page, apiHandler)
  const cond = encodeURIComponent(JSON.stringify(restoredCondition))
  await page.goto(`${URL_BASE}/search?cond=${cond}`, { waitUntil: 'domcontentloaded' })

  const summary = page.getByTestId('detail-condition-summary')
  await summary.getByText('設定中の詳細条件: 2件').waitFor({ timeout: 15000 })
  // 件数（issue #685）が入るとアクセシブル名が `詳細条件を表示（2件）` になる
  // （アクセシブル名は可視テキストと同じ。WCAG 2.5.3 Label in Name。この
  // フィクスチャは常に条件 2 件なので厳密にはこの文字列でも当てられるが、
  // `pages/search.test.tsx` / `condition-fields.test.tsx` と同じ規約（先頭一致）
  // に揃える）。
  const toggle = page.getByRole('button', { name: /^詳細条件を表示/ })
  if ((await toggle.getAttribute('aria-expanded')) !== 'false') {
    ng.push('③: URL から復元した詳細条件が初期状態で閉じていない')
  }
  if ((await page.getByRole('group', { name: 'チャンネル', exact: true }).count()) !== 0) {
    ng.push('③: 閉じた詳細条件のチャンネル選択肢が画面に残っている')
  }

  await toggle.focus()
  await page.keyboard.press('Enter')
  const openToggle = page.getByRole('button', { name: '詳細条件を閉じる', exact: true })
  if ((await openToggle.getAttribute('aria-expanded')) !== 'true') {
    ng.push('③: キーボードの Enter で詳細条件を開けない')
  }
  await page.getByRole('group', { name: 'チャンネル', exact: true }).waitFor({ timeout: 15000 })

  const input = page.getByLabel('テキスト条件 1 の値')
  await input.fill('ニュース')
  await input.press('End')
  await input.press('Backspace')
  await input.type('ス')
  const value = await input.inputValue()
  log(`  ③ 日本語の入力 → カーソル移動 → 削除 → 再入力: ${value}`)
  if (value !== 'ニュース') ng.push(`③: 日本語の修正結果が不正（${value}）`)

  await openToggle.click()
  const closedToggle = page.getByRole('button', { name: /^詳細条件を表示/ })
  if ((await closedToggle.getAttribute('aria-expanded')) !== 'false') {
    ng.push('③: 詳細条件を再び閉じられない')
  }
  await summary.getByRole('button', { name: 'ジャンルの条件を解除' }).click()
  if ((await summary.getByText('設定中の詳細条件: 1件').count()) === 0) {
    ng.push('③: 要約からジャンルを解除しても件数が減らない')
  }

  await context.close()
}

log('\n=== ① ② ④ ⑤ モバイル（360/390x844） ===')
await checkViewport({ width: 360, height: 844 })
await checkViewport({ width: 390, height: 844 })

log('\n=== ③ 詳細条件の要約・キーボード操作（390x844） ===')
await checkRestoredDetails()

log('\n=== ③ デスクトップ（1280x900）でも②④⑤を確認 ===')
await checkViewport({ width: 1280, height: 900 })

await finish(ng, browser)
