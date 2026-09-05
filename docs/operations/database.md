> [operations.md](../operations.md) §3「DB 運用」の一部。索引から辿る。

## 3. DB 運用

### 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への対策。故障モードは常に「収束の遅れ」であり、仕事の喪失ではない:

- **録画は影響を受けない（DB 全停止でも走る）**。スケジュールは mirakc 側の `schedules.json` に永続化済みで、録画実行は mirakc が自律的に行う。DB 輻輳の影響は「新規・変更予約の同期遅延」（reconciler のラグ）のみ
- **実行中のエンコード・ingest も影響を受けない**。ジョブは claim / complete 時に小さなトランザクション（SKIP LOCKED）で DB を触るだけで、実行中（ffmpeg・転送）は DB に依存しない
- **ルール評価は UI と同期しない**。ルール編集 API は編集を書いて再評価ジョブを投入するだけで即応答

実装規律:

- **ロール別コネクションプール上限を分ける**。api が全コネクションを食い潰して worker / reconciler が待つ事態を防ぐ。
  ただしプロセスは常に 1 個のコネクションプールしか持たない（`cmd/rokuban/server.go` が起動時に 1 回だけ作り、
  そのプロセスが担う全ロールが共有する）。したがって「ロール別」とは複数プールを作ることではなく、
  **そのプロセスが担う roles 集合と束縛サイト数から、そのプロセスが持つ唯一のプールの `MaxConns` を決める**
  ことを指す（`internal/db.NewPool`）。`db.max_conns` を明示すればそれを使い、未指定ならロールごとの
  budget（1 サイト束縛時: api: 10 / worker: 8 / watcher: 3 / notifier: 3 / streamer: 4。根拠は
  `internal/db.roleConnBudget` の doc コメント）を roles の分だけ合計する。monolith（`--all`）は
  全ロール分の合計になる。**1 プロセスが N サイトを束縛できるため（`--sites`）、watcher は
  束縛サイトごとに advisory lock 用コネクションを 1 本専有し続ける**（site ごとに
  `role.RunSingleton` の goroutine を持つ。`cmd/rokuban/server.go` の watcher ループ）。
  2 サイト目以降は budget にも自動で上乗せされる（`internal/db.perSiteConnBudget`。
  worker も ingest の rel_path advisory lock ぶんを同様に上乗せする）。
  `db.max_conns` を明示指定する場合の fail-fast（`internal/db.minRequiredConns`）も束縛サイト数を
  見る --- watcher の専有分（1 サイトあたり 1 本）の合計 + 他の仕事のための余地 1 本を下回ると
  起動時エラーになる。**worker の fail-fast はサイト数を見ない**（固定 1 本のまま）:
  River の LISTEN 用の 1 本だけが「プロセスの生存期間中ずっと専有される」資源で、ingest の
  rel_path advisory lock は転送中だけの一時専有（転送が終われば解放される）なので、無症状
  デッドロックの検査（“二度と戻ってこないコネクション”）には含めていない。ソフトな budget
  側の上乗せ（上記）が実運用の目安である。
- **API 系クエリに `statement_timeout`** を設定する。クエリ単位の context timeout だと「付け忘れた 1 本」が
  必ず生まれる。接続の `RuntimeParams`（起動パケットの session default）で一括適用する
  （`db.api_statement_timeout`、未指定なら 30s）。**api ロールを含むプロセスのプール全体に適用される**。
  monolith で api と worker/watcher を同居させると worker 側のクエリにも同じ上限がかかる。
  世帯スケールの通常クエリを十分に上回る値（既定 30s）にしてあるので実害は想定していないが、
  ロールを分離すれば worker 単独プロセスには一切適用されなくなる。`statement_timeout` は行ロック待ちにも
  効く（Postgres は「クエリの実行時間」と「ロック待ちの時間」を区別しない）ため、monolith で
  `record_sweep` 等が別トランザクションの行ロックを長く待つ状況では statement_timeout で中断されうる。
  中断されても River が再試行するので致命的ではないが、意図しない再試行が増える兆候として覚えておく

### pooler 越しに置けるのは api ロールと streamer ロールだけ

`db.pooler_compat: true` は PgBouncer / Neon pooler の **transaction pooling** 越しの接続を想定したモード。
pgx の prepared statement キャッシュを無効化する（`DefaultQueryExecMode` を `QueryExecModeExec` にする）。

これはデプロイの契約であり、**pooler を通せるのは api ロールと streamer ロールだけ**である。
worker は River の内部機構が LISTEN を使う。`notifier.New` で作る 1 個の Listener を leadership の
elector と job-available 通知が共有している。elector と notifier がそれぞれ別に 1 本ずつではない
（`river@v0.47.0 client.go` の `notifier.New` と `leadership.NewElector` で確認済み）。watcher は advisory lock によるリーダー選出を使う。
notifier はブラウザへの SSE 配送のために LISTEN を使う。いずれもセッション状態に依存する。
transaction pooling で物理コネクションが要求ごとに入れ替わると構造的に壊れる（[data.md](../data.md) §2 / §3）。
`internal/db.NewPool` は `pooler_compat: true` と worker/watcher/notifier のいずれかのロールの
組み合わせを起動時エラーにする（fail-fast）。**streamer は LISTEN も advisory lock も長期状態も
使わない**。pooler と組み合わせてよい（`internal/streamer` で確認済み。バイト転送も
X-Accel-Redirect か Go のファイル配信で、DB 接続を保持し続けない）。

`db.api_statement_timeout` は接続の起動パケット（`RuntimeParams`）で渡す。PgBouncer の
`ignore_startup_parameters` 設定次第では接続拒否、または黙って無視される可能性がある。
`pool.Ping` が失敗すれば起動時に大きく落ちるので気付ける。api + pooler を組み合わせて
運用する場合は `ignore_startup_parameters` に `statement_timeout` を含めない設定にしておく。

### EPG churn / autovacuum

EPG テーブルは 1 日に何度も大量 upsert されるため、遅くなるとしたら検索ではなく書き込みと autovacuum の追従。対策:

- バッチ upsert
- GIN fastupdate
- **テーブル別 autovacuum チューニング**（EPG テーブルに対して `autovacuum_vacuum_scale_factor` を小さく設定する等）

EPG テーブルは 8 日分 + 猶予のローリングウィンドウであり、永遠に太らない。検索性能自体は世帯スケール（数万〜20 万行）では問題にならない。

### Postgres datadir とエンコード scratch の分離

monolith モードでは Postgres のデータディレクトリとエンコードの scratch が同じディスクに載りやすい。実際に競合しやすいのはロックよりディスク I/O であり、**両者を同じディスクに置かない構成を推奨する**。

### バックアップ

保護対象は「ルール・録画履歴・media_assets・ドロップ統計・tombstone・手動オーバーライド」のみ（数 MB）。EPG プロジェクションは mirakc から再構築可能、ジョブキューは一時的。

- **catalog エクスポート**: worker の定期ジョブが、コアデータを JSON でメディアストレージ自身の `catalog/` 配下に書き出す（日次 + 世代保持）。メディアが生き残る障害では catalog も一緒に生き残る。pg_dump に依存しない（公式イメージに postgres クライアントを同梱しなくてよい）アプリレベルのエクスポート
  - **1 世代 = 1 ディレクトリ（`catalog/catalog-<時刻>/`）で、`manifest.json` を最後に書き終えたものだけが完成世代**。判定基準の詳細は [storage.md](../storage.md) §8
  - **健全性の確認は `rokuban catalog verify`**（DB に触らない。完成世代が 1 つも無ければ非ゼロ終了する）。何世代が完成しているか / rescue が使う世代 / 落ちた世代とその理由を出す。**`manifest.json` の存在を目視するだけでは確認にならない**（存在は必要条件でしかなく、完成判定はサイズと sha256 の照合まで含む）
  - **`rokuban rescue` を健全性の確認に使わない。破壊的な操作である**。rescue は検証の後に必ず DB へ書き、live DB を catalog スナップショットで**上書きする**。`recordings.status` / `deleted_at` / `purged_at`、`media_assets.state` / `deleted_at` を catalog の値で上書きし、id シーケンスを巻き戻す。健全な DB に対して実行すると、catalog を書き出した時点まで状態が巻き戻る（削除済み asset の復活を含む）。使うのは DB を失った後だけ
- **pg_dump（推奨・非必須）**: フル忠実度が欲しい場合の日次 pg_dump 構成例をドキュメントに記載する
- 世帯スケールでは catalog + 任意の pg_dump で十分。WAL アーカイビングは過剰

### 経緯と失敗事例

- 輻輳時の隔離の実装規律（ロール別プール上限・`statement_timeout` の一括適用）は issue #90 で実装した。
