// /search の主操作（検索）がモバイル初画面に届くことの受け入れ判定（issue #305）。
//
// jsdom は `getBoundingClientRect()` が常に 0 を返し、レイアウト・可視性・
// スクロール位置を測れない（web/e2e/README.md「jsdom が測れないもの」）。
// 「390px 幅の初画面でボタンがボトムタブに隠れない」「テキスト条件の入力が
// サービスチップより先に届く」はどちらも実レイアウトの上下関係そのものなので、
// ここでしか判定できない。
//
// 見るのは（すべて `page.goto` 直後、スクロールを一切せずに測る --- 「初画面」を
// 検証する判定でスクロールしてしまうと、直したい問題自体を回避してしまう）:
//   ⓪ 前提条件 --- 配っている bundle が dist/ の現物と一致するか
//      （badge-links.mjs / sse-refresh.mjs と同じ理由。web/e2e/README.md）
//   ① 390px 幅で「検索」ボタンの矩形がビューポート内に収まり、モバイルの
//      ボトムタブ（`nav[aria-label="主ナビゲーション"]`）と重なっていないこと
//   ② テキスト条件 1 行目の値入力（`条件を追加` を押さなくても出ている。
//      issue #305 の対処本体）が、初画面で「条件を追加」を押さずに直接
//      見えており、サービスのチップ列より前（画面の上）にあること
//   ③ 1280px（デスクトップ）でも同じ 2 点を確認する --- issue 本文は
//      「デスクトップではボタンは見えるが、キーワードは同じく一段奥」と
//      言っており、②はデスクトップでも直す対象
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
 * checkViewport は 1 viewport ぶんの判定（①②）をまとめて行う。
 *
 * **スクロールは一切しない** --- 「初画面（スクロール前）で主操作に届くか」を
 * 見る判定でスクロールすると、直したい問題そのものを回避してしまう。
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

      // モバイルのボトムタブ（`md:hidden`）が居るなら、その上端より下に
      // ボタンの下端がめり込んでいないことも見る。
      const bottomNav = page.locator('nav[aria-label="主ナビゲーション"]').last()
      const navBox = (await bottomNav.count()) > 0 ? await bottomNav.boundingBox() : null
      if (navBox !== null && navBox.width > 0) {
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

  await context.close()
}

log('\n=== ① ② モバイル（390x844） ===')
await checkViewport({ width: 390, height: 844 })

log('\n=== ③ デスクトップ（1280x900）でも②を確認 ===')
await checkViewport({ width: 1280, height: 900 })

await browser.close()

log('\n=== 結果 ===')
if (ng.length === 0) log('  すべて期待どおり')
else ng.forEach((f) => log('  NG: ' + f))

process.exit(ng.length === 0 ? 0 : 1)
