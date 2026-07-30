/**
 * DEFAULT_SITE は config.mirakc.site の既定値と同じ（サーバー側の唯一の権威）。
 *
 * 現状 Rokuban は単一サイト構成の UI しか持たない（複数サイト選択 UI は M4）。
 * サーバーが `mirakc.site` を変更した場合はこの定数も合わせて変える必要がある
 * --- サイト一覧を返す API が無いため、フロントは決め打ちで送るしかない
 * （変更前は `db.DefaultSite` のハードコードがサーバー側にあった、というのと
 * 同じ制約がここに移っただけで、新しい制約ではない）。
 */
export const DEFAULT_SITE = 'default'
