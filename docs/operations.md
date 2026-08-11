# 運用（索引）

Rokuban の監視・アラート・DB 運用・ストレージ運用・k8s 運用に関する設計指針をまとめる。

**本文は `docs/operations/` に分割してある。節番号は分割前のまま**なので、コードコメントの「operations.md §3」等はこの表で該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| §1 | **監視メトリクス**: `/metrics` エンドポイント / 実装済みメトリクス一覧 / 録画品質 / ジョブ化されたループの監視（`rokuban enqueue`）/ ruler / ingest / reconcile / 開始遅延検出器 / **沈黙は保証ではない** | [operations/monitoring.md](operations/monitoring.md) |
| §2 | **アラート設計**: scrambled / エッジディスク残量 / 大量削除サーキットブレーカー / 開始遅延 | [operations/alerts.md](operations/alerts.md) |
| §3 | **DB 運用**: 輻輳時の隔離（プール上限 / statement_timeout）/ pooler 越しに置けるロール / autovacuum / datadir と scratch の分離 / バックアップ | [operations/database.md](operations/database.md) |
| §4 | **ストレージ運用**: 録画バッファのサイジング / アーカイブの速度要件 / disaster recovery（catalog + rescue） | [operations/storage.md](operations/storage.md) |
| §5 | **k8s 運用**: ロールとキュー購読の関係 / キューの site 修飾と置き場所 / **streamer のスケール** / KEDA ScaledJob / シングルトンロールのリーダー選出 / healthz | [operations/k8s.md](operations/k8s.md) |

読む順の目安:

- **監視・アラートを組む** → §1 と §2
- **DB / pooler / サーバーレス構成を触る** → §3
- **ディスクのサイジング・復旧** → §4
- **ロール分割デプロイ・KEDA・streamer / ライブ視聴のスケール** → §5（`worker.queues` は [configuration.md](configuration.md) と対で読む）

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ・ロール分類）/ [data.md](data.md)（River・NOTIFY）/ [configuration.md](configuration.md)（設定）/ [storage.md](storage.md)（ストレージ契約）/ [runbook.md](runbook.md)（手動での動作確認手順）
