# データ層: PostgreSQL 一本化（索引）

ステートフルな基盤は **PostgreSQL ただ一つ**。Redis / NATS / 専用ジョブキュー等のミドルウェアは置かない。キュー・通知・排他・検索をすべて Postgres に背負わせるブローカーレス設計。

**本文は `docs/data/` に分割してある。節番号は分割前のまま**なので、コードコメントや他 doc の「data.md §6.5」等はこの表で該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| §1 §2 §3 §7 | 方針: PostgreSQL 一本 / **ジョブキュー River**（定期実行の契機 / **ジョブ一意性の注意** / Redis を採用しない理由）/ LISTEN/NOTIFY と notifier ロール / DB 輻輳時の隔離 | [data/jobs.md](data/jobs.md) |
| §4 §6 | スキーマ設計: desired / observed の分離 / **EPG プロジェクション**（UI 完全 / ローリングウィンドウ / 非正規化スナップショット / サービスロゴ） | [data/projections.md](data/projections.md) |
| §5 | **検索とルール評価の統一**（POSIX ARE / pg_trgm / 全角半角正規化 / 録画検索と rulequery の境界） | [data/search.md](data/search.md) |
| §6.5 | **チューナー射影と容量超過の判定**（Hall 条件 / twin vertices / 累積和 / 下界に限る原則） | [data/capacity.md](data/capacity.md) |

読む順の目安:

- **ジョブ・定期実行・NOTIFY を触る** → §2 §3（jobs.md。River の `UniqueOpts` の注意もここ）
- **検索 / ルール評価 / 録画一覧の絞り込みを触る** → §5
- **EPG 射影・スキーマの分離原則を触る** → §4 §6
- **容量判定・チューナー射影を触る** → §6.5（前提として §6 の非正規化スナップショット）
- **DB 輻輳・プール設定を触る** → §7 と [operations.md](operations.md) §3

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [schema.md](schema.md)（スキーマ DDL）/ [recording.md](recording.md)（録画エンジン）/ [storage.md](storage.md)（メディアストレージ）/ [operations.md](operations.md)（運用）
