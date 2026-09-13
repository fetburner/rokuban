> [operations.md](../operations.md) §3「DB 運用」の一部。索引から辿る。

## 3. DB 運用

### 輻輳時の隔離

「ユーザー操作で DB が詰まったら録画やエンコードに影響しないか」という懸念への対策。DB 輻輳から分離できる処理はあるが、故障モードを常に「収束の遅れ」とは一般化しない。運用上の保証は次の条件付きである:

- **録画は、mirakc に番組終了前まで同期済みの予約に限って DB 停止から分離される**。スケジュールは mirakc 側の `schedules.json` に永続化済みで、録画実行は mirakc が自律的に行う。ただし mirakc 自身、録画バッファ、チューナーが動作していることが条件であり、新規・変更予約は reconciler が期限内に同期できなければ録画されない
- **実行中の ingest は、転送中のバイト I/O だけを見れば DB の外側にあるが、ジョブ全体は DB に依存する**。開始時の `record_sync` 参照、転送中を通して保持する job-id advisory lock（生存確認用で転送先の排他ではない）、進捗の書き込み、公開点である `media_assets` コミットが必要である。DB 障害で接続やコミットを失えば、録画バッファに record が残り、再試行できる範囲では収束する。job lock 用接続が転送中に死んでも転送は止まらないが、`record_sweep` が生きた転送をプロセス死と誤認して二重 pull しうる。決着は DB の一意 INSERT が付け、canonical file は壊れない（詳細は [ingest](../recording/ingest.md) §5.3）
- **実行中の encode は ffmpeg のバイト処理だけを見れば DB の外側にあり、公開も `media_assets` コミットで決まるが、プロセス死の回収は ingest より弱い**。encode は `Timeout() = -1` のため River の stuck-job rescue（JobRescuer）対象外で、ingest の `record_sweep` のような代替回収も無い。ロール分割で常駐の River クライアントが無い構成では `running` のまま残る（詳細は [k8s 運用](k8s.md)）
- **長時間の滞留はポリシーを失うことがある**。ingest が `epg.retention_grace` を跨ぐと、予約から encode policy を解決できず既定値で凍結され、作成時点で予約も意図も無ければ `source` は `unattributed` になる。原本の保持・エンコードの扱い、回線断を含む滞留の測り方は [ストレージ運用](storage.md) §4 と [ストレージ](../storage.md) §6 を参照する
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
  worker も ingest の job advisory lock ぶんを同様に上乗せする）。
  `db.max_conns` を明示指定する場合の fail-fast（`internal/db.minRequiredConns`）も束縛サイト数を
  見る --- watcher の専有分（1 サイトあたり 1 本）の合計 + 他の仕事のための余地 1 本を下回ると
  起動時エラーになる。**worker の fail-fast はサイト数を見ない**（固定 1 本のまま）:
  River の LISTEN 用の 1 本だけが「プロセスの生存期間中ずっと専有される」資源で、
  ingest の job advisory lock は転送中だけの一時専有（転送が終われば解放される）なので、無症状
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

### transaction pooling は通さない

Rokuban は transaction pooling を通さない。
worker は River の LISTEN を使い、`notifier.New` の 1 個の Listener を elector と job-available 通知で共有する（`river@v0.47.0` で確認済み）。
watcher は advisory lock、notifier は SSE 用の LISTEN を使うため、transaction pooling で接続が要求ごとに入れ替わると壊れる（[data.md](../data.md) §2 / §3）。
将来ハイブリッド構成を実装するときは、必要な pooler 対応の形をその PR で改めて決める。

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
