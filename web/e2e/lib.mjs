// e2e/*.mjs 全体で共有する前置き。CLAUDE.md §テスト規律「非同期の空虚な成功に
// 注意する」と同じ理由で、ここに置くのは判定の**前後**（ブラウザの起動・終了、
// 配っている bundle が dist/ の現物と一致するかの確認、`/api/**` の配線、結果の
// 集計と終了コード）だけ。各スクリプト固有の判定（何が OK/NG かの基準）は置かない
// --- そちらは README.md「判定を足すときの規律」のとおり各 *.mjs 本体にとどめる。
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { chromium, firefox, webkit } from 'playwright'

const ENGINES = { chromium, firefox, webkit }

/**
 * launchBrowser は指定したエンジンでブラウザを起動する。既定は chromium。
 *
 * chromium 固定にしていない --- `live.mjs` は WebKit（Safari 相当のネイティブ
 * HLS 経路を確認するため）と `{ channel: 'chrome' }`（実 H.264/AAC 再生の
 * ため）も起動する。
 */
export function launchBrowser(engine = 'chromium', options) {
  return ENGINES[engine].launch(options)
}

/** log は各スクリプトの `console.log` 呼び出しの薄いラッパー。 */
export const log = (...a) => console.log(...a)

/**
 * finish は判定結果（`ng`）を集計して表示し、終了コードを決めて `process.exit`
 * する（呼び出し元に戻らない）。`browser` を渡すと終了前に close する。
 */
export async function finish(ng, browser) {
  log('\n=== 結果 ===')
  if (ng.length === 0) log('  すべて期待どおり')
  else ng.forEach((f) => log('  NG: ' + f))
  if (browser) await browser.close()
  process.exit(ng.length === 0 ? 0 : 1)
}

/**
 * verifyBundleMatches は `urlBase` が実際に配っている JS bundle のファイル名
 * （`index-<hash>.js`）と、ローカルの `dist/assets/` にある現物のファイル名を
 * 比較する。
 *
 * `page.route` で `/api/**` を丸ごと差し替える判定は、古い（無関係な）ビルドを
 * 配っているサーバーに対しても静かに動いてしまう --- サーバーが何のバイナリ・
 * dist を配っているかはその判定の関心外だからこそ、一致確認をここで能動的に
 * 行う。ファイル名にコンテンツハッシュが入っているため、内容が違えばファイル名も
 * 必ず違う（vite のデフォルト）。複数の worktree を並行して触っていると、
 * `--strictPort` を付けていても別 worktree の preview が同じポートに先に
 * 居座って自分の起動が黙って失敗し、`E2E_URL` が無関係な古いビルドを指したまま
 * 判定が進んでしまう事故が実際にあった（web/e2e/README.md 参照）。
 *
 * `dist/assets/` はこのスクリプトを `web/` をカレントディレクトリにして実行する
 * ことを前提に相対パスで読む。
 */
export async function verifyBundleMatches(urlBase) {
  const rootHtml = await fetch(urlBase + '/').then((r) => r.text())
  const served = /assets\/(index-[^"]+\.js)/.exec(rootHtml)?.[1]
  const distDir = path.join(process.cwd(), 'dist', 'assets')
  let local
  try {
    local = readdirSync(distDir).find((f) => /^index-.*\.js$/.test(f))
  } catch {
    local = undefined
  }
  return { served, local, matches: served !== undefined && served === local }
}

/**
 * verifyBundleMatchesOrExit は ⓪ の前提確認をまとめて行う --- `verifyBundleMatches`
 * で一致を確認し、served/local をログへ残し、不一致なら `finish` で打ち切る
 * （`browser` を渡せば終了前に close する。まだ起動していない呼び出し元は
 * 省略してよい）。一致すれば呼び出し元へ戻る。
 */
export async function verifyBundleMatchesOrExit(urlBase, ng, browser) {
  const bundleCheck = await verifyBundleMatches(urlBase)
  log(`  配っている bundle: ${bundleCheck.served ?? '(取得できない)'}`)
  log(`  dist/assets/     : ${bundleCheck.local ?? '(見つからない。web/ で実行しているか確認)'}`)
  if (!bundleCheck.matches) {
    ng.push(
      `⓪ ${urlBase} が配っている bundle（${bundleCheck.served ?? '不明'}）が dist/assets/ の現物（${bundleCheck.local ?? '不明'}）と一致しない --- 別プロセス・古いビルドを測っている可能性が高いので、これ以降の判定を打ち切る`,
    )
    await finish(ng, browser)
  }
  log('  一致（このサーバーは自分のビルドを配っている）')
}

/**
 * installApiStubs は `/api/**` を丸ごとブラウザ側で差し替える配線だけを共通化
 * する。各スクリプト固有の応答（フィクスチャ）は `handler` に残す --- `handler`
 * は `{ path, url, json, route }` を受け取り、`json(...)` か `route.fulfill(...)`
 * を呼んで応答する。どちらも呼ばなければ（該当パスが無ければ）、SSE
 * （`/api/events`）は 204（つなぎ直さずに諦めさせ、`networkidle` へ到達させる。
 * 各スクリプトが共通で書いていた既定）、それ以外は `json([])` にフォールバック
 * する（各スクリプトの catch-all と同じ既定）。
 *
 * **`handler` の戻り値では判定しない。** `route.fulfill(...)` は解決すると
 * `undefined` を返すため、`return route.fulfill(...)` は「何も返さなかった」と
 * 区別が付かない --- `route.fulfill` 自体を「呼ばれたら印を立てる」版に差し替え、
 * その印だけで判定する（実際にこの取り違えで「Route is already handled」を
 * 踏んだ）。`route` はそれ以外そのまま渡すので、`route.request()` 等を使う
 * handler（design.mjs の GET 判定など）もそのまま動く。
 */
export async function installApiStubs(page, handler) {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const p = url.pathname
    let handled = false
    const originalFulfill = route.fulfill.bind(route)
    route.fulfill = (...args) => {
      handled = true
      return originalFulfill(...args)
    }
    const json = (body) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
    await handler({ path: p, url, json, route })
    if (handled) return
    if (p === '/api/events') return route.fulfill({ status: 204 })
    return json([])
  })
}
