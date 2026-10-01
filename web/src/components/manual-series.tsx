/** ManualSeriesBadge は分類ルールで指定された棚（「手動」）の札。`/series` とハブで共有する。 */
export function ManualSeriesBadge() {
  return (
    <span className="shrink-0 rounded border border-border px-1 text-xs text-muted-foreground">
      手動
    </span>
  )
}

/**
 * LabelRulesUnavailableNote は分類ルールの取得に失敗したとき「手動」の判定が
 * 欠けていることを伝える。黙って札を消すと、手動の棚が自動の棚に見える。
 */
export function LabelRulesUnavailableNote() {
  return (
    <p role="status" className="text-xs text-muted-foreground">
      分類ルールを取得できないため、「手動」の表示を省略しています
    </p>
  )
}
