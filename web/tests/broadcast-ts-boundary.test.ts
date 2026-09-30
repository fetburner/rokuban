import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * フロントエンドが放送 TS を解釈しない（不変条件 6）ことを依存の粒度で固定する。
 *
 * `docs/recording/delegation.md`「ブラウザにも同じ境界を適用する」---ブラウザが
 * 扱うのは ffmpeg の出力（HLS / fMP4 / WebVTT）だけで、放送 TS を再生・解読する
 * ライブラリは本番の依存に入れないという決定を機械判定にする。hls.js は ffmpeg が
 * 書いた MPEG-TS セグメントを demux するが、放送 TS（PSI/SI・ARIB 字幕・BML）は
 * 扱わないので対象外。
 *
 * 見るのは `dependencies` だけ。`devDependencies` は計器（e2e）が基準として
 * 放送 TS を読むことを許しているので対象にしない。自前のコードで TS を解読する
 * 経路はこの判定では捕まらない（レビューで止める）。
 */

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

/** 放送 TS の再生（mpegts.js）・ARIB 字幕（aribb24.js / b24.js）・データ放送（BML）を名前で拾う。 */
const broadcastTsPackage = /mpegts|arib|b24|bml/i

describe('放送 TS を解釈するライブラリ', () => {
  it('本番の依存に入っていない', () => {
    const pkg = JSON.parse(readFileSync(path.join(webDir, 'package.json'), 'utf8')) as {
      dependencies?: Record<string, string>
    }
    const offending = Object.keys(pkg.dependencies ?? {}).filter((name) =>
      broadcastTsPackage.test(name),
    )
    expect(offending).toEqual([])
  })
})
