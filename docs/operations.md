# 運用（索引）

運用者向けに、監視・アラート、DB・ストレージ、k8s の運用判断をまとめる。

| 節 | 内容 | ファイル |
|---|---|---|
| §1 | 監視メトリクス: `/metrics` エンドポイント / 実装済みメトリクス一覧 / 録画品質 / ジョブ化されたループの監視（`rokuban enqueue`）/ ruler / ingest / reconcile / 開始遅延検出器 / 警告が出ないことは保証ではない | [operations/monitoring.md](operations/monitoring.md) |
| §2 | アラート設計: scrambled / エッジディスク残量 / 大量削除サーキットブレーカー / 開始遅延 | [operations/alerts.md](operations/alerts.md) |
| §3 | DB 運用: 輻輳時の隔離（プール上限 / statement_timeout）/ セッション依存機構 / autovacuum / datadir と scratch の分離 / バックアップ | [operations/database.md](operations/database.md) |
| §4 | ストレージ運用: 録画バッファのサイジング / アーカイブの速度要件 / disaster recovery（catalog + rescue） | [operations/storage.md](operations/storage.md) |
| §5 | k8s 運用: ロールとキュー購読 / キューの置き場所 / KEDA ScaledJob / CronJob / リーダー選出 / kustomize / healthz と readyz。streamer のスケール（ライブ視聴の配置・replicas・scratch の容量）は別ファイル | [operations/k8s.md](operations/k8s.md)（streamer のスケール以外）/ [operations/k8s-streamer.md](operations/k8s-streamer.md)（streamer のスケール） |

読む順の目安は次の通り。

| やること | 読む節 |
|---|---|
| 監視・アラートを組む | §1 と §2 |
| DB / サーバーレス構成を触る | §3 |
| ディスクのサイジング・復旧 | §4 |
| ロール分割デプロイ・KEDA・streamer / ライブ視聴のスケール | §5（`worker.queues` は [configuration.md](configuration.md) と対で読む） |

本文は `docs/operations/` に分割してある。**節番号は分割前のまま**なので、コードコメントの「operations.md §3」等はこの表で該当ファイルを引ける。

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ・ロール分類）/ [data.md](data.md)（River・NOTIFY）/ [configuration.md](configuration.md)（設定）/ [storage.md](storage.md)（ストレージ契約）/ [runbook.md](runbook.md)（導入・移行・運用手順）
