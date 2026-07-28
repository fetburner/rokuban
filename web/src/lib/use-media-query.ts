import { useEffect, useState } from 'react'

/** lgMediaQuery は Tailwind の `lg`（64rem）に対応するメディアクエリ。 */
export const lgMediaQuery = '(min-width: 64rem)'

/**
 * useMediaQuery はメディアクエリの成否を購読する。
 *
 * 番組表グリッドを CSS（`hidden lg:block`）で出し分けないのは、グリッドが
 * 自前のクエリ（全サービス x 24 時間）と仮想化を持つため。DOM に置いたまま
 * CSS で隠すと、モバイルでも取得と描画のコストを払うことになる
 * （docs/frontend.md「グリッドは lg 以上でのみ出し、モバイルは常にリスト」）。
 *
 * `window.matchMedia` を持たない環境では false を返す。グリッドが出ない側
 * （= リスト）に倒れるので、第一級のビューが失われることはない。
 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(false)

  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const list = window.matchMedia(query)
    const update = () => setMatches(list.matches)
    update()
    list.addEventListener('change', update)
    return () => list.removeEventListener('change', update)
  }, [query])

  return matches
}
