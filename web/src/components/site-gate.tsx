import type { ReactNode } from 'react'

import { useListSites } from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { SiteContext } from '@/lib/site'

/**
 * SiteGate は `GET /api/sites` を解決してから子を描画する（issue #184 M4-12）。
 * api は不変条件 1 によりどの site にも束縛されないため、フロントは自分で
 * 対象の site を決め打ちできなくなった（旧 `DEFAULT_SITE` 定数の撤去）。
 *
 * **サイト切り替え UI は持たない。** レジストリの先頭サイトだけを対象にする。
 * 2 サイト以上の運用実績が無い状態で切り替え UI を先回りして作ると、
 * その形が合っているかの判定基準を持たないまま固定することになる
 * （不変条件 11「形を固定する前に、その形を決める判定基準を書く」）。
 * 先頭以外のサイトは黙って見えなくなるが、これは「他サイトの予約が無い」
 * ように見える最悪の壊れ方ではなく、**サイト自体が UI に一切現れない**
 * ので気付ける（詳細は issue #184 のコメント参照）。
 */
export function SiteGate({ children }: { children: ReactNode }) {
  const query = useListSites()
  const sites = unwrap(query.data)

  if (sites === undefined) {
    if (query.status === 'error') {
      return (
        <div className="p-6 text-sm text-destructive">
          サイト一覧の取得に失敗しました{apiErrorMessage(query.error) ? `: ${apiErrorMessage(query.error)}` : ''}
        </div>
      )
    }
    return <div className="p-6 text-sm text-muted-foreground">読み込み中…</div>
  }

  if (sites.length === 0) {
    // config.mirakc/mirakcs レジストリが空の起動はサーバー側で弾かれる
    // （internal/config.validateMirakcRegistry）ので実運用では起きないが、
    // 起きた場合に空配列を site として使ってクラッシュするより説明を出す。
    return (
      <div className="p-6 text-sm text-destructive">
        利用可能なサイトがありません（サーバーの mirakc 設定を確認してください）
      </div>
    )
  }

  return <SiteContext.Provider value={sites[0]}>{children}</SiteContext.Provider>
}
