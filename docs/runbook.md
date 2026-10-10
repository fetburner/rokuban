# シャドー運用 runbook（索引）

既存の mirakc（EPGStation と共有）に Rokuban をぶら下げ、実放送で確認する手順。

**本文は `docs/runbook/` に分割してある。** やることに対応するファイルだけ開く。

| ファイル | 内容 |
|---|---|
| [runbook/setup.md](runbook/setup.md) | 前提 / 起動 / 設定の 2 段構え / 録画の保存先 / 定期ジョブの周期とヒント |
| [runbook/troubleshooting.md](runbook/troubleshooting.md) | 詰まったとき（症状別） |
| [runbook/shadow.md](runbook/shadow.md) | EPGStation との並走 —— チューナー競合と二重録画の回避 |
| [runbook/live.md](runbook/live.md) | ライブ視聴 —— idle GC・実再生の確認（実 mirakc 要）とブラウザ側配線の確認（`web/e2e/live.mjs`。mirakc 不要） |
| [runbook/nginx.md](runbook/nginx.md) | nginx 前段 —— TLS・Basic 認証・SSE・SPA・X-Accel-Redirect の実機確認 |
| [runbook/k8s.md](runbook/k8s.md) | k8s —— kind で中央 1 式を上げて api に到達 / `/readyz` が DB 断で 503 になることの確認 / ロール分割デプロイの受け入れ判定ハーネス（kind + KEDA） |
| [runbook/managed-postgres.md](runbook/managed-postgres.md) | managed PostgreSQL で `btree_gist` の作成が通るかの確認手順 |
| [runbook/testing.md](runbook/testing.md) | 開発時のテスト |
| [runbook/cutover.md](runbook/cutover.md) | 主運用の切替 —— 出口基準チェックリスト / エンコード・ごみ箱・ブレーカー・rescue 練習の手順 / 二重録画を避ける切替順 / ロールバック |
| [runbook/import-epgstation.md](runbook/import-epgstation.md) | EPGStation からの移行（`rokuban import epgstation`）—— ルール取り込みの手順とライブラリ JSON の書き出し方 |

UI の画面構成とルートは [frontend.md](frontend.md) を参照。
