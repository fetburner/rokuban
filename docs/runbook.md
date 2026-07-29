# シャドー運用 runbook（索引）

既存の mirakc（EPGStation と共有）に Rokuban をぶら下げ、実放送で確認する手順。

**本文は `docs/runbook/` に分割してある。** やることに対応するファイルだけ開く。

| ファイル | 内容 |
|---|---|
| [runbook/setup.md](runbook/setup.md) | 前提 / 起動 / 定期ジョブの周期とヒント |
| [runbook/m1.md](runbook/m1.md) | **M1: 実放送を 1 本録る** —— 手動予約 → schedule → 録画 → ingest → 再生。出口基準チェックリスト（M1） |
| [runbook/m2.md](runbook/m2.md) | **M2: ルールで録る** —— 手順 1〜11（検索 / ルール作成 / ruler / 番組単位の除外・上書き / 重複排除 / 重なり / 開始遅延 / ドロップ統計 / 容量超過 / グリッド）とサーキットブレーカー |
| [runbook/m2-checklist.md](runbook/m2-checklist.md) | M2 で見るメトリクス / **沈黙は保証ではない** / 既知の未解決事項 / 出口基準チェックリスト（M2） |
| [runbook/shadow.md](runbook/shadow.md) | EPGStation との並走 —— チューナー競合、二重録画の回避、`shadow-diff` の読み方 |
| [runbook/troubleshooting.md](runbook/troubleshooting.md) | 詰まったとき（症状別） |
| [runbook/testing.md](runbook/testing.md) | 開発時のテスト |

| 段 | 確認すること |
|---|---|
| [M1](runbook/m1.md) | 実放送を 1 本、**手動予約で録って再生できる** |
| [M2](runbook/m2.md) | **ルールで録れる**。除外・上書きが生き残り、「なぜ録られていないか」を運用者が読める |

エンコードと保持ポリシーは入っていない（[移行計画](https://github.com/fetburner/rokuban/issues/6) の M3 以降）。

M2 の機能のうち **UI があるのは番組表・検索・予約・録画で、ルールの編集画面だけが無い**。ルート（`web/src/routes.tsx`）は `/`（番組）/ `/search` / `/reservations` / `/reservations/$reservationId` / `/recordings` の 5 つ。ルールは API で作る。

出口基準そのものの検証（1〜2 週間の並走）は [issue #52](https://github.com/fetburner/rokuban/issues/52)。
