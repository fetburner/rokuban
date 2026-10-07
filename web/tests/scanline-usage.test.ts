import { readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 走査線ユーティリティ（`scanlines` / `tally-scanlines`）の使用箇所を固定する。
 *
 * `docs/frontend/design.md`「走査線は読み込み中・ON AIR の 2 箇所限定」---
 * EmptyState には使わないという決定を含め、他の用途へ広がっていないことを
 * grep 相当の機械判定にする。新しい実装ファイルへクラスを足すとここが落ちる。
 *
 * `src/` ではなくここに置くのは design-tokens.test.ts と同じ理由 ---
 * `?raw` は vitest の既定設定（`css: false`）で CSS ファイルを空文字列に
 * スタブするため、`index.css` を対象にするテストはここに置かないと
 * 素通りする。`node:fs` で直接読む
 */

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const srcDir = path.join(webDir, 'src')

/** walk は `srcDir` 以下の実装ファイル（テストを除く）を再帰的に列挙する。 */
function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry)
    if (statSync(full).isDirectory()) {
      out.push(...walk(full))
    } else if (/\.(tsx?|css)$/.test(entry) && !/\.test\.tsx?$/.test(entry)) {
      out.push(full)
    }
  }
  return out
}

/**
 * findUsers は指定したクラス名（単独のトークンとして）を含むファイルの
 * 相対パス一覧を返す。
 *
 * **`tally-scanlines` は文字列として `scanlines` を含む。** 単純な部分一致や
 * 素朴な `\bscanlines\b` では `tally-` の直後の `scanlines` も拾ってしまう
 * （ハイフンは JS の `\b` にとって非単語文字なので、そこにも語境界ができる）。
 * 前後が `-` でも単語文字でもないことを見て、独立したトークンだけを拾う
 */
function findUsers(className: string): string[] {
  const escaped = className.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const re = new RegExp(`(?<![\\w-])${escaped}(?![\\w-])`)
  const hits: string[] = []
  for (const file of walk(srcDir)) {
    const content = readFileSync(file, 'utf8')
    if (re.test(content)) hits.push(path.relative(webDir, file).split(path.sep).join('/'))
  }
  return hits.sort()
}

describe('走査線クラスの使用箇所', () => {
  it('scanlines は index.css（定義）と components/page.tsx（Skeleton）だけ', () => {
    // 使用元の追加はここで、Skeleton の実際のクラスは page.test.tsx で固定する。
    // EmptyState に scanlines を戻す変異は page.test.tsx と実ブラウザ gate で落とす。
    expect(findUsers('scanlines')).toEqual(['src/components/page.tsx', 'src/index.css'])
  })

  it('tally-scanlines は index.css（定義）と pages/live.tsx（ON AIR）だけ', () => {
    expect(findUsers('tally-scanlines')).toEqual(['src/index.css', 'src/pages/live.tsx'])
  })
})
