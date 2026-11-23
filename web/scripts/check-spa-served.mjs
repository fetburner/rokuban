#!/usr/bin/env node
import path from 'node:path'
import { fileURLToPath } from 'node:url'

function assert(condition, message) {
  if (!condition) throw new Error(message)
}

function localAssets(html, baseURL) {
  const assets = []
  for (const [tag] of html.matchAll(/<[^>]+>/g)) {
    for (const [, key, doubleQuoted, singleQuoted, unquoted] of tag.matchAll(/([\w:-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/g)) {
      const url = doubleQuoted ?? singleQuoted ?? unquoted
      if ((key === 'src' || key === 'href') && url && !/^[a-z][a-z\d+.-]*:/i.test(url) && !url.startsWith('//')) {
        assets.push(new URL(url, baseURL))
      }
    }
  }
  return assets
}

/** checkSPAServed は本体サーバーが SPA とビルド資産を配ることを確認する。 */
export async function checkSPAServed(baseURL, fetchImpl = fetch) {
  const base = new URL(baseURL)
  const root = await fetchImpl(base)
  assert(root.status === 200, `/ は HTTP ${root.status} を返しました`)
  assert((root.headers.get('content-type') ?? '').includes('text/html'), '/ は HTML を返しませんでした')
  const index = await root.text()
  const assets = localAssets(index, base)
  assert(assets.length > 0, 'index.html にローカル資産の参照がありません')
  assert(assets.some(({ pathname }) => pathname.startsWith('/assets/')), 'index.html に /assets/ の参照がありません')

  for (const asset of assets) {
    const response = await fetchImpl(asset)
    assert(response.status === 200, `資産 ${asset.pathname} は HTTP ${response.status} を返しました`)
    if (asset.pathname.startsWith('/assets/')) {
      assert(!(response.headers.get('content-type') ?? '').includes('text/html'), `資産 ${asset.pathname} が HTML にフォールバックしました`)
    }
    await response.body?.cancel()
  }

  const deep = await fetchImpl(new URL('/ci-spa-fallback/deep/route', base))
  assert(deep.status === 200, `深いパスは HTTP ${deep.status} を返しました`)
  assert(await deep.text() === index, '深いパスが index.html にフォールバックしませんでした')
}

async function main() {
  const baseURL = process.argv[2]
  if (!baseURL) throw new Error('使い方: node web/scripts/check-spa-served.mjs <base-url>')
  await checkSPAServed(baseURL)
  console.log(`SPA assets and fallback are served at ${baseURL}`)
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message)
    process.exitCode = 1
  })
}
